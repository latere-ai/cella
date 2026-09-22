// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// MaxSecretValueBytes is the largest value a Secret carries. A credential is
// a token, not a payload; the bound is what keeps one object from becoming a
// store of arbitrary bytes the gateway would hold in memory per sandbox.
const MaxSecretValueBytes = 64 << 10

var (
	headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)
	queryNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// unsubstitutableHeaders are the framing and hop-by-hop headers the
// substitution engine never rewrites. A secret aimed at one of them could
// never be substituted, so the object is refused rather than accepted and
// silently inert.
var unsubstitutableHeaders = []string{
	"host", "content-length", "transfer-encoding", "te", "trailer",
	"connection", "keep-alive", "upgrade", "proxy-connection", "proxy-authorization",
}

// The JSON paths of the Secret's fields, named once so a refusal and a test
// point at the same string.
const (
	pathSecretKind     = "spec.kind"
	pathSecretHosts    = "spec.scope.hosts"
	pathSecretPorts    = "spec.scope.ports"
	pathSecretHeader   = "spec.inject.header"
	pathSecretQuery    = "spec.inject.query"
	pathSecretScheme   = "spec.inject.scheme"
	pathSecretOAuth    = "spec.oauth"
	pathSecretTokenURL = "spec.oauth.tokenUrl"
	pathSecretValue    = "spec.value"
)

// DecodeSecret reads one Secret manifest, in any syntax design 003 admits,
// and discards the status a client sent. It is Decode for the kind the value
// belongs to; the two are separate functions because every caller of either
// knows which kind its route serves.
func DecodeSecret(body []byte, contentType string) (v1.Secret, error) {
	var obj v1.Secret
	if err := decode(body, contentType, v1.KindSecret, &obj); err != nil {
		return v1.Secret{}, err
	}
	obj.Status = v1.SecretStatus{}
	return obj, nil
}

// SecretOptions are what ResolveSecret needs beyond the manifest: who is
// applying, the object this one replaces, and the clock the timestamps come
// from.
type SecretOptions struct {
	Actor    Actor
	Existing *v1.Secret // the current object on update; nil on create
	Now      func() time.Time
}

// ResolveSecret validates and defaults one Secret. The returned object
// carries spec.value where the caller supplied one, because the store is the
// next reader; every path that answers a caller strips it, which is what
// StripSecretValue is for.
//
// It is deterministic and never mutates its input: the same manifest and the
// same options produce byte-identical output.
func ResolveSecret(in *v1.Secret, o SecretOptions) (*v1.Secret, error) {
	if in == nil {
		return nil, fail("missing_field", "ResolveSecret needs a manifest")
	}
	obj := cloneSecret(in)
	obj.Status = v1.SecretStatus{}
	if obj.APIVersion != v1.APIVersion {
		return nil, failAt("unsupported_version", "apiVersion", "This server serves "+v1.APIVersion+".")
	}
	if obj.Kind != v1.KindSecret {
		return nil, failAt("unsupported_kind", "kind", "This server does not serve that kind.")
	}
	if err := validateMetadata(obj.Metadata); err != nil {
		return nil, err
	}
	if obj.Metadata.Name == "" {
		return nil, failAt("missing_field", "metadata.name", "A secret is named by the caller.")
	}
	defaultSecret(&obj.Spec)
	if err := validateSecretSpec(obj.Spec); err != nil {
		return nil, err
	}
	if err := secretValueRule(obj.Spec, o.Existing); err != nil {
		return nil, err
	}
	if o.Existing != nil && o.Existing.Metadata.Name != obj.Metadata.Name {
		return nil, failAt("immutable_field", "metadata.name", "This field cannot be changed after the secret is created.")
	}
	return &obj, nil
}

// defaultSecret fills every absent field the contract gives a default. The
// injection place is defaulted before it is validated, so a manifest that
// names neither a header nor a query parameter takes the header the contract
// names rather than being refused for naming nothing.
func defaultSecret(s *v1.SecretSpec) {
	if s.Kind == "" {
		s.Kind = v1.SecretStatic
	}
	if len(s.Scope.Ports) == 0 {
		s.Scope.Ports = []int{v1.DefaultSecretPort}
	}
	if s.Inject.Header == "" && s.Inject.Query == "" {
		s.Inject.Header = v1.DefaultInjectHeader
	}
	if s.Inject.Scheme != "" {
		return
	}
	// The scheme is derived from where the value lands: a value written into
	// Authorization is a bearer token unless the owner said otherwise, and a
	// value written anywhere else is the value itself.
	if strings.EqualFold(s.Inject.Header, v1.DefaultInjectHeader) {
		s.Inject.Scheme = v1.SchemeBearer
		return
	}
	s.Inject.Scheme = v1.SchemeRaw
}

func validateSecretSpec(s v1.SecretSpec) error {
	if !slices.Contains(v1.SecretKinds, s.Kind) {
		return failAt("invalid_field", pathSecretKind, "The kind is "+strings.Join(v1.SecretKinds, " or ")+".")
	}
	if err := validateSecretScope(s.Scope); err != nil {
		return err
	}
	if err := validateSecretInject(s.Inject); err != nil {
		return err
	}
	return validateSecretOAuth(s)
}

func validateSecretScope(scope v1.SecretScope) error {
	if len(scope.Hosts) == 0 {
		return failAt("missing_field", pathSecretHosts, "A secret names the hosts its value may be sent to; there is no scope that means every host.")
	}
	for i, host := range scope.Hosts {
		if err := ValidateHostPattern(pathSecretHosts+"["+strconv.Itoa(i)+"]", host); err != nil {
			return err
		}
	}
	for i, port := range scope.Ports {
		if port < 1 || port > 65535 {
			return failAt("invalid_field", pathSecretPorts+"["+strconv.Itoa(i)+"]", "A port is between 1 and 65535.")
		}
	}
	return nil
}

func validateSecretInject(inject v1.SecretInject) error {
	if inject.Header != "" && inject.Query != "" {
		return failPaths("exclusive_fields", "A secret is injected into one header or one query parameter, never both.", []string{pathSecretHeader, pathSecretQuery})
	}
	switch {
	case inject.Header != "":
		if !headerNamePattern.MatchString(inject.Header) {
			return failAt("invalid_field", pathSecretHeader, "The header name carries only the characters a header name may.")
		}
		if slices.Contains(unsubstitutableHeaders, strings.ToLower(inject.Header)) ||
			strings.HasPrefix(strings.ToLower(inject.Header), "x-forwarded-") {
			return failAt("invalid_field", pathSecretHeader, "That header frames the request and is never rewritten, so a value placed in it would never be substituted.")
		}
	case inject.Query != "":
		if !queryNamePattern.MatchString(inject.Query) {
			return failAt("invalid_field", pathSecretQuery, "The query parameter name carries only letters, digits, and the characters . _ ~ and -.")
		}
	}
	if !slices.Contains(v1.SecretSchemes, inject.Scheme) {
		return failAt("invalid_field", pathSecretScheme, "The scheme is "+strings.Join(v1.SecretSchemes, ", ")+".")
	}
	return nil
}

func validateSecretOAuth(s v1.SecretSpec) error {
	if s.Kind != v1.SecretOAuthClientCredentials {
		if s.OAuth != nil {
			return failPaths("exclusive_fields", "The oauth section belongs to a secret of kind "+v1.SecretOAuthClientCredentials+".", []string{pathSecretKind, pathSecretOAuth})
		}
		return nil
	}
	if s.OAuth == nil {
		return failAt("missing_field", pathSecretOAuth, "A secret of kind "+v1.SecretOAuthClientCredentials+" names the endpoint it mints a token at.")
	}
	parsed, err := url.Parse(s.OAuth.TokenURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return failAt("invalid_field", pathSecretTokenURL, "The token endpoint is an https:// URL.")
	}
	return nil
}

// secretValueRule holds the one field a read never returns. It is required
// when the object is new, optional on an update that changes something else,
// and always held to the shape its kind and scheme need.
func secretValueRule(s v1.SecretSpec, existing *v1.Secret) error {
	if s.Value == "" {
		if existing == nil {
			return failAt("missing_field", pathSecretValue, "A secret is created with its value.")
		}
		return nil
	}
	if len(s.Value) > MaxSecretValueBytes {
		return failAt("invalid_field", pathSecretValue, fmt.Sprintf("The value is at most %d bytes.", MaxSecretValueBytes))
	}
	if strings.ContainsAny(s.Value, "\r\n\x00") {
		return failAt("invalid_field", pathSecretValue, "The value carries no line break and no NUL byte, because it is written into a header.")
	}
	if s.Kind != v1.SecretOAuthClientCredentials && s.Inject.Scheme != v1.SchemeBasic {
		return nil
	}
	// Both halves are needed: basic encodes user:pass and the client
	// credentials grant authenticates with an id and a secret.
	left, right, found := strings.Cut(s.Value, ":")
	if !found || left == "" || right == "" {
		return failAt("invalid_field", pathSecretValue, "This secret's value is two halves separated by a colon, and neither half is empty.")
	}
	return nil
}

// StripSecretValue is the object as a caller reads it. Every route answers
// through it, so a value that was written cannot be read back through any
// surface.
func StripSecretValue(obj v1.Secret) v1.Secret {
	out := cloneSecret(&obj)
	out.Spec.Value = ""
	return out
}

// CompanionEnv are the keys that travel beside a mounted secret's own key:
// the header or query parameter name the sandbox is told to put the
// placeholder in. A secret that injects into Authorization needs neither,
// because that is where a client puts a credential without being told.
func CompanionEnv(env string, inject v1.SecretInject) map[string]string {
	switch {
	case inject.Query != "":
		return map[string]string{env + "_QUERY": inject.Query}
	case inject.Header != "" && !strings.EqualFold(inject.Header, v1.DefaultInjectHeader):
		return map[string]string{env + "_HEADER": inject.Header}
	}
	return nil
}

// companionNames is CompanionEnv's key set as the resolver checks it, which
// is both possible companions rather than the one this secret has: a key that
// would collide with another entry's companion is refused whatever that
// entry's injection place turns out to be, so adding a query parameter to a
// secret never invalidates a manifest that already resolved.
func companionNames(env string) []string {
	return []string{env + "_HEADER", env + "_QUERY"}
}

func cloneSecret(in *v1.Secret) v1.Secret {
	out := *in
	out.Metadata.Labels = maps.Clone(in.Metadata.Labels)
	out.Metadata.Annotations = maps.Clone(in.Metadata.Annotations)
	out.Spec.Scope.Hosts = slices.Clone(in.Spec.Scope.Hosts)
	out.Spec.Scope.Ports = slices.Clone(in.Spec.Scope.Ports)
	if in.Spec.OAuth != nil {
		oauth := *in.Spec.OAuth
		out.Spec.OAuth = &oauth
	}
	return out
}
