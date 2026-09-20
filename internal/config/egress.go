// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults of the egress role and of the control plane's half of it.
const (
	DefaultEgressProxyAddr   = ":3128"
	DefaultEgressReverseAddr = ":8080"
	DefaultEgressAckTimeout  = 5 * time.Second
	DefaultEgressRecordsCap  = 1000
	// MaxEgressAckTimeout bounds how long a create may wait for a gateway.
	// A create that waits longer than this is a create that has failed.
	MaxEgressAckTimeout = time.Minute
)

// EgressGateway is the control plane's half: where sandboxes reach the
// gateway, how long a create waits for one, and how many records are kept.
type EgressGateway struct {
	// ProxyAddr and ReverseAddr are what a sandbox of the default
	// environment dials, from CELLA_GATEWAY and CELLA_GATEWAY_REVERSE. They
	// are addresses on the sandbox's own network, not listen addresses, and
	// unset means this installation runs no such door.
	ProxyAddr   string
	ReverseAddr string
	// AckTimeout is how long a create waits for one gateway of the
	// environment to acknowledge the sandbox's map.
	AckTimeout time.Duration
	// RecordsCap is how many connection records are kept per sandbox.
	RecordsCap int
}

// EgressConfig is the egress role's own configuration. It reads none of the
// control plane's variables: the role holds no store, no issuer and no
// authorizer, and connects outbound with one key.
type EgressConfig struct {
	// URL is the control plane's public URL, which the gateway opens its
	// one stream to.
	URL string
	// EnvironmentKey authenticates that stream and names the environment.
	EnvironmentKey string
	// ProxyAddr and ReverseAddr are the two doors' listen addresses.
	ProxyAddr   string
	ReverseAddr string
	// CAKeyPEM is the private key of the authority the gateway terminates
	// TLS with. Unset generates one at start, which is what a single
	// gateway wants; an environment with several sets the same key on each,
	// so every sandbox trusts every one of them.
	CAKeyPEM string
	// CABundlePEM are authorities the gateway trusts beside the system
	// roots when it dials an upstream. It is unset in an installation and
	// set by the test tiers to their own upstream's.
	CABundlePEM string
	// Insecure admits a control plane URL that is http:// on a host other
	// than loopback. The stubs set it; nothing else should.
	Insecure bool
}

// LoadEgress reads the egress role's configuration, or returns one error
// naming every problem, sorted, so an operator fixes a deployment in one
// round.
func LoadEgress(getenv Getenv) (EgressConfig, error) {
	c := EgressConfig{
		URL:            strings.TrimSpace(getenv("CELLA_URL")),
		EnvironmentKey: strings.TrimSpace(getenv("CELLA_ENVIRONMENT_KEY")),
		ProxyAddr:      withDefault(getenv("CELLA_EGRESS_PROXY_ADDR"), DefaultEgressProxyAddr),
		ReverseAddr:    withDefault(getenv("CELLA_EGRESS_REVERSE_ADDR"), DefaultEgressReverseAddr),
		CAKeyPEM:       getenv("CELLA_EGRESS_CA_KEY"),
		CABundlePEM:    getenv("CELLA_EGRESS_CA_BUNDLE"),
	}
	var problems []string
	if raw := strings.TrimSpace(getenv("CELLA_INSECURE_CONTROL_PLANE")); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			problems = append(problems, "CELLA_INSECURE_CONTROL_PLANE must be true or false")
		}
		c.Insecure = value
	}
	if c.URL == "" {
		problems = append(problems, "CELLA_URL is unset, and the gateway connects outbound to it")
	} else if err := checkControlPlaneURL(c.URL, c.Insecure); err != nil {
		problems = append(problems, "CELLA_URL "+err.Error())
	}
	if c.EnvironmentKey == "" {
		problems = append(problems, "CELLA_ENVIRONMENT_KEY is unset, and it is what authenticates the gateway's stream")
	}
	if err := checkAddr(c.ProxyAddr); err != nil {
		problems = append(problems, "CELLA_EGRESS_PROXY_ADDR "+err.Error())
	}
	if err := checkAddr(c.ReverseAddr); err != nil {
		problems = append(problems, "CELLA_EGRESS_REVERSE_ADDR "+err.Error())
	}
	if sameEndpoint(c.ProxyAddr, c.ReverseAddr) {
		problems = append(problems, "CELLA_EGRESS_REVERSE_ADDR must differ from CELLA_EGRESS_PROXY_ADDR; both are "+c.ProxyAddr)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return EgressConfig{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// checkControlPlaneURL holds a role's control plane URL to what a credential
// may be sent over: https, or http on loopback, or http anywhere with the
// operator's explicit consent.
func checkControlPlaneURL(raw string, insecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("is %q, not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if insecure || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("is http:// on a host that is not loopback; the environment key would travel in the clear, and CELLA_INSECURE_CONTROL_PLANE=1 is the explicit consent")
	default:
		return fmt.Errorf("has the scheme %q; the control plane is reached over http or https", u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// loadEgressGateway reads the control plane's half. It never fails on an
// unset address: an installation that runs no gateway is one where every
// sandbox's boundary is open, and that is a valid installation.
func loadEgressGateway(getenv Getenv, problems *[]string) EgressGateway {
	g := EgressGateway{
		ProxyAddr:   strings.TrimSpace(getenv("CELLA_GATEWAY")),
		ReverseAddr: strings.TrimSpace(getenv("CELLA_GATEWAY_REVERSE")),
		AckTimeout:  duration(getenv, "CELLA_EGRESS_ACK_TIMEOUT", DefaultEgressAckTimeout, problems),
		RecordsCap:  DefaultEgressRecordsCap,
	}
	if g.AckTimeout <= 0 || g.AckTimeout > MaxEgressAckTimeout {
		*problems = append(*problems, fmt.Sprintf("CELLA_EGRESS_ACK_TIMEOUT is %s; between 1ms and %s", g.AckTimeout, MaxEgressAckTimeout))
		g.AckTimeout = DefaultEgressAckTimeout
	}
	if raw := strings.TrimSpace(getenv("CELLA_EGRESS_RECORDS_CAP")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			*problems = append(*problems, "CELLA_EGRESS_RECORDS_CAP must be a positive count of records per sandbox")
		} else {
			g.RecordsCap = value
		}
	}
	if g.ReverseAddr != "" && g.ProxyAddr == "" {
		*problems = append(*problems, "CELLA_GATEWAY_REVERSE is set without CELLA_GATEWAY; a sandbox reaches the reverse door only where it reaches the gateway")
	}
	return g
}
