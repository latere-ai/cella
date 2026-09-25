// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
)

// logsDriver answers every log read the way the case needs: after its caller
// has gone, with the cancellation as the Kubernetes driver wraps it, or at
// once with a failure of its own.
type logsDriver struct {
	runtime.Driver
	entered chan struct{}
	fail    error
}

func (d logsDriver) Logs(ctx context.Context, _ string, _ runtime.LogsRequest) (io.ReadCloser, error) {
	if d.fail != nil {
		return nil, d.fail
	}
	close(d.entered)
	<-ctx.Done()
	return nil, fmt.Errorf("pod logs: %w", ctx.Err())
}

// TestACallerThatLeftIsNotADependencyFailure: a browser that aborts a log
// read while the driver is still opening it cancels the request's context,
// the driver's call ends on that cancellation, and the handler answers 503.
// Nobody reads that answer, and the failure is the caller's leaving, so the
// request is counted and logged as client_closed in the 4xx class. The same
// driver failure with the caller still waiting stays driver_unavailable.
func TestACallerThatLeftIsNotADependencyFailure(t *testing.T) {
	const route = "GET /v1/sandboxes/{id}/logs"
	t.Run("the caller left", func(t *testing.T) {
		entered := make(chan struct{})
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return logsDriver{Driver: d, entered: entered} })
		sink := &logSink{}
		f.h.(*handler).log = slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
		obj := f.sandbox("abandoned")
		sink.clear()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/v1/sandboxes/"+obj.Status.ID+"/logs", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		done := make(chan error, 1)
		go func() {
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = res.Body.Close()
			}
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the handler never reached the driver")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("the aborted read ended with %v", err)
		}

		if got := f.metrics.awaitCount(t, route); got != (request{route, "4xx", ClientClosed}) {
			t.Errorf("counted %+v, want the 4xx class and %s", got, ClientClosed)
		}
		line := logLines(t, sink, 1)[0]
		if line["status"] != "4xx" || line["code"] != ClientClosed {
			t.Errorf("the line records %v %v, want 4xx %s", line["status"], line["code"], ClientClosed)
		}
	})
	t.Run("the caller waited", func(t *testing.T) {
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver {
			return logsDriver{Driver: d, fail: errors.New("the log stream did not open")}
		})
		obj := f.sandbox("waited")
		f.request(http.MethodGet, "/v1/sandboxes/"+obj.Status.ID+"/logs", f.alice, "", http.StatusServiceUnavailable)
		if got := f.metrics.awaitCount(t, route); got != (request{route, "5xx", "driver_unavailable"}) {
			t.Errorf("counted %+v, want the 5xx class and driver_unavailable", got)
		}
	})
}
