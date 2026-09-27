// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The bounds of spec 076's two durations when their variables set nothing,
// and the most either may take: a hold or a handoff past it is an outage
// waited out rather than bridged.
const (
	DefaultForwardHold    = 15 * time.Second
	DefaultHandoffTimeout = 10 * time.Second
	MaxReplicaWait        = 5 * time.Minute
)

// Replicas is spec 076's half: where the other replicas reach this process's
// public listener, how long a standby holds a request while no writer can be
// reached, and how long a writer handing off waits for the requests it is
// still answering.
type Replicas struct {
	// AdvertiseURL is CELLA_ADVERTISE_URL, an http or https URL with a host
	// and no path, such as http://10.0.0.7:8080. Empty, this process never
	// forwards: it waits for the writer lease and then serves, which is a
	// deployment of one replica.
	AdvertiseURL string
	// ForwardHold is CELLA_FORWARD_HOLD and HandoffTimeout
	// CELLA_HANDOFF_TIMEOUT.
	ForwardHold    time.Duration
	HandoffTimeout time.Duration
}

// loadReplicas reads the three variables. An advertised address needs the
// durable store, since the lease that names the writer is a row in it.
func loadReplicas(getenv Getenv, durable bool, problems *[]string) Replicas {
	r := Replicas{
		ForwardHold:    replicaWait(getenv, "CELLA_FORWARD_HOLD", DefaultForwardHold, problems),
		HandoffTimeout: replicaWait(getenv, "CELLA_HANDOFF_TIMEOUT", DefaultHandoffTimeout, problems),
	}
	raw := strings.TrimSpace(getenv("CELLA_ADVERTISE_URL"))
	if raw == "" {
		return r
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "":
		*problems = append(*problems, "CELLA_ADVERTISE_URL is "+strconv.Quote(raw)+"; an http or https URL naming a host")
		return r
	case strings.Trim(u.Path, "/") != "", u.RawQuery != "", u.Fragment != "", u.User != nil:
		*problems = append(*problems, "CELLA_ADVERTISE_URL is "+strconv.Quote(raw)+"; a scheme and a host with no path, query or credential, since a forwarded request keeps its own path")
		return r
	case !durable:
		*problems = append(*problems, "CELLA_ADVERTISE_URL needs CELLA_DB_URL: the writer lease that names where to forward is a row in the store")
		return r
	}
	r.AdvertiseURL = strings.TrimSuffix(raw, "/")
	return r
}

// replicaWait reads one of the two durations, positive and at most
// MaxReplicaWait.
func replicaWait(getenv Getenv, name string, def time.Duration, problems *[]string) time.Duration {
	d := duration(getenv, name, def, problems)
	if d > MaxReplicaWait {
		*problems = append(*problems, fmt.Sprintf("%s is %s; at most %s", name, d, MaxReplicaWait))
		return def
	}
	return d
}
