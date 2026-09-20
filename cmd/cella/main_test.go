// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
)

// The walk below is the command against a running control plane: `cellad
// serve` on the native runtime with a stub issuer on loopback, and every
// command of design 011 this slice builds driven through this binary's own
// entry point. Nothing is faked between the two: the requests are HTTP, the
// bearer is a token the issuer signed, and the exit codes are the ones a
// shell would see.

// listening is the line the server writes once its listeners are up.
var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+) runtime=(\S+)`)

// node is one running control plane and the caller's token for it.
type node struct {
	url   string
	token string
}

// syncBuffer collects the server's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startNode builds the server, starts it on loopback with a stub issuer,
// and returns its address and a caller's bearer. The binary is the one the
// release carries, built here because a client and a server that agree in
// one process prove less than two processes that agree over HTTP.
func startNode(t *testing.T) node {
	t.Helper()
	if testing.Short() {
		t.Skip("the walk builds and runs the server")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no Go toolchain on PATH: %v", err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cellad")
	build := exec.CommandContext(t.Context(), goBin, "build", "-o", binary, "./cmd/cellad")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the server: %v\n%s", err, out)
	}

	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signing := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, binary, "serve")
	server.Env = append(os.Environ(),
		"CELLA_OIDC_ISSUERS="+issuer.URL(),
		"CELLA_RUNTIME=native",
		"CELLA_ALLOW_UNSAFE_NATIVE=true",
		"CELLA_PUBLIC_ADDR=127.0.0.1:0",
		"CELLA_INTERNAL_ADDR=127.0.0.1:0",
		"CELLA_PUBLIC_URL=http://127.0.0.1:0",
		"CELLA_DATA_DIR="+filepath.Join(dir, "state"),
		"CELLA_TOKEN_KEY="+string(signing),
		// The Secret kind needs the key its values are sealed under, which
		// is what makes `cella apply` of a Secret reach a store rather than
		// a refusal (design 046).
		"CELLA_SECRET_KEY="+sealingKey(t),
	)
	var out syncBuffer
	var errOut syncBuffer
	server.Stdout, server.Stderr = &out, &errOut
	if err = server.Start(); err != nil {
		t.Fatalf("starting the server: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		_ = server.Wait()
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(30 * time.Second):
			t.Error("the server did not stop")
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return node{url: "http://" + m[1], token: issuer.Mint(issuertest.Claims{Sub: "alice"})}
		}
		select {
		case <-stopped:
			t.Fatalf("the server stopped before listening:\n%s\n%s", out.String(), errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never reported its listeners:\n%s\n%s", out.String(), errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sealingKey is the key one run wraps its secret values under.
func sealingKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// moduleRoot is the checkout this test runs inside.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// cella runs the command against the node, through the same entry point the
// process uses, and returns the exit code and the two streams.
func (n node) cella(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut, func(key string) string {
		switch key {
		case "CELLA_URL":
			return n.url
		case "CELLA_TOKEN":
			return n.token
		default:
			return ""
		}
	})
	return code, out.String(), errOut.String()
}

// TestTheCommandDrivesARunningNode is the walk of design 011 against a real
// control plane: apply, get, exec, files, logs, stop, start, delete, and the
// exit codes each of them leaves behind.
func TestTheCommandDrivesARunningNode(t *testing.T) {
	n := startNode(t)
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"walk"},` +
		`"spec":{"command":["sh","-c","while true; do echo alive; sleep 1; done"]}}`

	code, out, errOut := n.cella(t, document, "apply", "-f", "-", "-w")
	if code != 0 {
		t.Fatalf("apply exited %d: %s%s", code, out, errOut)
	}
	if out != "sandbox/walk applied\n" {
		t.Fatalf("apply wrote %q", out)
	}

	// The object is there, in columns and as the API's own bytes.
	code, out, errOut = n.cella(t, "", "get", "sandbox", "walk")
	if code != 0 {
		t.Fatalf("get exited %d: %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "walk") || !strings.Contains(out, "Running") {
		t.Fatalf("the row is %q", out)
	}
	code, out, _ = n.cella(t, "", "get", "sandbox", "walk", "--json")
	if code != 0 {
		t.Fatalf("get --json exited %d", code)
	}
	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &object); err != nil {
		t.Fatalf("the answer is no object: %v\n%s", err, out)
	}
	if object.Status.ID == "" || object.Metadata.Name != "walk" {
		t.Fatalf("the object is %+v", object)
	}
	// The list carries it too, and the id addresses the same object as the
	// name.
	code, out, _ = n.cella(t, "", "get", "sandboxes")
	if code != 0 || !strings.Contains(out, "walk") {
		t.Fatalf("the list exited %d and holds %q", code, out)
	}
	if code, out, _ = n.cella(t, "", "get", "sandbox", object.Status.ID, "-o", "name"); code != 0 || out != "sandbox/walk\n" {
		t.Fatalf("the id addressed %q with exit %d", out, code)
	}

	// A command inside, its output and its exit code.
	code, out, errOut = n.cella(t, "", "exec", "walk", "--", "sh", "-c", "echo from inside; echo to stderr >&2")
	if code != 0 {
		t.Fatalf("exec exited %d: %s%s", code, out, errOut)
	}
	if out != "from inside\n" || !strings.Contains(errOut, "to stderr") {
		t.Fatalf("exec wrote %q and %q", out, errOut)
	}
	if code, _, _ = n.cella(t, "", "exec", "walk", "--", "sh", "-c", "exit 9"); code != 9 {
		t.Fatalf("the child exited 9 and the command exited %d", code)
	}

	// A file in, the same file out.
	local := filepath.Join(t.TempDir(), "carried.txt")
	if err := os.WriteFile(local, []byte("carried in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut = n.cella(t, "", "files", "put", local, "walk:/workspace/carried.txt"); code != 0 {
		t.Fatalf("files put exited %d: %s%s", code, out, errOut)
	}
	// The file routes address the workspace as /workspace; a command inside
	// starts there, so it reads the same file by its own name.
	if code, out, errOut = n.cella(t, "", "exec", "walk", "--", "cat", "carried.txt"); code != 0 || out != "carried in\n" {
		t.Fatalf("the file inside reads %q with exit %d: %s", out, code, errOut)
	}
	if code, out, _ = n.cella(t, "", "files", "get", "walk:/workspace/carried.txt"); code != 0 || out != "carried in\n" {
		t.Fatalf("files get wrote %q with exit %d", out, code)
	}
	if code, out, _ = n.cella(t, "", "files", "ls", "walk:/workspace"); code != 0 || !strings.Contains(out, "carried.txt") {
		t.Fatalf("files ls wrote %q with exit %d", out, code)
	}
	// The whole tree in one archive, both ways.
	down := filepath.Join(t.TempDir(), "down")
	if code, out, errOut = n.cella(t, "", "cp", "walk:/workspace", down); code != 0 {
		t.Fatalf("cp out exited %d: %s%s", code, out, errOut)
	}
	// The archive's names are workspace-relative, so the tree lands under
	// the destination directory itself.
	if data, err := os.ReadFile(filepath.Join(down, "carried.txt")); err != nil || string(data) != "carried in\n" {
		t.Fatalf("the archive carried %q, %v", data, err)
	}

	// The main process's output.
	code, out, errOut = n.cella(t, "", "logs", "walk", "--tail", "5")
	if code != 0 {
		t.Fatalf("logs exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "alive") {
		t.Fatalf("the log holds %q", out)
	}

	// The two verbs, and the phases they leave behind.
	if code, out, errOut = n.cella(t, "", "stop", "walk"); code != 0 {
		t.Fatalf("stop exited %d: %s%s", code, out, errOut)
	}
	if code, out, _ = n.cella(t, "", "get", "sandbox", "walk", "--json"); code != 0 {
		t.Fatalf("get after stop exited %d", code)
	}
	if err := json.Unmarshal([]byte(out), &object); err != nil {
		t.Fatal(err)
	}
	if object.Status.Phase != "Stopped" {
		t.Fatalf("the sandbox is %s after stop", object.Status.Phase)
	}
	if code, out, errOut = n.cella(t, "", "start", "walk"); code != 0 {
		t.Fatalf("start exited %d: %s%s", code, out, errOut)
	}

	// The identity of both sides, over the same address.
	code, out, _ = n.cella(t, "", "version")
	if code != 0 || !strings.HasPrefix(out, "cella ") || !strings.Contains(out, "server ") {
		t.Fatalf("version exited %d and wrote %q", code, out)
	}

	// The end, and what a reference that is gone answers.
	if code, out, errOut = n.cella(t, "", "delete", "sandbox", "walk"); code != 0 {
		t.Fatalf("delete exited %d: %s%s", code, out, errOut)
	}
	code, _, errOut = n.cella(t, "", "get", "sandbox", "walk")
	if code != 4 {
		t.Fatalf("a sandbox that is gone answered exit %d, want 4: %s", code, errOut)
	}
	if !strings.Contains(errOut, "There is no such object.") {
		t.Fatalf("the refusal reads %q", errOut)
	}
}

// TestTheWalkOfARefusalAndASession covers the paths the walk above does not:
// a refusal a real server writes, a session over the exec socket, and the
// secret kind, whose value no answer carries.
func TestTheWalkOfARefusalAndASession(t *testing.T) {
	n := startNode(t)

	// A manifest the server refuses is exit 3, with the code under -v.
	code, _, errOut := n.cella(t,
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"bad"},"spec":{"nosuchfield":true}}`,
		"apply", "-f", "-", "-v")
	if code != 3 {
		t.Fatalf("a manifest with an unknown field exited %d, want 3: %s", code, errOut)
	}
	// The request line carries the id the server stamped, whatever shape it
	// gives it.
	if !strings.Contains(errOut, "code: unknown_field") || !strings.Contains(errOut, "request: ") {
		t.Fatalf("the refusal under -v reads %q", errOut)
	}

	// A session over the socket, with what the caller types reaching the
	// command inside.
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"session"},` +
		`"spec":{"command":["sh","-c","sleep 60"]}}`
	if code, out, errOut := n.cella(t, document, "apply", "-f", "-", "-w"); code != 0 {
		t.Fatalf("apply exited %d: %s%s", code, out, errOut)
	}
	code, out, errOut := n.cella(t, "echo typed in\nexit 4\n", "exec", "session", "-i", "--", "sh")
	if code != 4 {
		t.Fatalf("the session exited %d, want the shell's 4: %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "typed in") {
		t.Fatalf("the session wrote %q", out)
	}

	// The Secret kind: applied by name, its value never answered.
	const canary = "walk-secret-canary"
	secret := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret","metadata":{"name":"api"},` +
		`"spec":{"kind":"static","scope":{"hosts":["api.example.com"]}}}`
	code, out, errOut = n.cella(t, secret, "apply", "-f", "-", "--value-from-env", "CELLA_URL")
	if code != 0 {
		t.Fatalf("the secret exited %d: %s%s", code, out, errOut)
	}
	code, out, errOut = n.cella(t, "", "get", "secrets", "--json")
	if code != 0 {
		t.Fatalf("get secrets exited %d: %s", code, errOut)
	}
	if strings.Contains(out, canary) || strings.Contains(out, `"value"`) {
		t.Fatalf("a secret's answer carries a value: %s", out)
	}
	if code, out, _ = n.cella(t, "", "get", "secret", "api"); code != 0 || !strings.Contains(out, "api.example.com") {
		t.Fatalf("the secret's row is %q with exit %d", out, code)
	}
	if code, _, errOut = n.cella(t, "", "delete", "secret", "api"); code != 0 {
		t.Fatalf("deleting the secret exited %d: %s", code, errOut)
	}
	if code, _, _ = n.cella(t, "", "delete", "sandbox", "session"); code != 0 {
		t.Fatalf("deleting the sandbox exited %d", code)
	}
}

// TestMainIsTheEntryPoint: the process reads its own arguments and ends with
// the exit code the command decided on.
func TestMainIsTheEntryPoint(t *testing.T) {
	arguments, ending := os.Args, exit
	t.Cleanup(func() { os.Args, exit = arguments, ending })
	os.Args = []string{"cella", "frobnicate"}
	code := -1
	exit = func(c int) { code = c }
	main()
	if code != 2 {
		t.Fatalf("the process ended with %d, want the usage error's 2", code)
	}
}

// TestTheProcessEntryPointCarriesItsOwnEdges: the binary reads its terminal,
// its environment and its arguments, and reports an identity of the form the
// server's own binary reports.
func TestTheProcessEntryPointCarriesItsOwnEdges(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, strings.NewReader(""), &out, &errOut, func(string) string { return "" }); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != identity() {
		t.Fatalf("version wrote %q and the identity is %q", got, identity())
	}
	if !strings.HasPrefix(identity(), "cella dev (") {
		t.Fatalf("a development build reports %q", identity())
	}
	// A reader that is not a file has no terminal, and a file that is not a
	// terminal has none either.
	if terminalOf(strings.NewReader("")) != nil {
		t.Error("a pipe was read as a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "notaterminal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if terminalOf(f) != nil {
		t.Error("an ordinary file was read as a terminal")
	}
	// An unknown command is the usage error design 011 gives exit 2.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"frobnicate"}, strings.NewReader(""), &out, &errOut, func(string) string { return "" }); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
