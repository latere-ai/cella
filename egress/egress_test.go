// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// sandbox is a resolved sandbox with a boundary and an id, which is what
// Compile is given: the controller assigns the id and the credential before
// the first map is compiled.
func sandbox(e v1.Egress) v1.Sandbox {
	return v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       "Sandbox",
		Metadata:   v1.Metadata{Name: "work"},
		Spec:       v1.SandboxSpec{Network: v1.Network{Egress: e}},
		Status: v1.SandboxStatus{
			ID:          "sbx_01j9zk2p7q8r9s0t1u2v3w4x5y",
			EgressState: &v1.EgressState{Credential: "abcdef", Version: 7},
		},
	}
}

// view is a secret view with a freshly minted placeholder.
func view(name string, hosts ...string) SecretView {
	return SecretView{Name: name, Placeholder: MintPlaceholder(), Hosts: hosts}
}

func compile(t *testing.T, sb v1.Sandbox, secrets ...SecretView) Map {
	t.Helper()
	m, err := Compile(sb, secrets)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return m
}

func TestCompileCarriesTheBoundary(t *testing.T) {
	m := compile(t, sandbox(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"B.example.com.", "a.example.com", "a.example.com"}}))
	if m.Principal != "sandbox:sbx_01j9zk2p7q8r9s0t1u2v3w4x5y" {
		t.Fatalf("principal = %q", m.Principal)
	}
	if m.Credential != "abcdef" || m.Version != 7 {
		t.Fatalf("credential = %q, version = %d, want the sandbox's own", m.Credential, m.Version)
	}
	// The list is normalized and deduped, so the map is a set and the
	// gateway matches the same spelling the resolver accepted.
	if !slices.Equal(m.Allow, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("allow = %v", m.Allow)
	}
	if len(m.Deny) != 0 || len(m.Entries) != 0 {
		t.Fatalf("map = %+v, want no deny list and no entry", m)
	}
}

// TestCompileIsDeterministic is what lets the control plane recompile a
// running sandbox: the same inputs must produce the same map, whatever order
// the caller wrote the secrets in.
func TestCompileIsDeterministic(t *testing.T) {
	sb := sandbox(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}})
	one := view("one", "one.example.com")
	two := view("two", "two.example.com")
	first := compile(t, sb, one, two)
	second := compile(t, sb, two, one)
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("two orders compiled differently:\n%s\n%s", a, b)
	}
}

func TestCompileJoinsASecretsScopeToTheAllowList(t *testing.T) {
	sb := sandbox(v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"pypi.example.org"}})
	m := compile(t, sb, SecretView{
		Name: "api-token", Placeholder: MintPlaceholder(),
		Hosts:  []string{"API.example.com", "*.cdn.example.com"},
		Inject: Inject{Header: "Authorization", Scheme: SchemeBearer},
	})
	if !slices.Equal(m.Allow, []string{"*.cdn.example.com", "api.example.com", "pypi.example.org"}) {
		t.Fatalf("allow = %v, want the boundary and the secret's scope", m.Allow)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %+v, want one", m.Entries)
	}
	e := m.Entries[0]
	if e.Secret != "api-token" || e.Header != "Authorization" || e.Scheme != SchemeBearer {
		t.Fatalf("entry = %+v", e)
	}
	if !slices.Equal(e.Ports, []int{DefaultPort}) {
		t.Fatalf("ports = %v, want the default", e.Ports)
	}
	if len(m.NotInjectable) != 0 {
		t.Fatalf("notInjectable = %v, want none", m.NotInjectable)
	}
	// No entry carries a value and no field could hold one.
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "value") || strings.Contains(string(body), "secretValue") {
		t.Fatalf("the map's wire form has a field for a value: %s", body)
	}
	// NotInjectable is the control plane's and never travels.
	if strings.Contains(string(body), "notInjectable") {
		t.Fatalf("notInjectable reached the wire: %s", body)
	}
}

func TestCompileDropsASecretTheBoundaryDenies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		e     v1.Egress
		hosts []string
		want  []string // the entry's hosts, nil when the secret is not injectable
	}{
		{"noneInjectsNothing", v1.Egress{Mode: v1.EgressNone}, []string{"api.example.com"}, nil},
		{"deniedExactly", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"api.example.com"}}, []string{"api.example.com"}, nil},
		{"deniedByAWildcard", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"*.example.com"}}, []string{"api.example.com"}, nil},
		{"aDeniedHostBeatsAWiderScope", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"api.example.com"}}, []string{"*.example.com"}, nil},
		{"oneHostSurvives", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"api.example.com"}}, []string{"api.example.com", "cdn.example.net"}, []string{"cdn.example.net"}},
		{"nothingDenied", v1.Egress{Mode: v1.EgressOpen}, []string{"api.example.com"}, []string{"api.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := compile(t, sandbox(tc.e), view("token", tc.hosts...))
			if tc.want == nil {
				if len(m.Entries) != 0 {
					t.Fatalf("entries = %+v, want none", m.Entries)
				}
				if !slices.Equal(m.NotInjectable, []string{"token"}) {
					t.Fatalf("notInjectable = %v, want the secret named", m.NotInjectable)
				}
				return
			}
			if len(m.Entries) != 1 || !slices.Equal(m.Entries[0].Hosts, tc.want) {
				t.Fatalf("entries = %+v, want hosts %v", m.Entries, tc.want)
			}
			if len(m.NotInjectable) != 0 {
				t.Fatalf("notInjectable = %v, want none", m.NotInjectable)
			}
		})
	}
}

func TestCompileRefusals(t *testing.T) {
	allow := v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"a.example.com"}}
	for _, tc := range []struct {
		name    string
		sb      v1.Sandbox
		secrets []SecretView
		want    error
	}{
		{"noID", func() v1.Sandbox { sb := sandbox(allow); sb.Status.ID = ""; return sb }(), nil, ErrNoPrincipal},
		{"noMode", sandbox(v1.Egress{}), nil, ErrNoMode},
		{"unknownMode", sandbox(v1.Egress{Mode: "everything"}), nil, ErrNoMode},
		{"emptyPlaceholder", sandbox(allow), []SecretView{{Name: "token", Hosts: []string{"a.example.com"}}}, ErrPlaceholder},
		{"aPlaceholderThatIsNotOne", sandbox(allow), []SecretView{{Name: "token", Placeholder: "ghp_secret", Hosts: []string{"a.example.com"}}}, ErrPlaceholder},
		{"twoSecretsOnOneHost", sandbox(allow), []SecretView{view("one", "api.example.com"), view("two", "api.example.com")}, ErrSecretHostConflict},
		{"twoSecretsOneUnderTheOthersWildcard", sandbox(allow), []SecretView{view("one", "*.example.com"), view("two", "api.example.com")}, ErrSecretHostConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.sb, tc.secrets)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compile = %v, want %v", err, tc.want)
			}
		})
	}
	// Two scopes that cannot meet are two secrets, not a conflict.
	compile(t, sandbox(allow), view("one", "*.a.example.com"), view("two", "*.b.example.com"))
}

func TestAdmits(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    Map
		host string
		want bool
	}{
		{"noneRefusesEverything", Map{Mode: v1.EgressNone}, "api.example.com", false},
		{"allowlistAdmitsAListedHost", Map{Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}}, "api.example.com", true},
		{"allowlistRefusesAnythingElse", Map{Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}}, "other.example.com", false},
		{"allowlistWildcard", Map{Mode: v1.EgressAllowlist, Allow: []string{"*.example.com"}}, "api.example.com", true},
		{"allowlistWildcardRefusesTheApex", Map{Mode: v1.EgressAllowlist, Allow: []string{"*.example.com"}}, "example.com", false},
		{"openAdmits", Map{Mode: v1.EgressOpen}, "anything.example.com", true},
		{"openRefusesADeniedHost", Map{Mode: v1.EgressOpen, Deny: []string{"api.example.com"}}, "api.example.com", false},
		{"openRefusesUnderADeniedWildcard", Map{Mode: v1.EgressOpen, Deny: []string{"*.example.com"}}, "api.example.com", false},
		{"caseAndTrailingDotDoNotMatter", Map{Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}}, "API.example.com.", true},
		{"anEmptyMapAdmitsNothing", Map{}, "api.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Admits(tc.host); got != tc.want {
				t.Fatalf("Admits(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestEntryFor(t *testing.T) {
	m := Map{Mode: v1.EgressOpen, Entries: []Entry{
		{Secret: "one", Hosts: []string{"api.example.com"}, Ports: []int{443}},
		{Secret: "two", Hosts: []string{"*.cdn.example.net"}, Ports: []int{443, 8443}},
	}}
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"api.example.com", 443, "one"},
		{"api.example.com", 8443, ""},
		{"a.cdn.example.net", 8443, "two"},
		{"cdn.example.net", 443, ""},
		{"other.example.com", 443, ""},
	} {
		got := m.EntryFor(tc.host, tc.port)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("EntryFor(%q, %d) = %q, want none", tc.host, tc.port, got.Secret)
		case tc.want != "" && (got == nil || got.Secret != tc.want):
			t.Errorf("EntryFor(%q, %d) = %v, want %q", tc.host, tc.port, got, tc.want)
		}
	}
	// An entry with no port list is not scoped by port.
	any := Map{Entries: []Entry{{Secret: "any", Hosts: []string{"api.example.com"}}}}
	if any.EntryFor("api.example.com", 9999) == nil {
		t.Error("an entry with no port list refused a port")
	}
}

func TestPrincipalRoundTrip(t *testing.T) {
	if got := SandboxOf(Principal("sbx_1")); got != "sbx_1" {
		t.Fatalf("SandboxOf(Principal()) = %q", got)
	}
	if got := SandboxOf("environment:env_1"); got != "" {
		t.Fatalf("SandboxOf of a foreign principal = %q, want empty", got)
	}
}

func TestMintCredential(t *testing.T) {
	first, err := MintCredential()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MintCredential()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two credentials are equal")
	}
	if len(first) != 52 {
		t.Fatalf("credential is %d characters, want 52 from 32 bytes", len(first))
	}
	if strings.ToLower(first) != first || strings.ContainsAny(first, ":/@=+ ") {
		t.Fatalf("credential %q needs escaping in a proxy URL", first)
	}
	// A source of randomness that fails is a refusal, never a weak credential.
	restore := randomBytes
	randomBytes = func([]byte) error { return errors.New("no entropy") }
	defer func() { randomBytes = restore }()
	if _, err = MintCredential(); err == nil {
		t.Fatal("a failed source of randomness minted a credential")
	}
}

func TestMintPlaceholderIsPerSandbox(t *testing.T) {
	first, second := MintPlaceholder(), MintPlaceholder()
	if first == second {
		t.Fatal("two sandboxes mounting one secret got one placeholder")
	}
	if !IsPlaceholder(first) || IsPlaceholder("ghp_value") {
		t.Fatal("the placeholder shape is not recognised")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for _, f := range []Frame{
		{Type: FrameHello, Hello: &Hello{Protocol: Protocol, GatewayID: "gw-1", Versions: map[string]int64{"sandbox:sbx_1": 2}}},
		{Type: FrameSnapshot, Snapshot: &Snapshot{Maps: []Map{{Principal: "sandbox:sbx_1", Version: 2}}}},
		{Type: FramePut, Put: &Map{Principal: "sandbox:sbx_1", Version: 3}},
		{Type: FramePurge, Purge: &Purge{Principal: "sandbox:sbx_1"}},
		{Type: FrameAck, Ack: &Ack{Principal: "sandbox:sbx_1", Version: 3}},
		{Type: FrameHeartbeat},
		{Type: FrameRecord, Record: &Record{Principal: "sandbox:sbx_1", Decision: DecisionAllowed}},
	} {
		line, err := Encode(f)
		if err != nil {
			t.Fatalf("Encode(%s): %v", f.Type, err)
		}
		if !strings.HasSuffix(string(line), "\n") || strings.Count(string(line), "\n") != 1 {
			t.Fatalf("Encode(%s) is not one line: %q", f.Type, line)
		}
		got, err := Decode(line)
		if err != nil {
			t.Fatalf("Decode(%s): %v", f.Type, err)
		}
		if got.Type != f.Type {
			t.Fatalf("type = %q, want %q", got.Type, f.Type)
		}
	}
	if _, err := Decode([]byte(`{"type":"shutdown"}`)); err == nil {
		t.Fatal("an unknown frame type was accepted")
	}
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Fatal("a line that is not JSON was accepted")
	}
}

func TestRecordNormalize(t *testing.T) {
	t.Run("aQueryStringIsCutAndAClockIsFilled", func(t *testing.T) {
		r := Record{Principal: "sandbox:sbx_1", Path: "/repos/example?token=cph_x"}
		if err := r.Normalize(); err != nil {
			t.Fatal(err)
		}
		if r.Path != "/repos/example" {
			t.Fatalf("path = %q, want the query string cut", r.Path)
		}
		if r.At.IsZero() || r.At.Location() != time.UTC {
			t.Fatalf("at = %v, want a UTC instant", r.At)
		}
	})
	t.Run("aLongPathIsCut", func(t *testing.T) {
		r := Record{Principal: "sandbox:sbx_1", Path: strings.Repeat("a", maxRecordPathBytes*2)}
		if err := r.Normalize(); err != nil {
			t.Fatal(err)
		}
		if len(r.Path) != maxRecordPathBytes {
			t.Fatalf("path is %d bytes, want it cut to %d", len(r.Path), maxRecordPathBytes)
		}
	})
	t.Run("aRecordThatCarriesAPlaceholderIsRefused", func(t *testing.T) {
		for _, r := range []Record{
			{Principal: "sandbox:sbx_1", Path: MintPlaceholder()},
			{Principal: "sandbox:sbx_1", Host: MintPlaceholder()},
			{Principal: "sandbox:sbx_1", Substituted: []string{MintPlaceholder()}},
		} {
			if err := r.Normalize(); err == nil {
				t.Fatalf("a record carrying a placeholder was accepted: %+v", r)
			}
		}
	})
	t.Run("aRecordWithNoSandboxIsRefused", func(t *testing.T) {
		r := Record{Principal: "environment:env_1"}
		if err := r.Normalize(); !errors.Is(err, ErrRecordPrincipal) {
			t.Fatalf("Normalize = %v, want %v", err, ErrRecordPrincipal)
		}
	})
}

func TestProjectionEnv(t *testing.T) {
	t.Run("everyDoorAndEveryTrustVariable", func(t *testing.T) {
		env := Projection{ProxyAddr: "gateway.example.internal", ReverseAddr: "gateway.example.internal:8080", Credential: "abc", CAPath: CAPath}.Env()
		want := "http://sandbox:abc@gateway.example.internal:3128"
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if env[key] != want {
				t.Fatalf("%s = %q, want %q", key, env[key], want)
			}
		}
		if env["NO_PROXY"] != NoProxy || env["no_proxy"] != NoProxy {
			t.Fatalf("no proxy = %q", env["NO_PROXY"])
		}
		for _, key := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
			if env[key] != CAPath {
				t.Fatalf("%s = %q, want the projected authority", key, env[key])
			}
		}
		if env["CELLA_GATEWAY_URL"] != "http://gateway.example.internal:8080" || env["CELLA_GATEWAY_CREDENTIAL"] != "abc" {
			t.Fatalf("reverse door = %q, %q", env["CELLA_GATEWAY_URL"], env["CELLA_GATEWAY_CREDENTIAL"])
		}
		// Every key the projection sets is one a manifest may not set.
		reserved := ReservedEnv()
		for key := range env {
			if !slices.Contains(reserved, key) {
				t.Fatalf("the projection sets %q, which is not reserved", key)
			}
		}
	})
	t.Run("whatIsAbsentIsNotSet", func(t *testing.T) {
		env := Projection{ProxyAddr: "gateway.example.internal:3128", Credential: "abc"}.Env()
		for _, key := range []string{"SSL_CERT_FILE", "CELLA_GATEWAY_URL", "CELLA_GATEWAY_CREDENTIAL"} {
			if _, ok := env[key]; ok {
				t.Fatalf("%s was set with no authority and no reverse door", key)
			}
		}
		if env["HTTPS_PROXY"] != "http://sandbox:abc@gateway.example.internal:3128" {
			t.Fatalf("proxy = %q", env["HTTPS_PROXY"])
		}
	})
	t.Run("noGatewayIsNoEnvironment", func(t *testing.T) {
		for _, p := range []Projection{{}, {ProxyAddr: "gateway.example.internal"}, {Credential: "abc"}} {
			if env := p.Env(); len(env) != 0 {
				t.Fatalf("Env() = %v, want nothing without a gateway and a credential", env)
			}
		}
	})
	t.Run("anAddressWrittenAsAURL", func(t *testing.T) {
		env := Projection{ProxyAddr: "http://gateway.example.internal:3129", Credential: "abc"}.Env()
		if env["HTTPS_PROXY"] != "http://sandbox:abc@gateway.example.internal:3129" {
			t.Fatalf("proxy = %q", env["HTTPS_PROXY"])
		}
	})
}
