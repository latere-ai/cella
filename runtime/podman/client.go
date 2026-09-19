// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiVersion is the libpod API version segment. Podman serves earlier versions
// compatibly, so a recent floor costs nothing and names what was tested.
const apiVersion = "5.0.0"

// DefaultSockets are the candidates tried in order when no socket is named:
// the rootless user socket first, then the system one. $XDG_RUNTIME_DIR is
// read at call time, so a test points the search at its own directory.
func DefaultSockets() []string {
	var out []string
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		out = append(out, filepath.Join(dir, "podman", "podman.sock"))
	}
	return append(out, "/run/podman/podman.sock")
}

// client speaks the libpod API (/vN/libpod/...) and the compatibility API
// (/vN/...), which carries the archive endpoints, over one unix socket. The
// request types are written out here rather than taken from a podman module:
// the driver's whole build list is the contract packages and the standard
// library, which the architecture test holds it to.
type client struct {
	hc     *http.Client
	socket string
}

// newClient dials socket for every request. The host in each URL is a
// placeholder the dialer ignores.
func newClient(socket string) *client {
	return &client{
		socket: socket,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				DisableCompression: true,
			},
		},
	}
}

func (c *client) libpodURL(p string) string { return "http://d/v" + apiVersion + "/libpod" + p }

// libpodPath is the same route without the placeholder host, which a hijacked
// request writes into its own request line.
func (c *client) libpodPath(p string) string { return "/v" + apiVersion + "/libpod" + p }

// dial opens one connection to the socket outside the pooled client, for a
// request whose reply is a raw byte stream rather than an HTTP body.
func (c *client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.socket)
}

// hijack issues an upgrade request and hands back the connection the engine
// then speaks bytes over. It is written by hand because net/http gives no way
// to read a 200 reply whose body has no framing: podman answers the exec
// start either with 101 and an upgraded connection or with 200 and the stream
// in place of a body.
func (c *client) hijack(ctx context.Context, path string, body any) (net.Conn, *bufio.Reader, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "POST %s HTTP/1.1\r\n", path)
	req.WriteString("Host: d\r\n")
	req.WriteString("Content-Type: application/json\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Upgrade: tcp\r\n")
	fmt.Fprintf(&req, "Content-Length: %d\r\n\r\n", len(payload))
	req.Write(payload)
	if _, err = conn.Write(req.Bytes()); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	// The accepted reply's body is never closed: a 200 upgrade has no framing,
	// so closing it would drain the hijacked stream and block. The caller reads
	// from br and tears the session down by closing conn. Only the refused
	// reply is closed, after the connection, so the drain fails at once.
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		// The refusal's own message is read before the connection goes, so the
		// caller sees what podman said and not only the status.
		serr := statusErr(resp)
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, nil, serr
	}
	return conn, br, nil
}
func (c *client) compatURL(p string) string { return "http://d/v" + apiVersion + p }

// do issues one request with an optional JSON body and returns the raw
// response; the caller closes the body. Streaming endpoints use it directly.
func (c *client) do(ctx context.Context, method, rawURL string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.hc.Do(req)
}

// json issues a libpod request and decodes the reply into out, which may be
// nil to discard it. A non-2xx status becomes an *apiError.
func (c *client) json(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, c.libpodURL(path), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiError is a reply podman refused with.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("podman: status %d: %s", e.Status, e.Message) }

// statusErr turns a non-2xx reply into an *apiError carrying podman's own
// message. 304 is podman's "there was nothing to do" on start and stop, which
// is success for an idempotent operation.
func statusErr(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	var payload struct {
		Message string `json:"message"`
		Cause   string `json:"cause"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(b, &payload)
	msg := payload.Message
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	return &apiError{Status: resp.StatusCode, Message: msg}
}

// notFound reports whether err is podman's 404.
func notFound(err error) bool {
	ae, ok := errors.AsType[*apiError](err)
	return ok && ae.Status == http.StatusNotFound
}

// conflict reports whether err is a reply that names an object that already
// exists. Podman answers a duplicate volume create with 500 and that sentence
// rather than 409, so the driver reads the sentence.
func conflict(err error) bool {
	ae, ok := errors.AsType[*apiError](err)
	return ok && (ae.Status == http.StatusConflict || strings.Contains(ae.Message, "already exists") || strings.Contains(ae.Message, "in use"))
}

// query builds an encoded query string from key and value pairs, dropping the
// pairs whose value is empty.
func query(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v.Encode()
}

// labelKeyFilter encodes the list endpoints' filter that selects every object
// carrying a label key, whatever its value.
func labelKeyFilter(key string) string {
	b, err := json.Marshal(map[string][]string{"label": {key}})
	if err != nil {
		return ""
	}
	return string(b)
}

// ping reports whether the engine on the socket answers within timeout.
func (c *client) ping(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, c.libpodURL("/info"), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return statusErr(resp)
}
