// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/internal/events"
)

// Events is spec 009's half of the configuration: where records go, what
// signs them, and how long one is retried.
type Events struct {
	// URL is CELLA_EVENTS_URL. Empty turns delivery off; records are still
	// journaled, so turning it on later delivers what the journal still
	// holds.
	URL string
	// Secrets are the one or two halves of CELLA_EVENTS_SECRET, in the
	// order the variable lists them. Two are sent while a secret is being
	// rotated.
	Secrets []string
	// Timeout bounds one delivery attempt and RetryWindow how long a record
	// is retried before it is dropped.
	Timeout     time.Duration
	RetryWindow time.Duration
}

// Enabled reports whether cellad delivers.
func (e Events) Enabled() bool { return e.URL != "" }

// loadEvents reads the sink's variables. A URL without a secret is a
// start-up failure because an unsigned record is a record from anywhere; a
// secret without a URL is one too, because that deployment believes it
// delivers and does not.
func (c *Config) loadEvents(getenv Getenv, problems *[]string) {
	raw := strings.TrimSpace(getenv("CELLA_EVENTS_URL"))
	for part := range strings.SplitSeq(getenv("CELLA_EVENTS_SECRET"), ",") {
		if secret := strings.TrimSpace(part); secret != "" {
			c.Events.Secrets = append(c.Events.Secrets, secret)
		}
	}
	c.Events.Timeout = duration(getenv, "CELLA_EVENTS_TIMEOUT", events.DefaultTimeout, problems)
	c.Events.RetryWindow = duration(getenv, "CELLA_EVENTS_RETRY_WINDOW", events.DefaultRetryWindow, problems)
	switch {
	case raw == "" && len(c.Events.Secrets) > 0:
		*problems = append(*problems, "CELLA_EVENTS_SECRET is set without CELLA_EVENTS_URL; nothing is delivered")
		return
	case raw == "":
		return
	case len(c.Events.Secrets) == 0:
		*problems = append(*problems, "CELLA_EVENTS_URL is set without CELLA_EVENTS_SECRET; a record nobody signed is a record from anywhere")
		return
	}
	if problem := checkSink(raw, getenv("CELLA_EVENTS_INSECURE_SINK")); problem != "" {
		*problems = append(*problems, problem)
		return
	}
	c.Events.URL = raw
	if c.Events.Timeout <= 0 || c.Events.Timeout > time.Minute {
		*problems = append(*problems, fmt.Sprintf("CELLA_EVENTS_TIMEOUT is %s; between 1s and 1m", c.Events.Timeout))
	}
	if c.Events.RetryWindow < time.Minute {
		*problems = append(*problems, fmt.Sprintf("CELLA_EVENTS_RETRY_WINDOW is %s; at least 1m", c.Events.RetryWindow))
	}
}

// checkSink holds the URL to the rule of spec 013: https, or http on
// loopback, or http anywhere with the escape hatch the stubs set.
func checkSink(raw, insecure string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "CELLA_EVENTS_URL must be an http:// or https:// URL naming a host"
	}
	if parsed.Scheme == "https" || loopback(parsed.Host) {
		return ""
	}
	if admitted, _ := strconv.ParseBool(strings.TrimSpace(insecure)); admitted {
		return ""
	}
	return "CELLA_EVENTS_URL is http:// on a host that is not loopback; use https:// or set CELLA_EVENTS_INSECURE_SINK=1"
}

// loopback reports whether a host:port names this machine, which is where an
// unencrypted sink is nobody else's to read.
func loopback(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(name, "[]"))
	return ip != nil && ip.IsLoopback()
}
