// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellaclient_test

import (
	"bufio"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/cellaclient"
)

// The frames below are written by hand rather than by a library: the client
// under test is this repository's own RFC 6455 implementation, and a library
// on the other side would only prove the two agree where they agree.

// rawSocket is a server that completes the handshake and then hands the
// connection to the case, which writes whatever frames it wants to prove.
func rawSocket(t *testing.T, session func(conn net.Conn, br *bufio.Reader)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot be hijacked")
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"))
		session(conn, buffered.Reader)
	}))
	t.Cleanup(server.Close)
	return server
}

// serverFrame writes one frame as a server writes it: unmasked, with the
// length in the form its size calls for.
func serverFrameBytes(final bool, opcode int, payload []byte) []byte {
	first := byte(opcode)
	if final {
		first |= 0x80
	}
	out := []byte{first}
	switch n := len(payload); {
	case n < 126:
		out = append(out, byte(n))
	case n <= 0xffff:
		out = append(out, 126, 0, 0)
		binary.BigEndian.PutUint16(out[2:], uint16(n))
	default:
		out = append(out, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(out[2:], uint64(n))
	}
	return append(out, payload...)
}

// maskedServerFrame is the same frame with the mask bit set, which a server
// never sets and a client must still unmask rather than misread.
func maskedServerFrame(opcode int, payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	out := []byte{byte(0x80 | opcode), byte(0x80 | len(payload))}
	out = append(out, mask[:]...)
	return append(out, masked...)
}

// readFrame reads one frame the client sent, which is always masked.
func readFrame(t *testing.T, br *bufio.Reader) (opcode int, payload []byte) {
	t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(br, header); err != nil {
		return 0, nil
	}
	opcode = int(header[0] & 0x0f)
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		_, _ = io.ReadFull(br, extended)
		length = int64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		_, _ = io.ReadFull(br, extended)
		length = int64(binary.BigEndian.Uint64(extended))
	}
	mask := make([]byte, 4)
	if header[1]&0x80 != 0 {
		if _, err := io.ReadFull(br, mask); err != nil {
			return opcode, nil
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return opcode, nil
	}
	if header[1]&0x80 != 0 {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload
}

// TestTheFramingOfTheSocketIsRFC6455 drives the client against frames
// written by hand: fragmentation, a masked server frame, a control frame
// between the halves of a message, and the closes design 008 states.
func TestTheFramingOfTheSocketIsRFC6455(t *testing.T) {
	t.Run("a message split across frames is read whole", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br) // the request frame
			_, _ = conn.Write(serverFrameBytes(false, 0x2, []byte("half ")))
			_, _ = conn.Write(serverFrameBytes(false, 0x0, []byte("a ")))
			_, _ = conn.Write(serverFrameBytes(true, 0x0, []byte("message")))
			_, _ = conn.Write(serverFrameBytes(true, 0x1, []byte(`{"exit":4}`)))
			_, _ = conn.Write(serverFrameBytes(true, 0x8, closePayload(1000)))
		})
		out, code, err := drive(t, server.URL)
		if err != nil || code != 4 {
			t.Fatalf("the session ended %d, %v", code, err)
		}
		if out != "half a message" {
			t.Fatalf("the output is %q", out)
		}
	})

	t.Run("a masked server frame is unmasked", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(maskedServerFrame(0x2, []byte("masked")))
			_, _ = conn.Write(serverFrameBytes(true, 0x1, []byte(`{"exit":0}`)))
		})
		out, code, err := drive(t, server.URL)
		if err != nil || code != 0 || out != "masked" {
			t.Fatalf("the session read %q, ended %d, %v", out, code, err)
		}
	})

	t.Run("a close with no payload is a clean end", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(true, 0x8, nil))
		})
		out, code, err := drive(t, server.URL)
		if err != nil || code != 0 || out != "" {
			t.Fatalf("the session read %q, ended %d, %v", out, code, err)
		}
	})

	t.Run("a close that is not normal is a failure", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(true, 0x8, append(closePayload(1011), []byte("driver_unavailable")...)))
		})
		_, _, err := drive(t, server.URL)
		if err == nil || !strings.Contains(err.Error(), "1011") {
			t.Fatalf("the session ended with %v", err)
		}
	})

	t.Run("an opcode this protocol does not have ends the session", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(true, 0x3, []byte("reserved")))
		})
		if _, _, err := drive(t, server.URL); err == nil {
			t.Fatal("a reserved opcode was accepted")
		}
	})

	t.Run("a frame past the client's bound ends the session", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			header := []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint64(header[2:], 1<<30)
			_, _ = conn.Write(header)
		})
		_, _, err := drive(t, server.URL)
		if err == nil || !strings.Contains(err.Error(), "bound") {
			t.Fatalf("a frame of a gigabyte ended the session with %v", err)
		}
	})

	t.Run("a message that begins before the last one ended is refused", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(false, 0x2, []byte("first")))
			_, _ = conn.Write(serverFrameBytes(true, 0x2, []byte("second")))
		})
		if _, _, err := drive(t, server.URL); err == nil {
			t.Fatal("two messages interleaved were accepted")
		}
	})

	t.Run("a continuation with no message before it is refused", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(true, 0x0, []byte("orphan")))
		})
		if _, _, err := drive(t, server.URL); err == nil {
			t.Fatal("a continuation that began a message was accepted")
		}
	})

	t.Run("a text frame that is neither exit nor error is ignored", func(t *testing.T) {
		server := rawSocket(t, func(conn net.Conn, br *bufio.Reader) {
			readFrame(t, br)
			_, _ = conn.Write(serverFrameBytes(true, 0x1, []byte("not json")))
			_, _ = conn.Write(serverFrameBytes(true, 0x1, []byte(`{"note":"something new"}`)))
			_, _ = conn.Write(serverFrameBytes(true, 0x2, nil))
			_, _ = conn.Write(serverFrameBytes(true, 0x1, []byte(`{"exit":2}`)))
		})
		out, code, err := drive(t, server.URL)
		if err != nil || code != 2 || out != "" {
			t.Fatalf("the session read %q, ended %d, %v", out, code, err)
		}
	})
}

// closePayload is a close frame's two-byte code.
func closePayload(code int) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, uint16(code))
	return out
}

// drive opens one session against an address, reads it to the end, and
// returns what it read and how it ended.
func drive(t *testing.T, address string) (string, int, error) {
	t.Helper()
	c, err := cellaclient.New(cellaclient.Config{URL: address, Token: "t", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.ExecSession(t.Context(), "dev", cellaclient.ExecRequest{Command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	out, _ := io.ReadAll(s)
	code, err := s.Wait()
	return string(out), code, err
}

// TestTLSUsesTheSystemRootsAndTheAuthorityTheCallerNamed: --ca adds one
// authority, and the socket's handshake pins HTTP/1.1 so the upgrade has an
// HTTP/1.1 exchange to happen in.
func TestTLSUsesTheSystemRootsAndTheAuthorityTheCallerNamed(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeObject(w, 200, "dev", "sbx_1")
	}))
	defer server.Close()
	authority := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(authority, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	// Without the authority the handshake fails, which is what a trust
	// store is for.
	plain, err := cellaclient.New(cellaclient.Config{URL: server.URL, Token: "t", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = plain.GetSandbox(t.Context(), "dev"); err == nil {
		t.Fatal("a certificate signed by nothing the client trusts was accepted")
	}
	var refused *cellaclient.Unreachable
	if !errors.As(err, &refused) {
		t.Fatalf("a TLS failure is %T: %v", err, err)
	}

	trusting, err := cellaclient.New(cellaclient.Config{URL: server.URL, CAFile: authority, Token: "t", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = trusting.GetSandbox(t.Context(), "dev"); err != nil {
		t.Fatalf("the authority the caller named was not trusted: %v", err)
	}
	// The same trust store reaches the socket, whose dial is the client's
	// own and not the transport's.
	if _, err = trusting.ExecSession(t.Context(), "dev", cellaclient.ExecRequest{Command: []string{"sh"}}); err == nil {
		t.Fatal("this server upgrades nothing, so the session cannot open")
	}
	var refusal *cellaclient.Error
	if !errors.As(err, &refusal) || refusal.Status != 200 {
		t.Fatalf("the socket failed with %T: %v", err, err)
	}
}

// TestTheSystemRootsStandWhenNoAuthorityIsNamed: a client with no --ca
// verifies against the system store, which is what reaches a public
// endpoint.
func TestTheSystemRootsStandWhenNoAuthorityIsNamed(t *testing.T) {
	if _, err := x509.SystemCertPool(); err != nil {
		t.Skip("this platform has no system certificate pool")
	}
	c, err := cellaclient.New(cellaclient.Config{URL: "https://127.0.0.1:1", Token: "t", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.GetSandbox(t.Context(), "dev"); err == nil {
		t.Fatal("an address nothing answers was reached")
	}
}
