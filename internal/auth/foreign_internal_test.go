// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIssuerDocumentIsReadLeniently: a discovery document with a duplicate
// member, a member in another letter case and a string that is not valid
// UTF-8 is read as its issuer's other clients read it.
func TestIssuerDocumentIsReadLeniently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"https://old.example","Issuer":"https://a` + "\xff" + `.example","JWKS_URI":"https://a.example/jwks"}`))
	}))
	defer srv.Close()
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := fetchJSON(context.Background(), srv.Client(), srv.URL, time.Second, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Issuer != "https://a�.example" || doc.JWKSURI != "https://a.example/jwks" {
		t.Errorf("doc = %+v", doc)
	}
}
