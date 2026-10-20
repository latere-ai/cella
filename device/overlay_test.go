// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/worker"
)

// What the plane decides is the embedder's: without a token reader no
// handed token is taken, without a platform audience no platform token is,
// a token the reader cannot read is refused, and without label keys an
// approval names no session.

func TestCredentialsWithoutAReaderTakeNoToken(t *testing.T) {
	creds := NewCredentials(CredentialsOptions{})
	token := tokenFor(AudienceCella, "alice", "", time.Now().Add(time.Hour))
	err := creds.Put(map[string]HeldToken{AudienceCella: {Token: token}})
	var ce *CredentialsError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "reads no token") {
		t.Fatalf("Put = %v, want a refusal for reading no token", err)
	}
	if creds.State().Cella != CredentialNeeded {
		t.Error("a refused token was kept")
	}
}

func TestATokenTheReaderCannotReadIsRefused(t *testing.T) {
	creds := NewCredentials(CredentialsOptions{Read: func(string) (TokenClaims, error) {
		return TokenClaims{}, errors.New("malformed")
	}})
	err := creds.Put(map[string]HeldToken{AudienceCella: {Token: "opaque"}})
	var ce *CredentialsError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "does not read: malformed") {
		t.Fatalf("Put = %v, want the reader's refusal", err)
	}
}

func TestAnEngineWithoutAPlatformAudienceTakesNoPlatformToken(t *testing.T) {
	paths := testPaths(t)
	creds := NewCredentials(CredentialsOptions{Read: readTestToken})
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Credentials: creds})
	token := tokenFor("api.example.com", "alice", "", time.Now().Add(time.Hour))
	_, err := e.PutCredentials(map[string]HeldToken{"platform": {Token: token}})
	var ce *CredentialsError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "no platform token") {
		t.Fatalf("PutCredentials = %v, want a refusal of the platform token", err)
	}
	if creds.PlatformAudience() != "" {
		t.Errorf("PlatformAudience = %q, want none", creds.PlatformAudience())
	}
}

func TestEmptyLabelKeysNameNoSession(t *testing.T) {
	c := worker.Create{
		Labels:  map[string]string{testLabels.Session: "ses_1", testLabels.Agent: "agt_1", testLabels.Trigger: "trg_1"},
		Sandbox: v1.Sandbox{Metadata: v1.Metadata{Annotations: map[string]string{testLabels.AgentName: "Site helper"}}},
	}
	a := approvalOf(c, SessionLabels{}, v1.HostInfo{}, time.Now())
	if a.Session != "" || a.AgentRef != "" || a.Trigger != "" || a.Agent != "" {
		t.Errorf("an approval with no label keys named %+v", a)
	}
	if named := approvalOf(c, testLabels, v1.HostInfo{}, time.Now()); named.Session != "ses_1" || named.Agent != "Site helper" {
		t.Errorf("an approval with the keys named %+v", named)
	}
}
