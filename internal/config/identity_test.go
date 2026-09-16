// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"
)

// identity fills the three variables spec 006 makes required, so a test
// about one variable is not also a test about the other two. A value the
// caller set is kept.
func identity(t *testing.T, m map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{
		"CELLA_OIDC_ISSUERS": "https://login.example.com",
		"CELLA_PUBLIC_URL":   "https://cella.example.com",
		"CELLA_TOKEN_KEY":    testKeyPEM(t, 1),
	}
	maps.Copy(out, m)
	return out
}

// testKeys holds the generated keys, because a 2048-bit RSA key costs
// more than every assertion in this package together.
var testKeys struct {
	sync.Mutex
	keys []*rsa.PrivateKey
}

// testKey is the nth generated key, counting from one.
func testKey(t *testing.T, n int) *rsa.PrivateKey {
	t.Helper()
	testKeys.Lock()
	defer testKeys.Unlock()
	for len(testKeys.keys) < n {
		key, err := rsa.GenerateKey(rand.Reader, TokenKeyBits)
		if err != nil {
			t.Fatal(err)
		}
		testKeys.keys = append(testKeys.keys, key)
	}
	return testKeys.keys[n-1]
}

// testKeyPEM renders the first n generated keys as one PEM value, which
// is what CELLA_TOKEN_KEY carries.
func testKeyPEM(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey(t, i))}
		if err := pem.Encode(&b, block); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// TestInsecureAndIncompleteEndpoints is spec 006's row: a non-loopback
// http:// issuer or authorizer is refused at start unless the issuer is
// listed insecure, and an authorizer URL without a token is a start-up
// failure.
func TestInsecureAndIncompleteEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{{
		name: "an http issuer on a public host is refused",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "http://login.example.com"},
		want: "CELLA_OIDC_ISSUERS http://login.example.com is http:// on a host other than loopback",
	}, {
		name: "an http issuer listed insecure is accepted",
		env: map[string]string{
			"CELLA_OIDC_ISSUERS":          "http://login.example.com",
			"CELLA_OIDC_INSECURE_ISSUERS": "http://login.example.com",
		},
	}, {
		name: "an http issuer on loopback is accepted with no list",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "http://127.0.0.1:8080"},
	}, {
		name: "an http issuer on localhost is accepted with no list",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "http://localhost:8080"},
	}, {
		name: "an http authorizer on a public host is refused",
		env: map[string]string{
			"CELLA_AUTHORIZER_URL":   "http://authz.example.com/decide",
			"CELLA_AUTHORIZER_TOKEN": "t",
		},
		want: "CELLA_AUTHORIZER_URL is http:// on a host other than loopback",
	}, {
		name: "an http authorizer on loopback is accepted",
		env: map[string]string{
			"CELLA_AUTHORIZER_URL":   "http://127.0.0.1:9090/decide",
			"CELLA_AUTHORIZER_TOKEN": "t",
		},
	}, {
		name: "an authorizer without a bearer is a start-up failure",
		env:  map[string]string{"CELLA_AUTHORIZER_URL": "https://authz.example.com/decide"},
		want: "CELLA_AUTHORIZER_TOKEN is unset while CELLA_AUTHORIZER_URL is set",
	}, {
		name: "a bearer without an authorizer selects the owner policy and is no failure",
		env:  map[string]string{"CELLA_AUTHORIZER_TOKEN": "t"},
	}, {
		name: "an issuer that is no URL is refused",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "login.example.com"},
		want: `CELLA_OIDC_ISSUERS entry "login.example.com" is not an absolute http:// or https:// URL`,
	}, {
		name: "an issuer listed twice is refused",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "https://login.example.com,https://login.example.com/"},
		want: "CELLA_OIDC_ISSUERS lists https://login.example.com twice",
	}, {
		name: "an admin subject that is not rendered is refused",
		env:  map[string]string{"CELLA_ADMIN_SUBJECTS": "alice"},
		want: `CELLA_ADMIN_SUBJECTS entry "alice" is not a rendered subject`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(identity(t, tc.env)))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() refused a sound configuration: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("Load() accepted the configuration; it should have reported %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a message carrying %q", err, tc.want)
			}
		})
	}
}

// TestIdentityAppliesEveryDefault: the four optional figures of spec 006
// fall back to the values spec 002's table names.
func TestIdentityAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(identity(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDCAudience != DefaultOIDCAudience {
		t.Errorf("OIDCAudience = %q, want %q", c.OIDCAudience, DefaultOIDCAudience)
	}
	if c.AuthorizerTimeout != 5*time.Second || c.AuthorizerCache != 60*time.Second {
		t.Errorf("timeout %v cache %v, want 5s and 60s", c.AuthorizerTimeout, c.AuthorizerCache)
	}
	if c.EnvironmentKeyTTL != 8760*time.Hour {
		t.Errorf("EnvironmentKeyTTL = %v, want 8760h", c.EnvironmentKeyTTL)
	}
	if c.DefaultEnvironment != DefaultEnvironment {
		t.Errorf("DefaultEnvironment = %q, want %q", c.DefaultEnvironment, DefaultEnvironment)
	}
}

// TestIdentityReadsEveryVariable: each variable of spec 006 reaches its
// field, and an issuer's trailing slash is removed so that one issuer
// written two ways is one issuer.
func TestIdentityReadsEveryVariable(t *testing.T) {
	c, err := Load(env(identity(t, map[string]string{
		"CELLA_OIDC_ISSUERS":        "https://a.example.com/, https://b.example.com",
		"CELLA_OIDC_AUDIENCE":       "cella-prod",
		"CELLA_PUBLIC_URL":          "https://cella.example.com/",
		"CELLA_AUTHORIZER_URL":      "https://authz.example.com/decide",
		"CELLA_AUTHORIZER_TOKEN":    "bearer-value",
		"CELLA_AUTHORIZER_TIMEOUT":  "2s",
		"CELLA_AUTHORIZER_CACHE":    "90s",
		"CELLA_ENVIRONMENT_KEY_TTL": "72h",
		"CELLA_ADMIN_SUBJECTS":      "https://a.example.com|alice, https://b.example.com|bob",
		"CELLA_DEFAULT_ENVIRONMENT": "shared",
		"CELLA_TOKEN_KEY":           testKeyPEM(t, 2),
	})))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://a.example.com", "https://b.example.com"}; !equal(c.OIDCIssuers, want) {
		t.Errorf("OIDCIssuers = %q, want %q", c.OIDCIssuers, want)
	}
	if c.PublicURL != "https://cella.example.com" {
		t.Errorf("PublicURL = %q; the trailing slash is not part of an issuer", c.PublicURL)
	}
	if c.OIDCAudience != "cella-prod" || c.AuthorizerURL != "https://authz.example.com/decide" || c.AuthorizerToken != "bearer-value" {
		t.Errorf("the authorizer read as %q %q %q", c.OIDCAudience, c.AuthorizerURL, c.AuthorizerToken)
	}
	if c.AuthorizerTimeout != 2*time.Second || c.AuthorizerCache != 90*time.Second || c.EnvironmentKeyTTL != 72*time.Hour {
		t.Errorf("the figures read as %v %v %v", c.AuthorizerTimeout, c.AuthorizerCache, c.EnvironmentKeyTTL)
	}
	if want := []string{"https://a.example.com|alice", "https://b.example.com|bob"}; !equal(c.AdminSubjects, want) {
		t.Errorf("AdminSubjects = %q, want %q", c.AdminSubjects, want)
	}
	if c.DefaultEnvironment != "shared" {
		t.Errorf("DefaultEnvironment = %q", c.DefaultEnvironment)
	}
	if len(c.TokenKeys) != 2 {
		t.Fatalf("TokenKeys holds %d keys, want the two blocks of the value", len(c.TokenKeys))
	}
	if !c.TokenKeys[0].Equal(testKey(t, 1)) {
		t.Error("the first key of the configuration is not the first block of the value; the first block signs")
	}
}

// TestTokenKeyRefusals: every shape of CELLA_TOKEN_KEY that cannot sign
// is named at start rather than at the first mint.
func TestTokenKeyRefusals(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}))
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
		want string
	}{
		{"unset", "", "CELLA_TOKEN_KEY is unset"},
		{"no PEM block", "not a key at all", "CELLA_TOKEN_KEY holds no PEM block that is an RSA private key"},
		{"a key below the floor", smallPEM, "is an RSA key of 1024 bits; 2048 is the smallest accepted"},
		{"three blocks", testKeyPEM(t, 3), "holds 3 keys; a rotation is one signing key and at most one predecessor"},
		{"a PKCS#8 block", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(identity(t, map[string]string{"CELLA_TOKEN_KEY": tc.key})))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() refused a key it signs with: %v", err)
			case tc.want == "":
			case err == nil || !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a message carrying %q", err, tc.want)
			}
		})
	}
}

// TestDurationsAreReadAsDurations: a figure that is no duration, and one
// that is not positive, are both named.
func TestDurationsAreReadAsDurations(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"soon", `CELLA_AUTHORIZER_TIMEOUT is "soon", not a duration such as 5s or 8760h`},
		{"0s", "CELLA_AUTHORIZER_TIMEOUT is 0s; a deadline and a lifetime are both positive"},
		{"-1s", "CELLA_AUTHORIZER_TIMEOUT is -1s; a deadline and a lifetime are both positive"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := Load(env(identity(t, map[string]string{"CELLA_AUTHORIZER_TIMEOUT": tc.value})))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() = %v, want a message carrying %q", err, tc.want)
			}
		})
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
