// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"latere.ai/x/pkg/egress/placeholder"
)

// Redacted is what a denied value is replaced by, on either path of the log
// tee and in the message as well as the attributes.
const Redacted = "[redacted]"

// denied are the words design 017 puts on the key denylist. A key is denied
// when it contains one of them, case insensitively, so Authorization,
// Proxy-Authorization and Cella-Egress-Credential are covered by
// "authorization" and "credential", and a key spelled secretValue or
// tokenFile is covered whatever its casing.
var denied = []string{"env", "value", "token", "credential", "secret", "authorization"}

// placeholderPattern is the egress placeholder of design 018: the prefix and
// thirty-two characters of the lowercase base32 alphabet. A placeholder grants
// nothing on its own, and it names a secret that exists, so a log line that
// carries one is a line that says which secret a sandbox holds.
var placeholderPattern = regexp.MustCompile(regexp.QuoteMeta(placeholder.Prefix) + `[a-z2-7]{32}`)

// Redact wraps a handler so no attribute reaching it carries a secret value.
// It is applied once, to the logger pkg/otel.Bootstrap returns, which means
// it sits above the tee and both the local JSON handler and the OTLP bridge
// receive the redacted record.
//
// It is the second line and not the first. The rule at the call site stands:
// no handler passes a secret value, a credential or a token as an attribute
// at all.
func Redact(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	return redactor{Handler: h}
}

// redactor applies design 017's two rules to every record, to the attributes
// a logger carries from WithAttrs, and to the attributes inside a group.
type redactor struct{ slog.Handler }

func (r redactor) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, scrub(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.Handler.Handle(ctx, out)
}

func (r redactor) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redactor{Handler: r.Handler.WithAttrs(redactAttrs(attrs))}
}

func (r redactor) WithGroup(name string) slog.Handler {
	return redactor{Handler: r.Handler.WithGroup(name)}
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return out
}

// redactAttr applies the key rule, then the value rule, to one attribute. A
// group whose own name is denied collapses to the placeholder rather than
// being walked: the name says the whole subtree is a credential.
func redactAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if isDenied(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	if a.Value.Kind() == slog.KindGroup {
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(redactAttrs(a.Value.Group())...)}
	}
	if s := a.Value.String(); placeholderPattern.MatchString(s) {
		return slog.String(a.Key, scrub(s))
	}
	return a
}

// isDenied reports whether the key carries one of the denied words.
func isDenied(key string) bool {
	lower := strings.ToLower(key)
	for _, word := range denied {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// scrub replaces every egress placeholder in s.
func scrub(s string) string {
	if !strings.Contains(s, placeholder.Prefix) {
		return s
	}
	return placeholderPattern.ReplaceAllLiteralString(s, Redacted)
}
