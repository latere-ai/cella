#!/bin/sh
# The display supervisor of spec 023: an X server on the declared geometry and
# a window manager, each restarted when it exits. It runs in the foreground,
# so a container whose command it is ends when it is ended, and the podman
# driver detaches it itself.
#
# This file is the one copy. The Go package embeds it, the podman driver
# writes those bytes into the sandbox, and the display image bakes them in.
set -eu

DISPLAY="${DISPLAY:-:0}"
export DISPLAY
GEOMETRY="${CELLA_DISPLAY_GEOMETRY:-1280x800x24}"
HOME="${HOME:-/tmp/cella-display}"
export HOME
XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/tmp/cella-display/run}"
export XDG_RUNTIME_DIR
mkdir -p "$HOME" "$XDG_RUNTIME_DIR"
chmod 700 "$XDG_RUNTIME_DIR"

pids=""

stop() {
	for pid in $pids; do
		kill "$pid" 2>/dev/null || true
	done
	exit 0
}
trap stop INT TERM

# supervise restarts one child whenever it exits, so a window manager that
# crashes does not take the desktop down with it and a child that starts
# before the X server is up succeeds on its next turn.
supervise() {
	name="$1"
	shift
	(
		while :; do
			echo "cella-display: starting $name" >&2
			"$@" || echo "cella-display: $name exited $?" >&2
			sleep 1
		done
	) &
	pids="$pids $!"
}

supervise xserver Xvfb "$DISPLAY" -screen 0 "$GEOMETRY" -nolisten tcp -ac

# The window manager registers _NET_SUPPORTING_WM_CHECK on the root window,
# which is the half of readiness that says a window can be managed. The first
# one present wins, and each of these is an EWMH manager, which a bare twm is
# not.
found=""
for wm in openbox mutter xfwm4; do
	if command -v "$wm" >/dev/null 2>&1; then
		found="$wm"
		break
	fi
done
case "$found" in
"") echo "cella-display: no window manager in this image; the desktop will not become ready" >&2 ;;
mutter) supervise "$found" "$found" --x11 ;;
*) supervise "$found" "$found" ;;
esac

wait
