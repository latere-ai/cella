// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package kind_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// defaultUpstream is the echo server of the stack's upstream.yaml as a
// sandbox of the stack names it: a Service in the stack's namespace, on the
// port its Service publishes. CELLA_TEST_UPSTREAM names another for a stack
// somebody else brought up.
const defaultUpstream = "cella-upstream.cella.svc:80"

// deniedHost is a host no allow list of these tests names. The gateway
// refuses it before any dial, so it needs no address.
const deniedHost = "denied.example.com"

// connectScript opens one CONNECT through the sandbox's own proxy door, read
// off HTTPS_PROXY as any client that honors it reads it, to the destination
// in $1, and sends the line in $2 through the tunnel once it is open. It
// prints what came back: the gateway's answer, and the line again where the
// destination echoed it.
const connectScript = `door=${HTTPS_PROXY#*://}
auth=${door%@*}
door=${door##*@}
door=${door%/}
basic=$(printf %s "$auth" | base64 | tr -d '\n')
{ printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n' "$1" "$1" "$basic"; sleep 2; printf '%s\n' "$2"; sleep 2; } |
  nc -w 5 "${door%:*}" "${door##*:}" 2>/dev/null | tr -d '\r'
`

// askScript opens one CONNECT through the sandbox's own proxy door to the
// destination in $1, sends nothing through it, and prints the gateway's
// answer. Closing the write side is what tells the gateway the caller has
// gone, so a destination the gateway cannot reach is answered 502 at once
// rather than when the dial gives up.
const askScript = `door=${HTTPS_PROXY#*://}
auth=${door%@*}
door=${door##*@}
door=${door%/}
basic=$(printf %s "$auth" | base64 | tr -d '\n')
{ printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n' "$1" "$1" "$basic"; sleep 3; } |
  nc -w 10 "${door%:*}" "${door##*:}" 2>/dev/null | head -n 1 | tr -d '\r'
`

// directScript sends the line in $3 straight to $1:$2, around every door,
// and prints how many bytes came back.
const directScript = `{ printf '%s\n' "$3"; sleep 2; } | nc -w 5 "$1" "$2" 2>/dev/null | wc -c | tr -d ' '`

// echoCommand is a sandbox serving port 8080 as an echo, which is what the
// port listing of dial_test.go waits on.
var echoCommand = `["nc","-lk","-p","` + strconv.Itoa(dialPort) + `","-e","cat"]`

// TestClusterEgressBoundary is the boundary of spec 018 on the cluster the
// stack runs: a sandbox whose allow list names the upstream reports it
// enforced, reaches the upstream through its own proxy door with a line
// carried both ways, is refused a host off the list, reaches nothing around
// the gateway, and finds both connections in its records.
func TestClusterEgressBoundary(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}
	awaitLive(t, client, url)
	upstream := upstreamOf(t)
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		t.Fatalf("the upstream %q is no host:port: %v", upstream, err)
	}

	const name = "tier-egress"
	createSandbox(t, client, url, token, name, `{"image":"docker.io/library/alpine:3.22","command":["sleep","600"],
		"network":{"egress":{"allowedHosts":["`+host+`"]}}}`)
	awaitRunning(t, client, url, token, name)
	_, read := call(t, client, http.MethodGet, url+"/v1/sandboxes/"+name, token, nil)
	if !enforced(read) {
		t.Fatalf("the sandbox does not report its boundary enforced: %s", read)
	}

	through := execIn(t, client, url, token, name, "/bin/sh", "-c", connectScript, "connect", upstream, "through the gateway")
	if !strings.Contains(through, "HTTP/1.1 200") || !strings.Contains(through, "through the gateway") {
		t.Fatalf("the CONNECT to %s through the proxy door answered %q, want 200 and the line echoed", upstream, through)
	}
	refused := execIn(t, client, url, token, name, "/bin/sh", "-c", connectScript, "connect", deniedHost+":443", "refused")
	if !strings.HasPrefix(refused, "HTTP/1.1 403") {
		t.Fatalf("the CONNECT to %s answered %q, want 403", deniedHost, refused)
	}
	if got := execIn(t, client, url, token, name, "/bin/sh", "-c", directScript, "direct", host, port, "around the gateway"); strings.TrimSpace(got) != "0" {
		t.Fatalf("a line sent straight to %s brought back %s bytes, want none", upstream, got)
	}

	awaitEgressRecord(t, client, url, token, name, host, "passthrough")
	awaitEgressRecord(t, client, url, token, name, deniedHost, "denied")
}

// TestClusterNoLateralMovement: a sandbox reaches no other sandbox's Pod,
// neither straight from its own network nor through the gateway. The
// gateway's own egress is open on this stack and the prober's boundary is
// `open`, so the gateway admits the destination and dials it, which its 502
// and the record of the attempt show: what refuses that connection is the
// other sandbox's ingress rule alone.
func TestClusterNoLateralMovement(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}
	awaitLive(t, client, url)

	const target, prober = "tier-lateral-target", "tier-lateral-prober"
	createSandbox(t, client, url, token, target, `{"image":"docker.io/library/alpine:3.22","command":`+echoCommand+`,
		"network":{"ports":[{"name":"echo","port":`+strconv.Itoa(dialPort)+`}]}}`)
	createSandbox(t, client, url, token, prober, `{"image":"docker.io/library/alpine:3.22","command":["sleep","600"]}`)
	awaitRunning(t, client, url, token, target)
	awaitRunning(t, client, url, token, prober)
	awaitListening(t, client, url, token, target)

	address := strings.Fields(execIn(t, client, url, token, target, "hostname", "-i"))
	if len(address) == 0 || net.ParseIP(address[0]) == nil {
		t.Fatalf("the target reports the address %q", address)
	}
	ip := address[0]
	// The target serves: from inside itself the line comes back.
	if got := execIn(t, client, url, token, target, "/bin/sh", "-c", directScript, "self", "127.0.0.1", strconv.Itoa(dialPort), "inside"); strings.TrimSpace(got) == "0" {
		t.Fatal("the target does not echo on its own loopback, so the refusals below prove nothing")
	}
	if got := execIn(t, client, url, token, prober, "/bin/sh", "-c", directScript, "direct", ip, strconv.Itoa(dialPort), "lateral"); strings.TrimSpace(got) != "0" {
		t.Fatalf("a sandbox reached another sandbox's Pod at %s and brought back %s bytes", ip, got)
	}
	through := execIn(t, client, url, token, prober, "/bin/sh", "-c", askScript, "ask", net.JoinHostPort(ip, strconv.Itoa(dialPort)))
	if !strings.HasPrefix(through, "HTTP/1.1 502") {
		t.Fatalf("the gateway answered %q for another sandbox's Pod at %s, want 502: admitted and not reached", through, ip)
	}
	awaitEgressRecord(t, client, url, token, prober, ip, "passthrough")
}

// TestClusterMeshReachability: a parent and the child spawned with its token
// are one mesh, and each reaches the other by name on the port the other
// serves, under the rule that confines both to the gateway, DNS and their
// peers; a sandbox outside the mesh reaches neither.
func TestClusterMeshReachability(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}
	awaitLive(t, client, url)

	const parent, child, outsider = "tier-mesh-parent", "tier-mesh-child", "tier-mesh-outsider"
	serve := `"image":"docker.io/library/alpine:3.22","command":` + echoCommand + `,
		"network":{"ports":[{"name":"echo","port":` + strconv.Itoa(dialPort) + `}]}`
	createSandbox(t, client, url, token, parent, `{`+serve+`,"mesh":{"enabled":true,"spawn":{"budget":1,"depth":1}}}`)
	createSandbox(t, client, url, token, outsider, `{"image":"docker.io/library/alpine:3.22","command":["sleep","600"]}`)
	awaitRunning(t, client, url, token, parent)
	_, read := call(t, client, http.MethodGet, url+"/v1/sandboxes/"+parent, token, nil)
	mesh := statusField(t, read, "mesh")
	if mesh == "" || mesh == "<nil>" {
		t.Fatalf("the parent reports no mesh: %s", read)
	}
	workload := strings.TrimSpace(execIn(t, client, url, token, parent, "/bin/sh", "-c", `cat "$CELLA_TOKEN_FILE"`))
	if workload == "" {
		t.Fatal("the parent holds no workload token")
	}
	// The child is applied with the parent's own token, which is how a
	// sandbox adds a peer to its mesh; it is its parent's owner's, so the
	// caller reads, runs and deletes it by name.
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes", workload, []byte(`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",
		"metadata":{"name":"`+child+`"},"spec":{`+serve+`}}`))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the child's create answered %d: %s", code, body)
	}
	t.Cleanup(func() { deleteSandbox(t, client, url, token, child) })
	awaitRunning(t, client, url, token, child)
	awaitListening(t, client, url, token, parent)
	awaitListening(t, client, url, token, child)

	domain := driver.MeshObjectName(mesh)
	reach := func(from, to, line string) string {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			got := execIn(t, client, url, token, from, "/bin/sh", "-c", `{ printf '%s\n' "$3"; sleep 2; } | nc -w 5 "$1" "$2" 2>/dev/null`,
				"peer", to+"."+domain, strconv.Itoa(dialPort), line)
			// The peer's name is published by the cluster's DNS once its
			// endpoint is, a moment after it reads Running.
			if strings.Contains(got, line) || time.Now().After(deadline) {
				return got
			}
			time.Sleep(2 * time.Second)
		}
	}
	if got := reach(child, parent, "from the child"); !strings.Contains(got, "from the child") {
		t.Fatalf("the child did not reach %s.%s: %q", parent, domain, got)
	}
	if got := reach(parent, child, "from the parent"); !strings.Contains(got, "from the parent") {
		t.Fatalf("the parent did not reach %s.%s: %q", child, domain, got)
	}
	if got := execIn(t, client, url, token, outsider, "/bin/sh", "-c", directScript, "outsider", parent+"."+domain, strconv.Itoa(dialPort), "outside"); strings.TrimSpace(got) != "0" {
		t.Fatalf("a sandbox outside the mesh reached %s and brought back %s bytes", parent, got)
	}
}

// upstreamOf is the host:port a sandbox of the stack may be allowed to reach.
func upstreamOf(t *testing.T) string {
	t.Helper()
	if u := strings.TrimSpace(os.Getenv("CELLA_TEST_UPSTREAM")); u != "" {
		return u
	}
	return defaultUpstream
}

// createSandbox creates one sandbox from its spec and deletes it when the
// test ends.
func createSandbox(t *testing.T, client *http.Client, url, token, name, spec string) {
	t.Helper()
	manifest := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},"spec":` + spec + `}`
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes", token, []byte(manifest))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the create of %s answered %d: %s", name, code, body)
	}
	t.Cleanup(func() { deleteSandbox(t, client, url, token, name) })
}

// deleteSandbox deletes one sandbox and reports a delete that failed.
func deleteSandbox(t *testing.T, client *http.Client, url, token, name string) {
	t.Helper()
	if code, body := call(t, client, http.MethodDelete, url+"/v1/sandboxes/"+name, token, nil); code >= 300 && code != http.StatusNotFound {
		t.Errorf("the delete of %s answered %d: %s", name, code, body)
	}
}

// execIn runs one command inside a sandbox and returns what it printed.
func execIn(t *testing.T, client *http.Client, url, token, name string, command ...string) string {
	t.Helper()
	request, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes/"+name+"/exec?wait=1", token, request)
	if code != http.StatusOK {
		t.Fatalf("the exec in %s answered %d: %s", name, code, body)
	}
	var result struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("the exec in %s answered %s: %v", name, body, err)
	}
	return result.Stdout
}

// enforced reports whether an answer carries EgressEnforced true.
func enforced(body []byte) bool {
	var read struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &read) != nil {
		return false
	}
	for _, c := range read.Status.Conditions {
		if c.Type == "EgressEnforced" && c.Status == "True" {
			return true
		}
	}
	return false
}

// awaitEgressRecord waits until a sandbox's records hold a connection to the
// host with the decision. The gateway reports on its own stream, so a record
// arrives a moment after the connection.
func awaitEgressRecord(t *testing.T, client *http.Client, url, token, name, host, decision string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var body []byte
	for time.Now().Before(deadline) {
		var code int
		code, body = call(t, client, http.MethodGet, url+"/v1/sandboxes/"+name+"/egress?limit=50", token, nil)
		var page struct {
			Items []struct {
				Host     string `json:"host"`
				Decision string `json:"decision"`
			} `json:"items"`
		}
		if code == http.StatusOK && json.Unmarshal(body, &page) == nil {
			for _, r := range page.Items {
				if r.Host == host && r.Decision == decision {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the records of %s never held %s as %s: %s", name, host, decision, body)
}
