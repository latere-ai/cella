// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/k8s/fill"
)

// terminationLog is where a container's last words are read from: the
// kubelet copies this file into the Pod's status, which is how the driver
// learns why a fill failed (spec 019).
const terminationLog = "/dev/termination-log"

// fillRole is the program the Kubernetes driver runs in a fill Pod. `stage
// <dst>` copies this executable into the Pod's shared directory, from the
// fill image; `copy --max-bytes N <src> <dst>` then runs from that directory
// inside a container of the source image and writes its root file system into
// the volume. It reads no variable of the control plane's and opens no
// connection: a fill reaches the image's files and the volume and nothing
// else.
func fillRole(args []string, stdout, stderr io.Writer) int {
	return fillWith(args, stdout, stderr, terminationLog)
}

// fillWith is fillRole with the termination log a test names.
func fillWith(args []string, stdout, stderr io.Writer, logPath string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(stderr, "cellad fill: name a mode, %s <dst> or %s --%s N <src> <dst>\n",
			fill.ModeStage, fill.ModeCopy, fill.FlagMaxBytes)
		return 2
	}
	mode, rest := args[0], args[1:]
	fs := flag.NewFlagSet("cellad fill "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	maxBytes := fs.Int64(fill.FlagMaxBytes, 0, "the most bytes the copied files may hold; zero is no cap")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	switch {
	case mode == fill.ModeStage && fs.NArg() == 1:
		if err := fill.Stage(fs.Arg(0)); err != nil {
			return fillFailed(stderr, logPath, err)
		}
		return 0
	case mode == fill.ModeCopy && fs.NArg() == 2 && *maxBytes >= 0:
		result, err := fill.Copy(fs.Arg(0), fs.Arg(1), fill.Options{MaxBytes: *maxBytes})
		if err != nil {
			return fillFailed(stderr, logPath, err)
		}
		_, _ = fmt.Fprintln(stdout, result.String())
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "cellad fill: %q is not %s <dst> or %s --%s N <src> <dst>\n",
			args, fill.ModeStage, fill.ModeCopy, fill.FlagMaxBytes)
		return 2
	}
}

// fillFailed writes why a fill failed to stderr and to the termination log,
// prefixed with the reason status.reason carries where the cause has one, so
// the driver reads the reason off the Pod rather than guessing it from an
// exit code. The log exists only inside a Pod; elsewhere stderr is the whole
// of the answer.
func fillFailed(stderr io.Writer, logPath string, err error) int {
	message := err.Error()
	if errors.Is(err, fill.ErrTooLarge) {
		message = driver.ReasonSourceTooLarge + ": " + message
	}
	_, _ = fmt.Fprintln(stderr, "cellad fill: "+message)
	if _, statErr := os.Stat(logPath); statErr == nil {
		if writeErr := os.WriteFile(logPath, []byte(message), 0o644); writeErr != nil {
			_, _ = fmt.Fprintln(stderr, "cellad fill: the termination log was not written: "+writeErr.Error())
		}
	}
	return 1
}
