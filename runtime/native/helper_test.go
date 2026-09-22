// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
)

// echoArg turns this test binary into the echo server a sandbox runs as its
// main command in the dial and port cases. The binary is already on the host,
// so the cases need no tool of the host's: a netcat's flags differ between
// systems, and some have none.
const echoArg = "cella-test-echo"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == echoArg {
		if err := serveEcho(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// echoCommand is the main command that serves one port on loopback and
// writes back what each connection sends.
func echoCommand(port int) []string {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return []string{exe, echoArg, strconv.Itoa(port)}
}

// serveEcho listens on the port and echoes every connection until the
// process is killed, which is how the driver ends a sandbox's main process.
func serveEcho(port string) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()
	}
}
