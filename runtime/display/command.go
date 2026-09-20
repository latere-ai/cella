// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package display

import (
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// StartScript is the desktop supervisor. It is embedded rather than written
// twice: the podman driver writes these bytes into the sandbox and the
// display image bakes the same file in, so the desktop both drivers bring up
// is the same desktop.
//
//go:embed start.sh
var StartScript string

// Where the desktop keeps what it needs inside the sandbox. It is under /tmp
// because /tmp is the writable mount the security baseline of spec 004
// grants, and on k8s it is the one emptyDir the workload and the desktop
// share, which is how the X socket reaches both.
const (
	HomeDir    = "/tmp/cella-display"
	StartPath  = HomeDir + "/start"
	PIDPath    = HomeDir + "/supervisor.pid"
	LogPath    = HomeDir + "/supervisor.log"
	SocketDir  = "/tmp/.X11-unix"
	SharedPath = "/tmp"
)

// What the desktop and the workload read to find the screen.
const (
	DisplayEnv   = "DISPLAY"
	DisplayValue = ":0"
	GeometryEnv  = "CELLA_DISPLAY_GEOMETRY"
	HomeEnv      = "HOME"
)

// Env is what both the desktop and a workload beside it need to address the
// screen. The geometry is read by the supervisor and the display by every
// tool.
func Env(g Geometry) map[string]string {
	return map[string]string{
		DisplayEnv:  DisplayValue,
		GeometryEnv: g.String(),
		HomeEnv:     HomeDir,
	}
}

// ReadyScript succeeds when the desktop is ready: the X server answers on its
// socket and a window manager has registered on the root window. Both halves
// are needed, because an X server with no manager accepts a connection and
// maps no window.
func ReadyScript() string {
	return "xdpyinfo -display " + DisplayValue + " >/dev/null 2>&1 && " +
		"xprop -display " + DisplayValue + " -root _NET_SUPPORTING_WM_CHECK 2>/dev/null | grep -q 'window id # 0x'"
}

// InstallScript writes the supervisor into the sandbox and starts it detached,
// once. It exits immediately when the recorded pid is a live supervisor, so
// the create, every start and a repeated call converge on one desktop.
//
// The body travels as base64 in the command rather than on standard input,
// because a driver that runs a command without a stdin stream can still send
// a file this way and the two drivers then run the same text.
func InstallScript(g Geometry) string {
	body := base64.StdEncoding.EncodeToString([]byte(StartScript))
	return strings.Join([]string{
		"set -e",
		"if test -s " + PIDPath + " && kill -0 \"$(cat " + PIDPath + ")\" 2>/dev/null; then exit 0; fi",
		"mkdir -p " + HomeDir,
		"printf %s " + quote(body) + " | base64 -d > " + StartPath,
		"chmod 700 " + StartPath,
		"setsid env " + HomeEnv + "=" + HomeDir + " " + DisplayEnv + "=" + DisplayValue +
			" " + GeometryEnv + "=" + g.String() + " sh " + StartPath + " >" + LogPath + " 2>&1 &",
		"echo $! > " + PIDPath,
	}, "; ")
}

// StopScript ends the supervisor and forgets its pid, which is what a stop
// does to a desktop that lives in the sandbox's own process tree.
func StopScript() string {
	return "if test -s " + PIDPath + "; then kill -- -\"$(cat " + PIDPath + ")\" 2>/dev/null || true; fi; rm -f " + PIDPath
}

// CaptureScript is one frame on standard output. The encoder is named by
// probing, because ImageMagick 7 installs magick and ImageMagick 6 installs
// convert and an image may carry either.
func CaptureScript(req ScreenshotRequest) string {
	return "set -e; c=$(command -v magick || command -v convert); " +
		"xwd -root -display " + DisplayValue + " | \"$c\" xwd:- " + resize(req.Scale) + encoder(req.Format) + ":-"
}

// MaxSessionSeconds bounds one screen session inside the sandbox. A container
// engine has no way to end an exec session it has started, so a viewer that
// vanishes would otherwise leave the capture loop running: the loop counts its
// own frames and ends itself.
const MaxSessionSeconds = 3600

// SessionPath is the file one screen session runs while it exists. Removing it
// ends the session's loop within one frame interval, which is how a driver
// stops a stream it can no longer read.
func SessionPath(session string) string { return HomeDir + "/session." + session }

// StreamScript is every frame of one screen session on standard output, each
// behind its length. One command serves the whole stream, because a command
// per frame would be ten calls a second into the container engine or the
// cluster's API server for as long as somebody watches.
//
// Each frame is captured to a file first, so the length is the whole frame's
// and a reader never meets a partial one.
func StreamScript(fps int, format, session string) string {
	return strings.Join([]string{
		"set -e",
		"c=$(command -v magick || command -v convert)",
		"mkdir -p " + HomeDir,
		"s=" + SessionPath(session),
		": > \"$s\"",
		"f=" + HomeDir + "/frame.$$",
		"trap 'rm -f \"$f\" \"$s\"' EXIT",
		"n=0",
		"while test -e \"$s\" && test \"$n\" -lt " + strconv.Itoa(frameCap(fps)),
		"do xwd -root -display " + DisplayValue + " | \"$c\" xwd:- " + encoder(format) + ":\"$f\"",
		"printf %0" + strconv.Itoa(FrameHeaderBytes) + "x $(wc -c < \"$f\")",
		"cat \"$f\"",
		"n=$((n+1))",
		"sleep " + interval(fps),
		"done",
	}, "; ")
}

// EndStreamScript ends one screen session by removing the file its loop runs
// while it exists.
func EndStreamScript(session string) string {
	return "rm -f " + SessionPath(session)
}

// frameCap is how many frames one session may produce before it ends itself.
func frameCap(fps int) int {
	if fps < 1 {
		fps = 1
	}
	if fps > MaxFPS {
		fps = MaxFPS
	}
	return fps * MaxSessionSeconds
}

// PortsScript is the kernel's own table of TCP sockets. Reading it answers for
// a port bound on any address inside the sandbox without opening a connection
// to the workload, which a probe by connecting cannot claim.
func PortsScript() string {
	return "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null || true"
}

// FrameHeaderBytes is the length prefix of one streamed frame: hexadecimal
// digits, fixed width, so a reader knows a frame's size before its first byte.
const FrameHeaderBytes = 8

// MaxFrameBytes bounds one frame, so a stream whose header is nonsense cannot
// make the control plane allocate without limit.
const MaxFrameBytes = 64 << 20

// ErrFrameTooLarge is a header naming more bytes than a frame may hold.
var ErrFrameTooLarge = errors.New("the stream announced a frame larger than the contract allows")

// ReadFrame reads one length-prefixed frame. The end of the stream between
// frames is io.EOF; one inside a frame is io.ErrUnexpectedEOF, so a caller
// tells a closed session from a truncated one.
func ReadFrame(r io.Reader) ([]byte, error) {
	var header [FrameHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(header[:])), 16, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("%w: the frame header %q is not a length", ErrInvalid, header)
	}
	if size > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(r, frame); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return frame, nil
}

// Listening is the local ports a kernel socket table reports as listening. A
// row's fourth column is the socket state and 0A is TCP_LISTEN; the local
// address is the second, and the port is its hexadecimal suffix.
func Listening(table string) map[int]bool {
	out := map[int]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		_, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		raw, err := hex.DecodeString(port)
		if err != nil || len(raw) != 2 {
			continue
		}
		out[int(raw[0])<<8|int(raw[1])] = true
	}
	return out
}

// encoder is the encoder's own name for a format: the tool spells a JPEG jpg.
func encoder(format string) string {
	if format == FormatJPEG {
		return "jpg"
	}
	return "png"
}

// resize is the scale as the encoder takes it, and nothing at full size.
func resize(scale float64) string {
	if scale <= 0 || scale >= MaxScale {
		return ""
	}
	return "-resize " + strconv.FormatFloat(scale*100, 'f', -1, 64) + "% "
}

// MaxFPS is the fastest a screen session is paced, which spec 023 fixes.
const MaxFPS = 10

// interval is the gap between frames in seconds, at most one frame per
// hundred milliseconds as spec 023 bounds the rate.
func interval(fps int) string {
	if fps < 1 {
		fps = 1
	}
	if fps > MaxFPS {
		fps = MaxFPS
	}
	return strconv.FormatFloat(1/float64(fps), 'f', 3, 64)
}

// quote wraps one argument for a POSIX shell.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
