// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package kind_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialPort is the port the echo sandbox serves and declares.
const dialPort = 8080

// TestClusterDial is the dial socket through the control plane the stack
// deployed, under the Role and the NetworkPolicy of the base: a sandbox
// serving an echo on a declared port, reached by two sockets at once, each
// carrying a line both ways, and a socket to a port the manifest did not
// declare closing with the refusal's code.
func TestClusterDial(t *testing.T) {
	url, token := stack(t)
	client := &http.Client{Timeout: 2 * time.Minute}

	awaitLive(t, client, url)
	const name = "tier-dial"
	// busybox's netcat serves every connection with its own cat, which is
	// an echo to two connections at once.
	manifest := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",
		"metadata":{"name":"` + name + `"},
		"spec":{"image":"docker.io/library/alpine:3.22",
			"command":["nc","-lk","-p","` + strconv.Itoa(dialPort) + `","-e","cat"],
			"network":{"ports":[{"name":"echo","port":` + strconv.Itoa(dialPort) + `}]}}}`
	code, body := call(t, client, http.MethodPost, url+"/v1/sandboxes", token, []byte(manifest))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the create answered %d: %s", code, body)
	}
	t.Cleanup(func() {
		if code, body := call(t, client, http.MethodDelete, url+"/v1/sandboxes/"+name, token, nil); code >= 300 && code != http.StatusNotFound {
			t.Errorf("the delete answered %d: %s", code, body)
		}
	})
	awaitRunning(t, client, url, token, name)
	awaitListening(t, client, url, token, name)

	// The server inside may come up a moment after the sandbox reads
	// Running, so the first socket is opened until a line makes the trip.
	first := dialUntilEcho(t, url, token, name)
	defer func() { _ = first.Close() }()
	second, err := dialSocket(url, token, name, dialPort)
	if err != nil {
		t.Fatalf("a second socket while the first is open: %v", err)
	}
	defer func() { _ = second.Close() }()
	if err := echoes(second, "second"); err != nil {
		t.Fatalf("the second socket: %v", err)
	}
	if err := echoes(first, "first again"); err != nil {
		t.Fatalf("the first socket once a second opened: %v", err)
	}

	// A port the manifest did not declare is not dialed: the socket opens,
	// because the dial runs after the upgrade, and closes with the code.
	undeclared, err := dialSocket(url, token, name, 22)
	if err != nil {
		t.Fatalf("the socket to an undeclared port: %v", err)
	}
	defer func() { _ = undeclared.Close() }()
	if err := undeclared.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, _, err = undeclared.ReadMessage()
	var closed *websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.CloseInternalServerErr || closed.Text != "not_found" {
		t.Fatalf("the socket to an undeclared port ended with %v, want 1011 not_found", err)
	}
}

// awaitListening polls the port listing until the probe inside reports the
// declared port listening, which is the port case of the driver on the
// cluster the stack runs.
func awaitListening(t *testing.T, client *http.Client, url, token, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var body []byte
	for time.Now().Before(deadline) {
		var code int
		code, body = call(t, client, http.MethodGet, url+"/v1/sandboxes/"+name+"/ports", token, nil)
		var list struct {
			Items []struct {
				Name  string `json:"name"`
				Port  int    `json:"port"`
				State string `json:"state"`
			} `json:"items"`
		}
		if code == http.StatusOK && json.Unmarshal(body, &list) == nil {
			for _, p := range list.Items {
				if p.Name == "echo" && p.Port == dialPort && p.State == "listening" {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("the port listing never reported echo listening: %s", body)
}

// dialSocket opens one dial socket to a port of a sandbox.
func dialSocket(url, token, name string, port int) (*websocket.Conn, error) {
	dialer := websocket.Dialer{Subprotocols: []string{"cella.dial.v1"}, HandshakeTimeout: 30 * time.Second}
	target := "ws" + strings.TrimPrefix(url, "http") + "/v1/sandboxes/" + name + "/dial/" + strconv.Itoa(port)
	conn, resp, err := dialer.Dial(target, http.Header{"Authorization": {"Bearer " + token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, errors.New(err.Error() + ": status " + resp.Status)
		}
		return nil, err
	}
	return conn, nil
}

// dialUntilEcho opens sockets until one carries a line both ways.
func dialUntilEcho(t *testing.T, url, token, name string) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		conn, err := dialSocket(url, token, name, dialPort)
		if err == nil {
			if err = echoes(conn, "first"); err == nil {
				return conn
			}
			_ = conn.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line came back through the dial socket: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// echoes sends one line as a binary frame and reads frames until the line has
// come back whole: the frames are bytes, so the line may arrive split.
func echoes(conn *websocket.Conn, line string) error {
	if err := conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(line+"\n")); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	var got []byte
	for !bytes.Contains(got, []byte("\n")) {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		got = append(got, data...)
	}
	if string(got) != line+"\n" {
		return errors.New("sent " + line + " and read back " + strings.TrimSpace(string(got)))
	}
	return nil
}
