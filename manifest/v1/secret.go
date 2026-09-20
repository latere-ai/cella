// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import "time"

// KindSecret is the kind a Secret manifest declares.
const KindSecret = "Secret"

// Secret is one credential a sandbox may use and never holds. What the
// sandbox holds is an opaque placeholder; the value lives encrypted in the
// control plane's store and reaches the gateway in the map, which swaps the
// placeholder for it on the way out toward a host this object's scope names
// and nowhere else.
type Secret struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata"`
	Spec       SecretSpec   `json:"spec"`
	Status     SecretStatus `json:"status,omitzero"`
}

// SecretSpec is what the owner declared. Value is write-only: it is accepted
// on apply and absent from every read, so a value that has been written can
// be replaced and never retrieved.
type SecretSpec struct {
	Kind   string       `json:"kind,omitempty"`
	Scope  SecretScope  `json:"scope,omitzero"`
	Inject SecretInject `json:"inject,omitzero"`
	OAuth  *SecretOAuth `json:"oauth,omitempty"`
	Value  string       `json:"value,omitempty"`
}

// The two kinds of secret. A static one substitutes the value it was given;
// an oauth_client_credentials one substitutes an access token the gateway
// mints from the value, which is the client id and secret.
const (
	SecretStatic                 = "static"
	SecretOAuthClientCredentials = "oauth_client_credentials"
)

// SecretKinds is every kind a Secret takes, in the order the contract lists
// them.
var SecretKinds = []string{SecretStatic, SecretOAuthClientCredentials}

// SecretScope is how far a value travels: the hosts it may be substituted
// toward and the ports on those hosts. There is no "any host" scope, because
// a value sent anywhere is a value the sandbox effectively holds.
type SecretScope struct {
	Hosts []string `json:"hosts,omitempty"`
	Ports []int    `json:"ports,omitempty"`
}

// DefaultSecretPort is the port a scope covers when it names none.
const DefaultSecretPort = 443

// SecretInject is where in a request the value replaces its placeholder: one
// header or one query parameter, never both, and the body only where the
// owner opted into it.
type SecretInject struct {
	Header string `json:"header,omitempty"`
	Scheme string `json:"scheme,omitempty"`
	Query  string `json:"query,omitempty"`
	Body   bool   `json:"body,omitempty"`
}

// DefaultInjectHeader is the header a secret injects into when it names
// neither a header nor a query parameter.
const DefaultInjectHeader = "Authorization"

// The encodings the substituted value takes. The scheme is the encoding and
// nothing more: the client writes whatever word goes before the value,
// because substitution replaces the placeholder where the client put it.
const (
	SchemeBearer = "bearer"
	SchemeBasic  = "basic"
	SchemeRaw    = "raw"
)

// SecretSchemes is every scheme, in the order the contract lists them.
var SecretSchemes = []string{SchemeBearer, SchemeBasic, SchemeRaw}

// SecretOAuth is the client_credentials grant an oauth secret mints from.
// The client id and secret are the value, not fields here, so the halves a
// read may not return are in the one field a read never returns.
type SecretOAuth struct {
	TokenURL string `json:"tokenUrl,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Audience string `json:"audience,omitempty"`
}

// SecretStatus is the server's. Version counts value writes, so a sandbox's
// map can be re-pushed on a change that altered no other field, and
// MountedBy is derived at read from the sandboxes that name this secret.
type SecretStatus struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	MountedBy int       `json:"mountedBy"`
}

// SecretIDPrefix is the kind prefix a Secret's id carries.
const SecretIDPrefix = "sec_"

// SecretMount is one entry of a Sandbox's spec.secrets: which secret, and
// the environment key its placeholder arrives under.
type SecretMount struct {
	Name string `json:"name"`
	Env  string `json:"env"`
}

// SecretsStatus says which placeholders are in the sandbox's environment and
// which of them will leave it as inert strings, because the secret was
// deleted or its scope no longer has a host the sandbox may reach. A caller
// reads the second list to learn that those requests go out unauthenticated
// rather than fail.
type SecretsStatus struct {
	Mounted       []string `json:"mounted,omitempty"`
	NotInjectable []string `json:"notInjectable,omitempty"`
}

// MountedSecret is the control plane's own record of one mount: which secret
// the sandbox was bound to, under which key, and the token it holds in place
// of the value. Binding is by id, so a secret deleted and recreated under one
// name is a different secret and a running sandbox is not re-bound to it.
type MountedSecret struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	Env         string `json:"env"`
	Placeholder string `json:"placeholder"`
}
