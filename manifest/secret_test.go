// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// secretManifest is the smallest Secret this contract accepts: a name, a
// scope and a value.
func secretManifest(name string, hosts ...string) v1.Secret {
	if len(hosts) == 0 {
		hosts = []string{"api.example.com"}
	}
	return v1.Secret{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSecret,
		Metadata:   v1.Metadata{Name: name},
		Spec: v1.SecretSpec{
			Scope: v1.SecretScope{Hosts: hosts},
			Value: "the-value",
		},
	}
}

func resolveSecret(t *testing.T, in v1.Secret, o SecretOptions) v1.Secret {
	t.Helper()
	out, err := ResolveSecret(&in, o)
	if err != nil {
		t.Fatalf("ResolveSecret: %v", err)
	}
	return *out
}

func secretRefusal(t *testing.T, in v1.Secret, o SecretOptions) *Error {
	t.Helper()
	_, err := ResolveSecret(&in, o)
	var known *Error
	if !errors.As(err, &known) {
		t.Fatalf("ResolveSecret: got %v, want a manifest error", err)
	}
	return known
}

// TestSecretDefaults holds the three defaults the contract names: the static
// kind, port 443, and the Authorization header with the bearer scheme.
func TestSecretDefaults(t *testing.T) {
	got := resolveSecret(t, secretManifest("github"), SecretOptions{})
	want := v1.SecretSpec{
		Kind:   v1.SecretStatic,
		Scope:  v1.SecretScope{Hosts: []string{"api.example.com"}, Ports: []int{443}},
		Inject: v1.SecretInject{Header: "Authorization", Scheme: v1.SchemeBearer},
		Value:  "the-value",
	}
	if got.Spec.Kind != want.Kind {
		t.Fatalf("kind = %q", got.Spec.Kind)
	}
	if !slices.Equal(got.Spec.Scope.Ports, want.Scope.Ports) {
		t.Fatalf("ports = %v", got.Spec.Scope.Ports)
	}
	if got.Spec.Inject != want.Inject {
		t.Fatalf("inject = %+v, want %+v", got.Spec.Inject, want.Inject)
	}

	// A secret that injects elsewhere is raw by default: the value is what
	// goes in, with no word around it.
	elsewhere := secretManifest("api-key")
	elsewhere.Spec.Inject.Header = "X-Api-Key"
	if got := resolveSecret(t, elsewhere, SecretOptions{}); got.Spec.Inject.Scheme != v1.SchemeRaw {
		t.Fatalf("scheme = %q, want raw", got.Spec.Inject.Scheme)
	}
	// So is one that injects into a query parameter.
	query := secretManifest("api-key")
	query.Spec.Inject.Query = "api_key"
	got = resolveSecret(t, query, SecretOptions{})
	if got.Spec.Inject.Scheme != v1.SchemeRaw || got.Spec.Inject.Header != "" {
		t.Fatalf("inject = %+v, want the query parameter alone and the raw scheme", got.Spec.Inject)
	}
}

// TestSecretRefusals drives one case per rule of the contract's table.
func TestSecretRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		path string
		with func(*v1.Secret)
	}{
		{"an unknown kind", "invalid_field", pathSecretKind, func(s *v1.Secret) { s.Spec.Kind = "vault" }},
		{"no scope", "missing_field", pathSecretHosts, func(s *v1.Secret) { s.Spec.Scope.Hosts = nil }},
		{"an address for a host", "invalid_field", pathSecretHosts + "[0]", func(s *v1.Secret) {
			s.Spec.Scope.Hosts = []string{"10.0.0.1"}
		}},
		{"a single label", "invalid_field", pathSecretHosts + "[0]", func(s *v1.Secret) {
			s.Spec.Scope.Hosts = []string{"intranet"}
		}},
		{"the machine it runs on", "invalid_field", pathSecretHosts + "[0]", func(s *v1.Secret) {
			s.Spec.Scope.Hosts = []string{"api.localhost"}
		}},
		{"a host with a port", "invalid_field", pathSecretHosts + "[0]", func(s *v1.Secret) {
			s.Spec.Scope.Hosts = []string{"api.example.com:443"}
		}},
		{"a port out of range", "invalid_field", pathSecretPorts + "[0]", func(s *v1.Secret) {
			s.Spec.Scope.Ports = []int{70000}
		}},
		{"both injection places", "exclusive_fields", pathSecretHeader, func(s *v1.Secret) {
			s.Spec.Inject.Header, s.Spec.Inject.Query = "Authorization", "api_key"
		}},
		{"a header name that is not one", "invalid_field", pathSecretHeader, func(s *v1.Secret) {
			s.Spec.Inject.Header = "X Api Key"
		}},
		{"a framing header", "invalid_field", pathSecretHeader, func(s *v1.Secret) {
			s.Spec.Inject.Header = "Content-Length"
		}},
		{"a forwarded header", "invalid_field", pathSecretHeader, func(s *v1.Secret) {
			s.Spec.Inject.Header = "X-Forwarded-For"
		}},
		{"a query name that is not one", "invalid_field", pathSecretQuery, func(s *v1.Secret) {
			s.Spec.Inject.Query = "api key"
		}},
		{"an unknown scheme", "invalid_field", pathSecretScheme, func(s *v1.Secret) {
			s.Spec.Inject.Scheme = "digest"
		}},
		{"oauth on a static secret", "exclusive_fields", pathSecretKind, func(s *v1.Secret) {
			s.Spec.OAuth = &v1.SecretOAuth{TokenURL: "https://login.example.com/token"}
		}},
		{"an oauth secret with no endpoint", "missing_field", pathSecretOAuth, func(s *v1.Secret) {
			s.Spec.Kind = v1.SecretOAuthClientCredentials
			s.Spec.Value = "id:secret"
		}},
		{"a token endpoint that is not https", "invalid_field", pathSecretTokenURL, func(s *v1.Secret) {
			s.Spec.Kind = v1.SecretOAuthClientCredentials
			s.Spec.Value = "id:secret"
			s.Spec.OAuth = &v1.SecretOAuth{TokenURL: "http://login.example.com/token"}
		}},
		{"no value", "missing_field", pathSecretValue, func(s *v1.Secret) { s.Spec.Value = "" }},
		{"a value with a line break", "invalid_field", pathSecretValue, func(s *v1.Secret) {
			s.Spec.Value = "token\r\nX-Injected: 1"
		}},
		{"a value past the bound", "invalid_field", pathSecretValue, func(s *v1.Secret) {
			s.Spec.Value = strings.Repeat("x", MaxSecretValueBytes+1)
		}},
		{"a basic value with no colon", "invalid_field", pathSecretValue, func(s *v1.Secret) {
			s.Spec.Inject.Scheme = v1.SchemeBasic
			s.Spec.Value = "onlyauser"
		}},
		{"a basic value with an empty half", "invalid_field", pathSecretValue, func(s *v1.Secret) {
			s.Spec.Inject.Scheme = v1.SchemeBasic
			s.Spec.Value = "user:"
		}},
		{"an oauth value with no colon", "invalid_field", pathSecretValue, func(s *v1.Secret) {
			s.Spec.Kind = v1.SecretOAuthClientCredentials
			s.Spec.OAuth = &v1.SecretOAuth{TokenURL: "https://login.example.com/token"}
			s.Spec.Value = "clientonly"
		}},
		{"no name", "missing_field", "metadata.name", func(s *v1.Secret) { s.Metadata.Name = "" }},
		{"a name that is not a label", "invalid_field", "metadata.name", func(s *v1.Secret) {
			s.Metadata.Name = "Not A Label"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := secretManifest("github")
			tc.with(&in)
			got := secretRefusal(t, in, SecretOptions{})
			if got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("got %s at %s, want %s at %s", got.Code, got.Path, tc.code, tc.path)
			}
		})
	}
}

// TestSecretVersionRules holds the one field a read never returns: required
// on create, optional on update, and stripped from every answer.
func TestSecretVersionRules(t *testing.T) {
	existing := resolveSecret(t, secretManifest("github"), SecretOptions{})
	without := secretManifest("github")
	without.Spec.Value = ""
	if got := resolveSecret(t, without, SecretOptions{Existing: &existing}); got.Spec.Value != "" {
		t.Fatalf("value = %q, want none carried on an update that set none", got.Spec.Value)
	}
	if got := StripSecretValue(existing); got.Spec.Value != "" {
		t.Fatalf("StripSecretValue left %q", got.Spec.Value)
	}
	// Stripping copies: the object handed in is unchanged.
	if existing.Spec.Value == "" {
		t.Fatal("StripSecretValue emptied the object it was given")
	}
	renamed := secretManifest("other")
	if got := secretRefusal(t, renamed, SecretOptions{Existing: &existing}); got.Code != "immutable_field" {
		t.Fatalf("code = %q, want immutable_field", got.Code)
	}
}

// TestResolveSecretIsDeterministic holds the promise every resolver in this
// package makes: one input, one output, byte for byte.
func TestResolveSecretIsDeterministic(t *testing.T) {
	in := secretManifest("github", "*.example.com")
	in.Spec.Kind = v1.SecretOAuthClientCredentials
	in.Spec.OAuth = &v1.SecretOAuth{TokenURL: "https://login.example.com/token", Scope: "read"}
	in.Spec.Value = "id:secret"
	first, second := resolveSecret(t, in, SecretOptions{}), resolveSecret(t, in, SecretOptions{})
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("two resolves differ:\n%s\n%s", a, b)
	}
	// The input is untouched: the scope it was given is still its own.
	in.Spec.Scope.Hosts[0] = "other.example.com"
	if first.Spec.Scope.Hosts[0] != "*.example.com" {
		t.Fatal("ResolveSecret aliased the manifest it was given")
	}
	if _, err := ResolveSecret(nil, SecretOptions{}); err == nil {
		t.Fatal("ResolveSecret(nil) returned no error")
	}
}

func TestDecodeSecret(t *testing.T) {
	body, err := json.Marshal(secretManifest("github"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeSecret(body, "application/json"); err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	for _, tc := range []struct {
		name, body, contentType, code string
	}{
		{"a content type this server does not read", `{}`, "application/yaml", "unsupported_media_type"},
		{"a content type that is not one", `{}`, "not a type", "unsupported_media_type"},
		{"a field the schema does not have", `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","nope":1}`, "application/json", "unknown_field"},
		{"a body that is not JSON", `{`, "application/json", "bad_request"},
		{"two documents", `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret"}{}`, "application/json", "multi_document"},
		{"another version", `{"apiVersion":"cella.latere.ai/v2","kind":"Secret"}`, "application/json", "unsupported_version"},
		{"another kind", `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox"}`, "application/json", "unsupported_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeSecret([]byte(tc.body), tc.contentType)
			var known *Error
			if !errors.As(err, &known) || known.Code != tc.code {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
		})
	}
	// Status on apply is ignored rather than refused, so a caller may GET,
	// edit and PUT without stripping it.
	withStatus := `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"github"},"status":{"id":"sec_1","version":9}}`
	got, err := DecodeSecret([]byte(withStatus), "application/json")
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	if got.Status != (v1.SecretStatus{}) {
		t.Fatalf("status = %+v, want the zero status", got.Status)
	}
	if _, err = ResolveSecret(&v1.Secret{Kind: v1.KindSecret}, SecretOptions{}); err == nil {
		t.Fatal("a manifest with no apiVersion resolved")
	}
	if _, err = ResolveSecret(&v1.Secret{APIVersion: v1.APIVersion}, SecretOptions{}); err == nil {
		t.Fatal("a manifest with no kind resolved")
	}
}

func TestCompanionEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject v1.SecretInject
		want   map[string]string
	}{
		{"the header a client already uses needs no companion", v1.SecretInject{Header: "Authorization"}, nil},
		{"another header is named", v1.SecretInject{Header: "X-Api-Key"}, map[string]string{"TOKEN_HEADER": "X-Api-Key"}},
		{"a query parameter is named", v1.SecretInject{Query: "api_key"}, map[string]string{"TOKEN_QUERY": "api_key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompanionEnv("TOKEN", tc.inject)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// secretsLookup answers a fixed set of secrets by name and by id, which is
// what the API's own lookup does over the store.
type secretsLookup struct {
	secrets []v1.Secret
	err     error
}

func (s secretsLookup) Secret(_ context.Context, nameOrID string) (*v1.Secret, error) {
	if s.err != nil {
		return nil, s.err
	}
	for i := range s.secrets {
		if s.secrets[i].Metadata.Name == nameOrID || s.secrets[i].Status.ID == nameOrID {
			out := s.secrets[i]
			return &out, nil
		}
	}
	return nil, ErrNotFound
}

func withSecrets(o Options, secrets ...v1.Secret) Options {
	o.Lookup = WithSecrets(o.Lookup, secretsLookup{secrets: secrets}.Secret)
	return o
}

func mounted(name, env string) v1.SecretMount { return v1.SecretMount{Name: name, Env: env} }

// scoped is a resolved Secret with an id, as the store holds one.
func scoped(t *testing.T, name, id string, hosts ...string) v1.Secret {
	t.Helper()
	out := resolveSecret(t, secretManifest(name, hosts...), SecretOptions{})
	out.Status.ID = id
	out.Spec.Value = ""
	return out
}

// TestSecretReferences is stage 4 over the mounts: the objects are found, the
// refusals are the contract's, and the environment keys do not collide.
func TestSecretReferences(t *testing.T) {
	github := scoped(t, "github", "sec_1", "api.github.com")
	openai := scoped(t, "openai", "sec_2", "api.openai.com")

	t.Run("two secrets resolve and come back in order", func(t *testing.T) {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN"), mounted("sec_2", "OPENAI_API_KEY")}
		got := resolve(t, obj, withSecrets(containerOptions(), github, openai))
		if len(got.Secrets) != 2 || got.Secrets[0].Metadata.Name != "github" || got.Secrets[1].Metadata.Name != "openai" {
			t.Fatalf("secrets = %+v", got.Secrets)
		}
		// The mount is not written into the boundary: the allow list the
		// caller wrote is the allow list it reads back, and the join
		// happens where the map is compiled.
		if len(got.Sandbox.Spec.Network.Egress.AllowedHosts) != 0 {
			t.Fatalf("allowedHosts = %v, want the caller's own", got.Sandbox.Spec.Network.Egress.AllowedHosts)
		}
	})

	for _, tc := range []struct {
		name    string
		code    string
		path    string
		mounts  []v1.SecretMount
		env     map[string]string
		secrets []v1.Secret
	}{
		{
			name: "a secret that does not exist", code: "not_found", path: pathSecretAt(0, "name"),
			mounts: []v1.SecretMount{mounted("absent", "TOKEN")}, secrets: []v1.Secret{github},
		},
		{
			name: "no name", code: "missing_field", path: pathSecretAt(0, "name"),
			mounts: []v1.SecretMount{mounted("  ", "TOKEN")}, secrets: []v1.Secret{github},
		},
		{
			name: "no environment key", code: "missing_field", path: pathSecretAt(0, "env"),
			mounts: []v1.SecretMount{mounted("github", "")}, secrets: []v1.Secret{github},
		},
		{
			name: "a key that is not a POSIX name", code: "invalid_field", path: pathSecretAt(0, "env"),
			mounts: []v1.SecretMount{mounted("github", "not-a-name")}, secrets: []v1.Secret{github},
		},
		{
			name: "a key the boundary owns", code: "reserved_prefix", path: pathSecretAt(0, "env"),
			mounts: []v1.SecretMount{mounted("github", "HTTPS_PROXY")}, secrets: []v1.Secret{github},
		},
		{
			name: "a key spec.env already has", code: "invalid_field", path: pathSecretAt(0, "env"),
			mounts: []v1.SecretMount{mounted("github", "TOKEN")}, env: map[string]string{"TOKEN": "x"},
			secrets: []v1.Secret{github},
		},
		{
			name: "a key another mount has", code: "invalid_field", path: pathSecretAt(1, "env"),
			mounts:  []v1.SecretMount{mounted("github", "TOKEN"), mounted("openai", "TOKEN")},
			secrets: []v1.Secret{github, openai},
		},
		{
			name: "a key that is another mount's header companion", code: "invalid_field", path: pathSecretAt(1, "env"),
			mounts:  []v1.SecretMount{mounted("github", "TOKEN"), mounted("openai", "TOKEN_HEADER")},
			secrets: []v1.Secret{github, openai},
		},
		{
			name: "a key that is another mount's query companion", code: "invalid_field", path: pathSecretAt(1, "env"),
			mounts:  []v1.SecretMount{mounted("github", "TOKEN"), mounted("openai", "TOKEN_QUERY")},
			secrets: []v1.Secret{github, openai},
		},
		{
			name: "a companion spec.env already has", code: "invalid_field", path: pathSecretAt(0, "env"),
			mounts: []v1.SecretMount{mounted("github", "TOKEN")}, env: map[string]string{"TOKEN_HEADER": "x"},
			secrets: []v1.Secret{github},
		},
		{
			name: "two secrets on one host", code: "secret_host_conflict", path: pathSecretAt(0, "name"),
			mounts: []v1.SecretMount{mounted("github", "A"), mounted("other", "B")},
			secrets: []v1.Secret{
				github,
				scoped(t, "other", "sec_3", "api.github.com"),
			},
		},
		{
			name: "a wildcard that covers another secret's host", code: "secret_host_conflict", path: pathSecretAt(0, "name"),
			mounts: []v1.SecretMount{mounted("wide", "A"), mounted("narrow", "B")},
			secrets: []v1.Secret{
				scoped(t, "wide", "sec_4", "*.example.com"),
				scoped(t, "narrow", "sec_5", "api.example.com"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := sandbox()
			obj.Spec.Secrets = tc.mounts
			obj.Spec.Env = tc.env
			got := refusal(t, obj, withSecrets(containerOptions(), tc.secrets...))
			if got.Code != tc.code || got.Path != tc.path {
				t.Fatalf("got %s at %s, want %s at %s", got.Code, got.Path, tc.code, tc.path)
			}
		})
	}

	t.Run("a lookup that cannot decide", func(t *testing.T) {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "TOKEN")}
		o := containerOptions()
		o.Lookup = WithSecrets(o.Lookup, secretsLookup{err: errors.New("the authorizer is down")}.Secret)
		if got := refusal(t, obj, o); got.Code != "authorizer_unavailable" {
			t.Fatalf("code = %q, want authorizer_unavailable", got.Code)
		}
	})

	t.Run("a lookup that refuses in the contract's own words", func(t *testing.T) {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "TOKEN")}
		o := containerOptions()
		o.Lookup = WithSecrets(o.Lookup, secretsLookup{err: failAt("forbidden", "x", "no")}.Secret)
		if got := refusal(t, obj, o); got.Code != "forbidden" {
			t.Fatalf("code = %q, want forbidden", got.Code)
		}
	})

	t.Run("a lookup with no secret half", func(t *testing.T) {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "TOKEN")}
		if got := refusal(t, obj, containerOptions()); got.Code != "not_found" {
			t.Fatalf("code = %q, want not_found", got.Code)
		}
		if got := refusal(t, obj, withSecrets(containerOptions())); got.Code != "not_found" {
			t.Fatalf("code = %q, want not_found", got.Code)
		}
	})

	t.Run("a lookup that answers neither an object nor an error", func(t *testing.T) {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "TOKEN")}
		o := containerOptions()
		o.Lookup = WithSecrets(o.Lookup, func(context.Context, string) (*v1.Secret, error) { return nil, nil })
		if got := refusal(t, obj, o); got.Code != "not_found" {
			t.Fatalf("code = %q, want not_found", got.Code)
		}
	})
}

// TestModeInferenceFromASecret is the row the Secret kind adds to the
// inference table: a mount with no host list of its own asks for allowlist,
// because a sandbox that reaches everything has no boundary for the secret's
// scope to sit inside.
func TestModeInferenceFromASecret(t *testing.T) {
	github := scoped(t, "github", "sec_1", "api.github.com")
	obj := sandbox()
	obj.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN")}
	if got := resolve(t, obj, withSecrets(containerOptions(), github)).Sandbox; got.Spec.Network.Egress.Mode != v1.EgressAllowlist {
		t.Fatalf("mode = %q, want allowlist", got.Spec.Network.Egress.Mode)
	}
	// A denied list still asks for open, mount or no mount: the caller said
	// what to keep out rather than what to let through.
	denied := obj
	denied.Spec.Network.Egress.DeniedHosts = []string{"metrics.example.com"}
	if got := resolve(t, denied, withSecrets(containerOptions(), github)).Sandbox; got.Spec.Network.Egress.Mode != v1.EgressOpen {
		t.Fatalf("mode = %q, want open", got.Spec.Network.Egress.Mode)
	}
}

// TestAWorkloadCannotMountASecret is the mounts half of the narrowing rule.
func TestAWorkloadCannotMountASecret(t *testing.T) {
	github := scoped(t, "github", "sec_1", "api.github.com")
	openai := scoped(t, "openai", "sec_2", "api.openai.com")
	existing := resolve(t, func() v1.Sandbox {
		obj := sandbox()
		obj.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN")}
		return obj
	}(), withSecrets(containerOptions(), github)).Sandbox

	both := sandbox()
	both.Spec.Secrets = []v1.SecretMount{mounted("github", "GITHUB_TOKEN"), mounted("openai", "OPENAI_API_KEY")}

	o := withSecrets(containerOptions(), github, openai)
	o.Existing = &existing
	o.Actor = Actor{Subject: "sandbox:sbx_1", Workload: true}
	got := refusal(t, both, o)
	if got.Code != "boundary_widened" || !slices.Contains(got.Paths, pathSecrets) {
		t.Fatalf("got %s at %v, want boundary_widened at %s", got.Code, got.Paths, pathSecrets)
	}

	// Dropping one is narrowing and is accepted. The mode is named, because
	// a manifest with no secret and no host list infers open, and moving from
	// allowlist to open is the widening this rule refuses.
	fewer := sandbox()
	fewer.Spec.Network.Egress.Mode = v1.EgressAllowlist
	resolve(t, fewer, o)
	owner := o
	owner.Actor = Actor{Subject: "https://login.example.com|alice"}
	resolve(t, both, owner)
}
