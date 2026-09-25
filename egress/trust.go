// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// MaxRootsBytes bounds the public roots a driver writes into a sandbox. The
// k8s driver carries them in the sandbox's Secret beside the token, and a
// Secret holds at most 1 MiB; a system bundle is about 220 KB.
const MaxRootsBytes = 512 << 10

// rootFiles is where Go's crypto/x509 looks for the system roots on Linux, in
// its order. The first file that holds a certificate is the one read.
var rootFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Gentoo, distroless
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS, RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine, macOS
}

// Roots are the public roots a sandbox pointed at the gateway verifies a
// tunneled host against, as the process that builds the driver read them.
type Roots struct {
	// PEM is every certificate of the file, each re-encoded as one PEM block,
	// in the file's order.
	PEM []byte
	// Count is how many certificates PEM holds, and Source the file they
	// were read from.
	Count  int
	Source string
}

// LoadRoots reads the public roots: the file SSL_CERT_FILE names, or else the
// first file of Go's Linux list that holds a certificate. It keeps every
// CERTIFICATE block that parses as X.509 and drops every other block and every
// comment, so the file a sandbox reads holds only what this process itself
// would trust.
//
// It is an error when SSL_CERT_FILE names a file that cannot be read or holds
// no certificate, when no file of the list holds one, and when the roots
// exceed MaxRootsBytes. A caller that points sandboxes at a gateway refuses to
// start on it: a sandbox given the gateway's authority alone fails to verify
// every host the gateway tunnels.
func LoadRoots(getenv func(string) string) (Roots, error) {
	if named := strings.TrimSpace(getenv("SSL_CERT_FILE")); named != "" {
		roots, err := readRoots(named)
		if err != nil {
			return Roots{}, fmt.Errorf("SSL_CERT_FILE: %w", err)
		}
		return roots, nil
	}
	for _, path := range rootFiles {
		roots, err := readRoots(path)
		switch {
		case err == nil:
			return roots, nil
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, errNoCertificate):
			continue
		default:
			return Roots{}, err
		}
	}
	return Roots{}, fmt.Errorf("no public roots: SSL_CERT_FILE is unset and none of %s holds a certificate", strings.Join(rootFiles, ", "))
}

var errNoCertificate = errors.New("holds no certificate")

// readRoots reads one file and keeps its certificates.
func readRoots(path string) (Roots, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Roots{}, err
	}
	var out bytes.Buffer
	count := 0
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			continue
		}
		// The block is written again without its headers and without the
		// text around it, so a distribution's comments stay behind and the
		// file holds certificates alone.
		out.Write(pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes}))
		count++
	}
	if count == 0 {
		return Roots{}, fmt.Errorf("%s %w", path, errNoCertificate)
	}
	if out.Len() > MaxRootsBytes {
		return Roots{}, fmt.Errorf("%s holds %d bytes of certificates, over the %d a sandbox's trust file may carry", path, out.Len(), MaxRootsBytes)
	}
	return Roots{PEM: out.Bytes(), Count: count, Source: path}, nil
}

// TrustBundle is the file a driver writes at the path the trust variables
// name: the public roots, then the gateway's authority. The gateway
// terminates TLS only toward a host a secret is bound to and tunnels every
// other admitted host, so the workload is shown the gateway's leaf for the
// first and the upstream's own certificate for the second, and the file is
// read as the whole trust store by OpenSSL, curl, git, Python and Go.
//
// Empty roots give the authority alone, and an empty authority gives nothing:
// without an authority no trust variable is set.
func TrustBundle(roots []byte, authority string) []byte {
	if strings.TrimSpace(authority) == "" {
		return nil
	}
	out := make([]byte, 0, len(roots)+len(authority)+2)
	out = append(out, roots...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, authority...)
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out
}
