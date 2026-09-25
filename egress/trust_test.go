// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRoot is one self-signed certificate in PEM, named so a reader of a
// failure sees which one is where.
func testRoot(t *testing.T, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// names is the common name of every certificate in a PEM file, in order, and
// fails on a block that is not one.
func names(t *testing.T, file []byte) []string {
	t.Helper()
	var out []string
	for rest := file; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			if len(bytes.TrimSpace(rest)) > 0 {
				t.Fatalf("bytes after the last block: %q", rest)
			}
			return out
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("a block is no certificate: %v", err)
		}
		out = append(out, cert.Subject.CommonName)
	}
}

func TestTrustBundle(t *testing.T) {
	one, two, authority := testRoot(t, "one"), testRoot(t, "two"), testRoot(t, "authority")
	t.Run("theRootsThenTheAuthority", func(t *testing.T) {
		got := TrustBundle([]byte(one+two), authority)
		if strings.Join(names(t, got), ",") != "one,two,authority" {
			t.Fatalf("the bundle holds %v, want the roots in order and the authority last", names(t, got))
		}
		if !bytes.HasSuffix(got, []byte("\n")) {
			t.Fatal("the bundle does not end with a newline")
		}
	})
	t.Run("aBlockEndingWithoutANewlineStaysOnItsOwnLines", func(t *testing.T) {
		got := TrustBundle([]byte(strings.TrimSuffix(one, "\n")), strings.TrimSuffix(authority, "\n"))
		if strings.Join(names(t, got), ",") != "one,authority" {
			t.Fatalf("the bundle holds %v", names(t, got))
		}
		if !bytes.Contains(got, []byte("-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----")) || !bytes.HasSuffix(got, []byte("\n")) {
			t.Fatalf("the blocks share a line: %q", got)
		}
	})
	t.Run("noRootsIsTheAuthorityAlone", func(t *testing.T) {
		if got := TrustBundle(nil, authority); string(got) != authority {
			t.Fatalf("the bundle = %q, want the authority alone", got)
		}
	})
	t.Run("noAuthorityIsNothing", func(t *testing.T) {
		for _, a := range []string{"", " \n"} {
			if got := TrustBundle([]byte(one), a); got != nil {
				t.Fatalf("the bundle for authority %q = %q, want nothing", a, got)
			}
		}
	})
}

func TestLoadRoots(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	one, two := testRoot(t, "one"), testRoot(t, "two")
	system := write("system.pem", one)
	named := write("named.pem", "# a comment a distribution writes\n"+two+
		"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"+
		"-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydGlmaWNhdGU=\n-----END CERTIFICATE-----\n")
	empty := write("empty.pem", "no certificate here\n")
	missing := filepath.Join(dir, "missing.pem")
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	list := func(t *testing.T, files ...string) {
		t.Helper()
		saved := rootFiles
		rootFiles = files
		t.Cleanup(func() { rootFiles = saved })
	}

	t.Run("sslCertFileWinsOverTheList", func(t *testing.T) {
		list(t, system)
		roots, err := LoadRoots(getenv(map[string]string{"SSL_CERT_FILE": named}))
		if err != nil {
			t.Fatal(err)
		}
		if roots.Source != named || roots.Count != 1 || strings.Join(names(t, roots.PEM), ",") != "two" {
			t.Fatalf("roots = %d from %s holding %v, want the named file's one certificate", roots.Count, roots.Source, names(t, roots.PEM))
		}
		// The comment, the key and the block that does not parse are
		// dropped: the file a sandbox reads holds certificates alone.
		if bytes.Contains(roots.PEM, []byte("comment")) || bytes.Contains(roots.PEM, []byte("PRIVATE KEY")) {
			t.Fatalf("roots kept a block that is no certificate: %q", roots.PEM)
		}
	})
	t.Run("theListIsReadInOrderPastAFileWithNoCertificate", func(t *testing.T) {
		list(t, missing, empty, system, named)
		roots, err := LoadRoots(getenv(nil))
		if err != nil {
			t.Fatal(err)
		}
		if roots.Source != system || strings.Join(names(t, roots.PEM), ",") != "one" {
			t.Fatalf("roots came from %s holding %v, want the first file that holds a certificate", roots.Source, names(t, roots.PEM))
		}
	})
	t.Run("aNamedFileThatIsMissingOrEmptyIsRefused", func(t *testing.T) {
		list(t, system)
		for _, path := range []string{missing, empty} {
			_, err := LoadRoots(getenv(map[string]string{"SSL_CERT_FILE": path}))
			if err == nil || !strings.Contains(err.Error(), "SSL_CERT_FILE") || !strings.Contains(err.Error(), path) {
				t.Fatalf("SSL_CERT_FILE=%s gave %v, want a refusal naming the variable and the file", path, err)
			}
		}
	})
	t.Run("nothingFoundIsRefused", func(t *testing.T) {
		list(t, missing, empty)
		_, err := LoadRoots(getenv(nil))
		if err == nil || !strings.Contains(err.Error(), "SSL_CERT_FILE is unset") || !strings.Contains(err.Error(), empty) {
			t.Fatalf("err = %v, want a refusal naming the variable and every file tried", err)
		}
	})
	t.Run("anUnreadableFileOfTheListIsRefused", func(t *testing.T) {
		list(t, dir, system)
		if _, err := LoadRoots(getenv(nil)); err == nil || !strings.Contains(err.Error(), dir) {
			t.Fatalf("err = %v, want the file that could not be read named", err)
		}
	})
	t.Run("rootsOverTheBoundAreRefused", func(t *testing.T) {
		var big strings.Builder
		for big.Len() <= MaxRootsBytes {
			big.WriteString(one)
		}
		path := write("big.pem", big.String())
		_, err := LoadRoots(getenv(map[string]string{"SSL_CERT_FILE": path}))
		if err == nil || !strings.Contains(err.Error(), "bytes of certificates") {
			t.Fatalf("err = %v, want the bound named", err)
		}
	})
	t.Run("theDefaultListIsGosLinuxList", func(t *testing.T) {
		if rootFiles[0] != "/etc/ssl/certs/ca-certificates.crt" || rootFiles[len(rootFiles)-1] != "/etc/ssl/cert.pem" || len(rootFiles) != 6 {
			t.Fatalf("rootFiles = %v", rootFiles)
		}
	})
}
