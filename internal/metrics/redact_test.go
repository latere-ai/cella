// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/metrics"
)

// canaries are one value per kind design 017 names, each written under a key
// the denylist covers or carrying the placeholder of design 018. None of them
// may appear on either path of the log tee.
var canaries = map[string]string{
	"env":                     "CELLA_TOKEN_KEY=canary-env-value",
	"value":                   "canary-secret-value",
	"token":                   "canary-bearer-token",
	"credential":              "canary-credential",
	"secret":                  "canary-secret",
	"Authorization":           "Bearer canary-authorization",
	"Proxy-Authorization":     "Basic canary-proxy-authorization",
	"Cella-Egress-Credential": "canary-egress-credential",
	"secretValue":             "canary-camel-case",
	"CELLA_TOKEN_FILE":        "canary-upper-case",
}

// placeholderCanary is a well-formed egress placeholder: the prefix and
// thirty-two characters of the lowercase base32 alphabet.
const placeholderCanary = "cph_abcdefghijklmnopqrstuvwxyz234567"

// logger builds a redacting logger over a buffer, which is the local path of
// the tee pkg/otel.Bootstrap installs.
func logger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := metrics.Redact(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return slog.New(h), &buf
}

// TestLogsRedact writes every canary through every shape a log line takes and
// asserts none of them reaches the handler.
func TestLogsRedact(t *testing.T) {
	log, buf := logger()
	for key, value := range canaries {
		log.Info("a line with an attribute", key, value)
		log.With(key, value).Info("a line with a carried attribute")
		log.Info("a line with a group", slog.Group("outer", key, value))
		log.WithGroup("outer").Info("a line under a group", key, value)
	}
	out := buf.String()
	for key, value := range canaries {
		if strings.Contains(out, value) {
			t.Errorf("the canary under %q reached the handler: %s", key, line(out, value))
		}
	}
	if !strings.Contains(out, metrics.Redacted) {
		t.Error("nothing was redacted at all; the check would pass vacuously")
	}
}

// TestPlaceholdersAreScrubbed is design 017's second rule: a placeholder
// grants nothing on its own and still names a secret a sandbox holds.
func TestPlaceholdersAreScrubbed(t *testing.T) {
	log, buf := logger()
	log.Info("the boundary substituted " + placeholderCanary)
	log.Info("a line", "placeholder", placeholderCanary)
	log.Info("a line", "upstream", "https://api.example.com/?k="+placeholderCanary)
	log.With("carried", placeholderCanary).Info("a carried line")
	log.Info("a grouped line", slog.Group("boundary", "seen", placeholderCanary))
	out := buf.String()
	if strings.Contains(out, placeholderCanary) {
		t.Errorf("a placeholder reached the handler: %s", line(out, placeholderCanary))
	}
	if strings.Count(out, metrics.Redacted) != 5 {
		t.Errorf("want five redactions, got %d:\n%s", strings.Count(out, metrics.Redacted), out)
	}
	// The rest of the value survives: the line still says which host was
	// dialled, which is what makes it worth keeping.
	if !strings.Contains(out, "https://api.example.com/?k="+metrics.Redacted) {
		t.Errorf("the placeholder's surroundings were dropped:\n%s", out)
	}
}

// TestRedactionKeepsWhatIsNotSecret holds the handler to replacing values and
// nothing else: the message, the level, the keys and every other attribute
// reach the handler unchanged.
func TestRedactionKeepsWhatIsNotSecret(t *testing.T) {
	log, buf := logger()
	log.Warn("the pool deleted an entry", "sandbox", "sbx_123", "reason", "Surplus", "attempt", 2)
	out := buf.String()
	for _, want := range []string{`"msg":"the pool deleted an entry"`, `"level":"WARN"`, `"sandbox":"sbx_123"`, `"reason":"Surplus"`, `"attempt":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("the handler dropped %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, metrics.Redacted) {
		t.Errorf("a line with no secret was redacted:\n%s", out)
	}
}

// TestRedactionResolvesAndTypes proves the two shapes a value arrives in that
// a naive handler misses: a LogValuer that produces the secret only when
// resolved, and a group whose own key is denied.
func TestRedactionResolvesAndTypes(t *testing.T) {
	log, buf := logger()
	log.Info("a resolved value", "credential", lazy(placeholderCanary))
	log.Info("a denied group", slog.Group("credential", "host", "api.example.com", "value", "canary-secret-value"))
	log.Info("a resolved placeholder", "upstream", lazy(placeholderCanary))
	out := buf.String()
	for _, canary := range []string{placeholderCanary, "canary-secret-value"} {
		if strings.Contains(out, canary) {
			t.Errorf("a resolved canary reached the handler: %s", line(out, canary))
		}
	}
	if strings.Contains(out, "api.example.com") {
		t.Errorf("a group under a denied key was walked instead of collapsed:\n%s", out)
	}
}

// TestRedactNilIsNil keeps the wiring honest: a caller with no handler gets
// no handler and not one that panics on the first line.
func TestRedactNilIsNil(t *testing.T) {
	if metrics.Redact(nil) != nil {
		t.Error("Redact(nil) returned a handler")
	}
}

// TestRedactorDelegatesEnabled proves the wrapper does not widen the level:
// a handler that drops debug lines still drops them through the wrapper.
func TestRedactorDelegatesEnabled(t *testing.T) {
	var buf bytes.Buffer
	h := metrics.Redact(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("the wrapper enabled a level the handler under it drops")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("the wrapper dropped a level the handler under it takes")
	}
}

// lazy is a value that produces the canary only when the handler resolves it.
type lazy string

func (l lazy) LogValue() slog.Value { return slog.StringValue(string(l)) }

// line is the one output line holding s, for a failure message that does not
// print the whole buffer.
func line(out, s string) string {
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}
