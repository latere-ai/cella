// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"strings"
	"sync"
	"testing"
)

// identity fills the variables spec 006 makes required, so a test about
// a listener is not also a test about the issuers. A value the caller
// set is kept.
func identity(t *testing.T, m map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{
		"CELLA_OIDC_ISSUERS": "https://login.example.com",
		"CELLA_PUBLIC_URL":   "https://cella.example.com",
		"CELLA_TOKEN_KEY":    signingKeyPEM(t, 1),
	}
	maps.Copy(out, m)
	return out
}

// signingKeys holds the generated keys, because a 2048-bit RSA key costs
// more than every assertion in this package together.
var signingKeys struct {
	sync.Mutex
	keys []*rsa.PrivateKey
}

// signingKey is the nth generated key, counting from one.
func signingKey(t *testing.T, n int) *rsa.PrivateKey {
	t.Helper()
	signingKeys.Lock()
	defer signingKeys.Unlock()
	for len(signingKeys.keys) < n {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		signingKeys.keys = append(signingKeys.keys, key)
	}
	return signingKeys.keys[n-1]
}

// signingKeyPEM renders the first n generated keys as one PEM value,
// which is what CELLA_TOKEN_KEY carries.
func signingKeyPEM(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signingKey(t, i))}
		if err := pem.Encode(&b, block); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}
