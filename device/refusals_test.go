// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellaruntime "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// The refusals an embedder meets at the edges: a body the control API cannot
// read, a token missing what a device needs, a policy the driver cannot
// serve, and a Cella that cannot be reached.

func TestControlRefusesABodyItCannotRead(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths}})
	h := e.ControlHandler()
	for _, route := range []string{"POST /v1/connect", "PUT /v1/credentials", "PUT /v1/policy", "POST /v1/remove"} {
		method, path, _ := strings.Cut(route, " ")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with a broken body answered %d, want 400", route, rec.Code)
		}
	}
}

func TestControlAnswersAPolicyFileItCannotRead(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.PolicyFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PolicyFile(), []byte("folders: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}})
	rec := httptest.NewRecorder()
	e.ControlHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/policy", nil))
	if rec.Code < http.StatusBadRequest {
		t.Errorf("a broken policy file answered %d", rec.Code)
	}
}

func TestControlTellsAnOrganizationsToken(t *testing.T) {
	rec := httptest.NewRecorder()
	writeControlError(rec, &CredentialsError{Code: CodeCredentialsOrg, Reason: "the context is org_1"})
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || body.Error.Code != CodeCredentialsOrg {
		t.Errorf("an organization's token answered %d %q", rec.Code, body.Error.Code)
	}
}

func TestCheckTokenRefusals(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	reads := func(c TokenClaims) TokenReader {
		return func(string) (TokenClaims, error) { return c, nil }
	}
	for _, tc := range []struct {
		name  string
		token HeldToken
		read  TokenReader
		want  string
	}{
		{"an empty token", HeldToken{Token: "  "}, reads(TokenClaims{}), "is empty"},
		{"no person", HeldToken{Token: "t"}, reads(TokenClaims{Audiences: []string{AudienceCella}, Personal: true}), "names no person"},
		{"no expiry", HeldToken{Token: "t"}, reads(TokenClaims{Audiences: []string{AudienceCella}, Subject: "alice", Personal: true}), "names no expiry"},
		{"expiring within a minute", HeldToken{Token: "t", ExpiresAt: now.Add(30 * time.Second)}, reads(TokenClaims{Audiences: []string{AudienceCella}, Subject: "alice", Personal: true}), "within a minute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := checkToken(AudienceCella, tc.token, now, tc.read)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkToken = %v, want it to say %q", err, tc.want)
			}
		})
	}
	expiry, subject, err := checkToken(AudienceCella, HeldToken{Token: "t", ExpiresAt: now.Add(time.Hour)},
		now, reads(TokenClaims{Audiences: []string{AudienceCella}, Subject: "alice", Personal: true, Expiry: now.Add(30 * time.Minute)}))
	if err != nil || subject != "alice" || !expiry.Equal(now.Add(30*time.Minute)) {
		t.Errorf("checkToken = %v, %q, %v; want the token's own earlier expiry", expiry, subject, err)
	}
}

func TestAPolicyTheDriverCannotServeIsRefused(t *testing.T) {
	paths := testPaths(t)
	project := filepath.Join(paths.Home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	e := testEngine(Config{
		Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths},
		NewDriver: func(local.Options) (cellaruntime.Driver, error) { return nil, errors.New("no sandbox runtime here") },
	})
	terms := Terms{Name: "Desk", SessionStart: SessionStartAllow, Folders: []Folder{{Path: project, Write: true}}, Network: Network{Mode: NetworkNone}}
	if _, err := e.Connect(t.Context(), terms, "any"); CodeOf(err) != CodePolicyInvalid || !strings.Contains(err.Error(), "no sandbox runtime") {
		t.Errorf("Connect = %v, want %s with the driver's refusal", err, CodePolicyInvalid)
	}
}

func TestAChangeTheDriverCannotServeIsRefused(t *testing.T) {
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	e := testEngine(Config{
		Paths: paths, Keys: keys, Account: &fakeAccount{exists: true}, Enroll: &fakeEnroller{paths: paths},
		NewDriver: func(local.Options) (cellaruntime.Driver, error) { return nil, errors.New("no sandbox runtime here") },
	})
	none := Network{Mode: NetworkNone}
	if _, err := e.PutPolicy(t.Context(), PolicyChange{Network: &none}); CodeOf(err) != CodePolicyInvalid {
		t.Errorf("PutPolicy = %v, want %s", err, CodePolicyInvalid)
	}
}

func TestAnUnreachableCella(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	cella, err := client.New(client.Config{URL: srv.URL, Token: client.StaticToken("person-token")})
	if err != nil {
		t.Fatal(err)
	}
	account := NewCellaAccount(cella)
	if ok, err := account.Exists(t.Context(), "dev-1"); ok || err == nil || errors.Is(err, ErrRemoved) {
		t.Errorf("Exists over an unreachable Cella = %v, %v; want the transport's error", ok, err)
	}
	e := NewEnrollment(EnrollmentOptions{Paths: testPaths(t), Cella: cella, Platform: &fakePlatform{}, Name: func() string { return "Test Mac" }})
	if _, err := e.Connect(t.Context(), Policy{Name: "Desk"}); CodeOf(err) != CodeRefused {
		t.Errorf("Connect over an unreachable Cella = %v, want %s", err, CodeRefused)
	}
}

func TestAPlaneWithNoLabelsStillLabelsTheDevice(t *testing.T) {
	cella := &stubCella{}
	e, _ := enrollmentOn(t, cella, &fakePlatform{})
	if _, err := e.Connect(t.Context(), Policy{Name: "Desk"}); err != nil {
		t.Fatal(err)
	}
	if len(cella.created) != 1 || cella.created[0].Metadata.Labels["example.com/kind"] != "device" {
		t.Errorf("the environment was created as %+v, want the embedder's labels", cella.created)
	}
}

func TestARefusalWithNoDetailSaysWhatCellaAnswered(t *testing.T) {
	cella := &stubCella{}
	cella.refuseWith(http.StatusBadRequest, "invalid", "")
	e, _ := enrollmentOn(t, cella, &fakePlatform{})
	_, err := e.Connect(t.Context(), Policy{Name: "Desk"})
	if CodeOf(err) != CodeRefused || !strings.Contains(err.Error(), "refused") {
		t.Errorf("Connect = %v, want %s with Cella's own message", err, CodeRefused)
	}
}

func TestAnAnswerToNothingWaiting(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}})
	if err := e.Answer("apr_missing", AnswerAllow, "test"); !errors.Is(err, ErrNotPending) {
		t.Errorf("Answer = %v, want %v", err, ErrNotPending)
	}
}

func TestFilesThatDoNotRead(t *testing.T) {
	paths := testPaths(t)
	broken := filepath.Join(paths.Home, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwner(broken, testEnv); err == nil {
		t.Error("readOwner of a broken record answered no error")
	}
	if _, err := readOwner(paths.Home, testEnv); err == nil {
		t.Error("readOwner of a directory answered no error")
	}
	if _, _, err := (KeyFile{Path: paths.Home}).Get(testEnv); err == nil {
		t.Error("a keys file that is a directory answered no error")
	}
	if err := os.MkdirAll(filepath.Dir(paths.PolicyFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PolicyFile(), []byte("folders: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Forget(OpenKeys(paths.KeyFile()), paths.PolicyFile(), testEnv, false); err == nil {
		t.Error("Forget over a broken policy file answered no error")
	}
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths}})
	if _, err := e.Connect(t.Context(), Terms{Name: "Desk"}, "any"); err == nil {
		t.Error("Connect over a broken policy file answered no error")
	}
}

func TestAProposalOfAFolderThatNamesNone(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths}})
	if _, err := e.ProposeConnect(ConnectQuery{Folders: []string{":ro"}}); CodeOf(err) != CodePolicyInvalid {
		t.Errorf("ProposeConnect = %v, want %s", err, CodePolicyInvalid)
	}
}

func TestCleanNameKeepsTheLongestNameThePlatformTakes(t *testing.T) {
	if got := CleanName(strings.Repeat("a", MaxNameLength+10)); len(got) != MaxNameLength {
		t.Errorf("CleanName kept %d characters, want %d", len(got), MaxNameLength)
	}
}

func TestCredentialsOfAnotherPersonAreDroppedForTheOwner(t *testing.T) {
	c := NewCredentials(testCredentials(time.Second))
	if err := c.Put(map[string]HeldToken{}); err != nil {
		t.Fatalf("Put of nothing = %v", err)
	}
	if err := c.Put(map[string]HeldToken{AudienceCella: {Token: tokenFor(AudienceCella, "bob", "", time.Now().Add(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	c.SetOwner("alice")
	if c.Subject() != "" {
		t.Errorf("bob's tokens outlived alice confirmed as the owner; held for %q", c.Subject())
	}
}

func TestAComputerNotConnectedRefusesChanges(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{
		Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths},
		Credentials: NewCredentials(testCredentials(time.Second)),
	})
	name := "Desk"
	if _, err := e.PutPolicy(t.Context(), PolicyChange{Name: &name}); err == nil {
		t.Error("PutPolicy of a computer not connected answered no error")
	}
	if err := e.ForgetRemembered("trg_1"); err == nil {
		t.Error("ForgetRemembered of a computer not connected answered no error")
	}
	if _, err := e.PutCredentials(map[string]HeldToken{"other": {Token: "t"}}); err == nil || !strings.Contains(err.Error(), "names no audience") {
		t.Errorf("PutCredentials for no audience = %v", err)
	}
}

func TestTheContextStepKeepsThePlanesRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"the plane's own refusal", Fail(CodeNoSignIn, "no saved sign-in", nil), CodeNoSignIn},
		{"a token not handed", ErrCredentialsNeeded, CodeCredentialsNeeded},
		{"anything else", errors.New("the plane is down"), CodeRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := enrollmentOn(t, &stubCella{}, &fakePlatform{contextErr: tc.err})
			if _, err := e.Connect(t.Context(), Policy{Name: "Desk"}); CodeOf(err) != tc.want {
				t.Errorf("Connect = %v, want %s", err, tc.want)
			}
		})
	}
}
