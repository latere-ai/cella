// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/internal/metrics"
	"latere.ai/x/cella/internal/version"
)

// telemetryTimeout bounds the flush at shutdown. A collector that has stopped
// answering must not hold the process open past its grace period.
const telemetryTimeout = 5 * time.Second

// telemetry is what design 017 turns on, per role: the logger every line goes
// through, the word the start-up line says, and the flush.
type telemetry struct {
	log      *slog.Logger
	mode     string
	shutdown func()
}

// startTelemetry wires design 017's three signals for one role and returns
// the logger to build every collaborator with.
//
// It runs before the store, the identity and the driver are opened, because a
// collaborator built earlier captures the slog default as it stands and a
// later SetDefault does not reach it.
//
// Export is off unless the deployment set OTEL_EXPORTER_OTLP_ENDPOINT, which
// is pkg/otel's variable and not this project's. With it unset the logger is
// local JSON on stderr and the traces and process metrics are discarded; the
// scrape surface is served either way, because it is this process's own
// listener and needs no collector.
//
// The handler pkg/otel returns is the tee: the local JSON handler and the
// OTLP bridge. Wrapping it once is what puts design 017's redaction on both
// paths, and the wrapped logger is set as the default again so a package that
// reaches for slog.Default gets the redacting one.
func startTelemetry(ctx context.Context, role string, stderr io.Writer) telemetry {
	logger, shutdown, err := otel.Bootstrap(ctx, otel.Config{
		ServiceName: role,
		Version:     version.Version,
		Stdout:      slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
	})
	redacting := slog.New(metrics.Redact(logger.Handler()))
	slog.SetDefault(redacting)
	if err != nil {
		// The log bridge is degraded and the local path is not, so the
		// process serves and says so rather than refusing to start.
		redacting.Warn("the OTLP log bridge is degraded; logging stays local", "err", err)
	}
	return telemetry{
		log:  redacting,
		mode: telemetryMode(),
		shutdown: func() {
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryTimeout)
			defer cancel()
			if err := shutdown(stop); err != nil {
				redacting.Warn("telemetry did not flush", "err", err)
			}
		},
	}
}

// telemetryMode is the word the start-up line says, so an operator reads from
// the first line whether anything is leaving this process.
func telemetryMode() string {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" || os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return "off"
	}
	return "otlp"
}

// phaseCounts is the scrape-time answer for cella_sandboxes: the phases of
// the controller's cached index, which is a map read under its own lock and
// never a driver call.
func phaseCounts(phases []string) map[string]int {
	counts := make(map[string]int, len(phases))
	for _, phase := range phases {
		counts[phase]++
	}
	return counts
}
