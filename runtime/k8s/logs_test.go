// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

// logOptions returns the options the driver asked the cluster for.
func logOptions(t *testing.T, h *harness) *corev1.PodLogOptions {
	t.Helper()
	for _, action := range h.cs.Actions() {
		generic, ok := action.(k8stesting.GenericAction)
		if !ok || action.GetSubresource() != "log" {
			continue
		}
		if opts, ok := generic.GetValue().(*corev1.PodLogOptions); ok {
			return opts
		}
	}
	t.Fatal("no request for a pod log was made")
	return nil
}

func TestLogsReadsThePodsOutput(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_logs"
	h.created(t, spec(id))
	stream, err := h.Logs(t.Context(), id, driver.LogsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("the log stream carried nothing")
	}
	opts := logOptions(t, h)
	if opts.Container != Container || opts.Follow || opts.SinceTime != nil || opts.TailLines != nil {
		t.Fatalf("a plain request asked for %+v", opts)
	}
}

func TestLogsCarriesTheRequest(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_logsopts"
	h.created(t, spec(id))
	since := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	stream, err := h.Logs(t.Context(), id, driver.LogsRequest{Follow: true, Since: since, TailLines: 3})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	opts := logOptions(t, h)
	if !opts.Follow {
		t.Error("Follow did not reach the cluster")
	}
	if opts.SinceTime == nil || !opts.SinceTime.Time.Equal(since) {
		t.Errorf("SinceTime %v", opts.SinceTime)
	}
	if opts.TailLines == nil || *opts.TailLines != 3 {
		t.Errorf("TailLines %v", opts.TailLines)
	}
}

func TestLogsNeedAPod(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_logsstopped"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Logs(t.Context(), id, driver.LogsRequest{}); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("Logs while stopped = %v, want ErrNotRunning", err)
	}
}

func TestLogsReportsAFailedStream(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_logsfail"
	h.created(t, spec(id))
	// The record reads from the client double, which ignores the context; the
	// stream is a real request, so a caller that has hung up fails there.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.Logs(ctx, id, driver.LogsRequest{}); err == nil {
		t.Fatal("a log stream to a caller that hung up was opened")
	}
}
