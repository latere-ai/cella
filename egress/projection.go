// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// CAPath is where a driver projects the sandbox's trust file, read-only,
// under the control plane's own prefix: the public roots and the gateway's
// certificate authority, which TrustBundle composes.
const CAPath = "/run/cella/egress-ca.pem"

// ProxyUser is the user half of the proxy credential. A stock client sends
// the URL's userinfo as Proxy-Authorization: Basic, so the user is a constant
// and the password is the sandbox's credential.
const ProxyUser = "sandbox"

// CredentialHeader is what the reverse door reads instead of the proxy
// header, because the reverse door speaks plain HTTP inside the environment
// and leaves Authorization free for the upstream's own placeholder.
const CredentialHeader = "Cella-Egress-Credential"

// DefaultProxyPort and DefaultReversePort are the ports a gateway address
// takes when it names none, and are the defaults of CELLA_EGRESS_PROXY_ADDR
// and CELLA_EGRESS_REVERSE_ADDR.
const (
	DefaultProxyPort   = 3128
	DefaultReversePort = 8080
)

// NoProxy is what a sandbox never sends through the gateway: itself. The
// control plane and DNS are admitted by the driver's own network rule and
// never routed through the gateway, so they are not here either.
const NoProxy = "127.0.0.1,localhost,::1"

// reservedEnv is every key the boundary sets inside a sandbox. A manifest
// that sets one of them is refused, because the value the workload wrote
// would be overwritten and the workload would not know.
var reservedEnv = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
	"GIT_SSL_CAINFO", "CURL_CA_BUNDLE",
	"CELLA_GATEWAY_URL", "CELLA_GATEWAY_CREDENTIAL",
}

// ReservedEnv returns the keys the boundary sets inside a sandbox, sorted.
// The manifest package refuses the same set in spec.env, and a test holds the
// two copies equal.
func ReservedEnv() []string {
	out := slices.Clone(reservedEnv)
	slices.Sort(out)
	return out
}

// Projection is what a driver needs to point one sandbox at its gateway: the
// address of each door, the sandbox's own credential, and where the driver
// put the certificate authority inside the sandbox.
type Projection struct {
	// ProxyAddr is the host or host:port of the proxy door, which is the
	// environment's spec.gateway.
	ProxyAddr string
	// ReverseAddr is the host or host:port of the reverse door. It is empty
	// on an installation that runs only the proxy door, and the two reverse
	// variables are then not set.
	ReverseAddr string
	// Credential is what both doors authenticate.
	Credential string
	// CAPath is where the driver projected the trust file inside the
	// sandbox. It is empty when the driver could not project a file, and the
	// trust variables are then not set.
	CAPath string
}

// Env is the environment the driver adds to the workload's own. It is empty
// when there is no gateway to point at, so a driver applies the result
// unconditionally and a sandbox on an installation with no gateway is
// unchanged.
func (p Projection) Env() map[string]string {
	if p.ProxyAddr == "" || p.Credential == "" {
		return nil
	}
	proxy := (&url.URL{
		Scheme: "http",
		User:   url.UserPassword(ProxyUser, p.Credential),
		Host:   withPort(p.ProxyAddr, DefaultProxyPort),
	}).String()
	env := map[string]string{
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "NO_PROXY": NoProxy,
		"http_proxy": proxy, "https_proxy": proxy, "no_proxy": NoProxy,
	}
	if p.CAPath != "" {
		for _, key := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
			env[key] = p.CAPath
		}
	}
	if p.ReverseAddr != "" {
		env["CELLA_GATEWAY_URL"] = "http://" + withPort(p.ReverseAddr, DefaultReversePort)
		env["CELLA_GATEWAY_CREDENTIAL"] = p.Credential
	}
	return env
}

// withPort gives an address a port when it carries none, and accepts an
// address written as a URL, because an operator who set the variable to
// "http://gateway:3128" meant the same host.
func withPort(addr string, def int) string {
	addr = strings.TrimSpace(addr)
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		addr = u.Host
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(def))
}
