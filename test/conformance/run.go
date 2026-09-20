// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Run executes the suite and mirrors every result onto a subtest named
// <NNN>/<Name>, so a Go test run reads as the report does: a pass is a pass,
// a skip carries its reason, a declared gap is a skip naming the
// declaration, and a failure carries the exchange that disagreed.
func Run(t *testing.T, cfg Config) Report {
	t.Helper()
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatalf("the suite could not start: %v", err)
	}
	t.Log("\n" + report.String())
	for _, res := range report.Results {
		t.Run(res.Subtest(), func(t *testing.T) {
			switch res.Status {
			case StatusSkipped:
				t.Skip(res.Reason)
			case StatusKnown:
				t.Skipf("declared gap: %s\n%v", res.Reason, res.Err)
			case StatusFailed:
				t.Error(res.Err)
			case StatusPassed:
			}
		})
	}
	for _, name := range report.Undeclared {
		t.Errorf("%s is declared as a gap and passed; the declaration has outlived the gap and is removed", name)
	}
	return report
}

// Minter takes a subject's token from an issuer with a mint route, which is
// what a tier points the suite's Token at: POST <issuer>/mint with the
// subject, and the token in the answer.
func Minter(issuer string) func(context.Context, string) (string, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	client := &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}
	return func(ctx context.Context, subject string) (string, error) {
		body, err := json.Marshal(map[string]string{"sub": subject})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/mint", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("minting a token for %s: %w", subject, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return "", fmt.Errorf("the issuer answered %d minting a token for %s", resp.StatusCode, subject)
		}
		var minted struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
			return "", fmt.Errorf("the issuer's answer for %s is no token: %w", subject, err)
		}
		if minted.Token == "" {
			return "", fmt.Errorf("the issuer minted nothing for %s", subject)
		}
		return minted.Token, nil
	}
}

// Declaration is a server's statement of which cases it fails and why. It is
// read from a file so the statement is reviewed like any other change, and
// the suite refuses a declaration that names a case it does not hold.
type Declaration struct {
	// Note says what the declaration is for, for a person reading the file.
	Note string `json:"note"`
	// Cases maps a case name to the reason this server fails it.
	Cases map[string]string `json:"cases"`
}

// LoadDeclaration reads a declaration file. An empty path is no declaration.
func LoadDeclaration(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the declared gaps: %w", err)
	}
	var declaration Declaration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declaration); err != nil {
		return nil, fmt.Errorf("%s is no declaration: %w", path, err)
	}
	names := Names()
	for name := range declaration.Cases {
		if !slices.Contains(names, name) {
			return nil, fmt.Errorf("%s declares %s, which is no case of this suite", path, name)
		}
	}
	for name, reason := range declaration.Cases {
		if reason == "" {
			return nil, fmt.Errorf("%s declares %s with no reason", path, name)
		}
	}
	return declaration.Cases, nil
}
