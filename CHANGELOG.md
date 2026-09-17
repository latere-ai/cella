# Changelog

What changed for whoever writes a manifest, runs `cellad`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- A caller's token is refused once it is more than 24 hours old, even
  when its `exp` is still in the future, so a caller that works for days
  re-mints at its own issuer instead of holding one token. A token that
  does not say when it was minted, carrying no `iat`, is refused for the
  same reason. This is the same rule Origo and Lux apply, one age bound
  across the three. The keys `cellad` mints for environments are
  unaffected: one lives `CELLA_ENVIRONMENT_KEY_TTL`, a year by default,
  and its `exp` is its only bound.
- `cellad` knows who is calling. `CELLA_OIDC_ISSUERS` lists the OpenID
  Connect issuers you trust, any of them; at start `cellad` reads each
  one's discovery document and key set and refuses to start when one does
  not answer, names another issuer, or publishes no `RS256` or `ES256`
  key. A bearer is accepted when a listed issuer signed it, its `aud`
  contains `CELLA_OIDC_AUDIENCE` (default `cella`), it has not expired,
  and it was minted less than 24 hours ago; nothing else about it is
  interpreted. A subject is the issuer and the `sub` joined,
  `https://login.example.com|alice`, so two issuers that agree on a `sub`
  are two subjects, and every claim of the token reaches your authorizer
  verbatim. There is no anonymous access and no
  API key.
- `cellad` signs the identities it hands out. `CELLA_TOKEN_KEY` is one or
  two PEM RSA private keys of at least 2048 bits: the first signs a
  sandbox's token and an environment's key, every one is published at
  `/.well-known/jwks.json`, and each carries its RFC 7638 thumbprint as
  its `kid`, so anything verifies a sandbox's identity offline with a
  stock JWT library. Rotation is yours and lives in the configuration:
  put a new key in front, and remove the old block once the tokens it
  signed have expired. `CELLA_PUBLIC_URL` is now required, because it is
  the issuer those tokens name.
- `cellad` asks somebody else what a caller may do.
  `CELLA_AUTHORIZER_URL` and `CELLA_AUTHORIZER_TOKEN` point at the
  endpoint you write, with `CELLA_AUTHORIZER_TIMEOUT` (`5s`) bounding one
  call and `CELLA_AUTHORIZER_CACHE` (`60s`) holding an allow whose answer
  names no `ttl`. An allow may carry the three ceilings and, on a list, a
  filter. Anything that is not a decision fails the request and never
  allows it, and a connection that died before a response line is retried
  once and nothing else is. With the URL unset, the built-in owner policy
  decides from `CELLA_ADMIN_SUBJECTS` and the owner of each object: you
  create anything but an environment, you act on what you own, a list
  returns your own, admins act on everything and alone make environments,
  and a sandbox's own identity reads and execs itself, reads the tree
  below it, creates a child and nothing else.
- Running `cellad` therefore needs three variables it did not need
  before, `CELLA_OIDC_ISSUERS`, `CELLA_PUBLIC_URL` and `CELLA_TOKEN_KEY`,
  and `make run` needs an issuer named on the command line until the test
  stubs land. The README has the whole of it.

- The vocabulary an authorization endpoint for Cella speaks is an
  importable package, `latere.ai/x/cella/authorizer`. Write the endpoint
  `CELLA_AUTHORIZER_URL` points at in Go and take the thirty-two actions
  of the identity spec's table as constants, the resource kind each one
  acts on from `Kind`, the whole table from `Vocabulary()`, and the three
  ceilings an allow may carry from `WireLimits`, rather than keeping a
  copy of the strings. The envelope on the wire is
  `latere.ai/x/pkg/authz`'s, and so are the stub and the conformance
  suite an endpoint passes; an endpoint built on
  `latere.ai/x/pkg/authz/server` with nothing behind it but a decider
  passes that suite with this vocabulary. `cellad` asks no authorizer
  yet: the configuration, the client and the guard are the identity
  spec's and are not built.

- The repository: the `cellad` binary serving its probes on two listeners,
  typed configuration from `CELLA_*` variables, the quality gate, and the
  design specs. Nothing creates a sandbox yet; the specs say what will.
- The design, revised after review: Cella is a control plane whose data
  plane is a driver in-process or a worker on your own infrastructure;
  the API group is `cella.latere.ai/v1`; five kinds (`Sandbox`, `Secret`,
  `Volume`, `SandboxSet`, `Environment`); an egress gateway that keeps
  secret values out of sandboxes; volumes for persistent state; mesh and
  spawn with a boundary a child cannot widen; three scheduling
  strategies and sets for rollouts; desired state that recovers a lost
  sandbox; six drivers across four isolation classes.
