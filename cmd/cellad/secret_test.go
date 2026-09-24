// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/internal/egressd"
)

// theCanary is the value this tier follows. It is a shape nothing else in the
// tree produces, so a single occurrence anywhere but the upstream's own view
// of the request is a leak and not a coincidence.
const theCanary = "sk-cella-canary-0000-never-in-a-sandbox"

// TestSecretValuesNeverEnterASandbox is the whole of spec 018's promise over
// one running control plane, one running gateway and one running sandbox: the
// workload holds a placeholder, the value reaches the destination its owner
// named, and the value is in no environment, no file, no event, no record and
// no log line of either process.
func TestSecretValuesNeverEnterASandbox(t *testing.T) {
	// The upstream records what it was sent, which is the one place the
	// value is allowed to appear.
	var carried atomic.Value
	carried.Store("")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		carried.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "the upstream answered "+r.Host+r.URL.Path)
	}))
	defer upstream.Close()
	trust := x509.NewCertPool()
	trust.AddCert(upstream.Certificate())

	sink := newStubSink(t, "only-secret")
	proxyLn, reverseLn := doors(t)
	proxyAddr, reverseAddr := proxyLn.Addr().String(), reverseLn.Addr().String()
	plane := startPlaneWith(t, proxyAddr, reverseAddr, map[string]string{
		// A control plane that stores a value needs the key that seals it.
		"CELLA_SECRET_KEY":    base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"CELLA_EVENTS_URL":    sink.server.URL,
		"CELLA_EVENTS_SECRET": "only-secret",
	})

	var gatewayLog syncBuffer
	ready := make(chan struct{})
	startGateway(t, plane, egressd.Options{
		ProxyListener: proxyLn, ReverseListener: reverseLn,
		UpstreamCAPEM: certificatePEM(t, upstream),
		Dial:          dialTo(upstream.Listener.Addr().String()),
		Log:           slog.New(slog.NewTextHandler(&gatewayLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Ready:         func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	// The owner writes the secret. The answer carries no value, which is the
	// first place one could leak.
	created := plane.applySecret(t, "vendor", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret",`+
		`"metadata":{"name":"vendor"},"spec":{"scope":{"hosts":["upstream.example.com"]},"value":"`+theCanary+`"}}`)
	if strings.Contains(created, theCanary) {
		t.Fatalf("the create answered with the value: %s", created)
	}
	if strings.Contains(plane.get(t, "/v1/secrets/vendor"), theCanary) {
		t.Fatal("a read of the secret answered with the value")
	}
	if strings.Contains(plane.get(t, "/v1/secrets"), theCanary) {
		t.Fatal("the list answered with the value")
	}

	sandbox := plane.createMounting(t, "vendor", "VENDOR_TOKEN")

	t.Run("theSandboxHoldsAPlaceholderAndNotTheValue", func(t *testing.T) {
		token := strings.TrimSpace(plane.exec(t, sandbox, "printenv VENDOR_TOKEN"))
		if !strings.HasPrefix(token, "cph_") {
			t.Fatalf("the sandbox's own key holds %q, want a placeholder", token)
		}
		environment := plane.exec(t, sandbox, "env")
		if strings.Contains(environment, theCanary) {
			t.Fatal("the value is in the sandbox's environment")
		}
		// Nothing the control plane put inside the sandbox holds it
		// either: its own workspace, and the directory the boundary's
		// authority and the rest of its projected files live in.
		files := plane.exec(t, sandbox,
			"grep -rl '"+theCanary+"' /workspace /run/cella 2>/dev/null | head -5; true")
		if strings.TrimSpace(files) != "" {
			t.Fatalf("the value is in a file the sandbox can read: %s", files)
		}
	})

	t.Run("theValueReachesTheHostItsOwnerNamed", func(t *testing.T) {
		gatewayURL := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_URL"))
		credential := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_CREDENTIAL"))
		token := strings.TrimSpace(plane.exec(t, sandbox, "printenv VENDOR_TOKEN"))
		if gatewayURL == "" || credential == "" {
			t.Fatalf("the sandbox was not pointed at a gateway: %q %q", gatewayURL, credential)
		}
		body := reverseGet(t, gatewayURL+"/upstream.example.com/v1/things", credential, "Bearer "+token)
		if !strings.Contains(body, "the upstream answered upstream.example.com/v1/things") {
			t.Fatalf("the upstream answered %q", body)
		}
		if got := carried.Load().(string); got != "Bearer "+theCanary {
			t.Fatalf("the upstream saw %q, want the value", got)
		}
	})

	t.Run("thePlaceholderLeavesVerbatimTowardEveryOtherHost", func(t *testing.T) {
		gatewayURL := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_URL"))
		credential := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_CREDENTIAL"))
		token := strings.TrimSpace(plane.exec(t, sandbox, "printenv VENDOR_TOKEN"))
		// The boundary admits the secret's own host and nothing else, so a
		// request elsewhere never leaves; the value is the point, and the
		// refusal is what keeps it in.
		body := reverseStatus(t, gatewayURL+"/elsewhere.example.com/v1/things", credential, "Bearer "+token)
		if body != http.StatusForbidden {
			t.Fatalf("a request off the allow list answered %d", body)
		}
		if got := carried.Load().(string); got != "Bearer "+theCanary {
			t.Fatalf("the upstream saw %q after a refused request", got)
		}
	})

	t.Run("theValueIsInNoRecordAndNoAnswer", func(t *testing.T) {
		records := plane.records(t, sandbox, 1)
		encoded, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(theCanary)) || bytes.Contains(encoded, []byte("cph_")) {
			t.Fatalf("a connection record carries a value or a placeholder: %s", encoded)
		}
		if strings.Contains(plane.get(t, "/v1/sandboxes/"+sandbox), theCanary) {
			t.Fatal("the sandbox's own answer carries the value")
		}
	})

	t.Run("theValueIsInNoEvent", func(t *testing.T) {
		// The two records this run must produce: the secret's own create and
		// the sandbox that mounts it. Waiting for both is what makes the
		// check below an assertion rather than a race the sink can win by
		// delivering nothing.
		want := map[string]bool{"secret.created": false, "sandbox.created": false}
		deadline := time.Now().Add(30 * time.Second)
		for {
			for _, record := range sink.records() {
				if bytes.Contains(record.Raw, []byte(theCanary)) || bytes.Contains(record.Raw, []byte("cph_")) {
					t.Fatalf("an event carries a value or a placeholder: %s", record.Raw)
				}
				if _, named := want[record.Type]; named {
					want[record.Type] = true
				}
				if record.Type == "sandbox.created" && !bytes.Contains(record.Raw, []byte(`"mounts":["vendor"]`)) {
					t.Fatalf("the created record does not name the mounted secret: %s", record.Raw)
				}
			}
			if want["secret.created"] && want["sandbox.created"] {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the sink received %v", want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("theValueIsInNoFileAndNoLog", func(t *testing.T) {
		if found := grepTree(t, plane.dataDir, theCanary); len(found) > 0 {
			t.Fatalf("the value is on disk under the control plane's own directory: %v", found)
		}
		for name, written := range map[string]string{
			"the control plane's output": plane.out.String(),
			"the control plane's errors": plane.errOut.String(),
			"the gateway's log":          gatewayLog.String(),
		} {
			if strings.Contains(written, theCanary) {
				t.Fatalf("%s carries the value", name)
			}
		}
		// The log did run: a tier that printed nothing would pass the check
		// above without proving anything.
		if plane.out.String() == "" || gatewayLog.String() == "" {
			t.Fatal("neither process logged, so the check above proves nothing")
		}
	})
}

// grepTree is every file under root whose bytes hold the needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			// A file the walk cannot open is a file this check did not read,
			// which is a gap in the check and not a pass.
			return fmt.Errorf("walking %s: %w", path, err)
		case d.IsDir():
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if bytes.Contains(body, []byte(needle)) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	return found
}

// applySecret writes one Secret through the route an owner uses.
func (p *plane) applySecret(t *testing.T, name, body string) string {
	t.Helper()
	status, answer := p.do(t, http.MethodPut, "/v1/secrets/"+name, strings.NewReader(body))
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("PUT /v1/secrets/%s = %d %s", name, status, answer)
	}
	return answer
}

// createMounting applies a sandbox that mounts one secret and answers its id.
// The boundary names no host of its own: the mounted secret's scope is what
// the sandbox may reach, which is the join the compiler makes.
func (p *plane) createMounting(t *testing.T, secret, env string) string {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"spec":{"command":["/bin/sh","-c","sleep 60"],` +
		`"secrets":[{"name":"` + secret + `","env":"` + env + `"}]}}`
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes?wait=1", strings.NewReader(body))
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
	}
	var obj struct {
		Status struct {
			ID      string `json:"id"`
			Secrets struct {
				Mounted       []string `json:"mounted"`
				NotInjectable []string `json:"notInjectable"`
			} `json:"secrets"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Status.Secrets.Mounted) != 1 || len(obj.Status.Secrets.NotInjectable) != 0 {
		t.Fatalf("status.secrets = %+v", obj.Status.Secrets)
	}
	return obj.Status.ID
}

// reverseGet is one request at the gateway's reverse door, as a runtime that
// ignores proxy variables makes one.
func reverseGet(t *testing.T, target, credential, authorization string) string {
	t.Helper()
	resp := reverseDo(t, target, credential, authorization)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// reverseStatus is reverseGet for a request whose refusal is the point.
func reverseStatus(t *testing.T, target, credential, authorization string) int {
	t.Helper()
	resp := reverseDo(t, target, credential, authorization)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func reverseDo(t *testing.T, target, credential, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cella-Egress-Credential", credential)
	req.Header.Set("Authorization", authorization)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	return resp
}
