## What this changes

<!-- What behaviour is different after this, for whoever writes a manifest, runs cellad, or builds on the packages. -->

## Why

<!-- The problem it solves. Link the issue or the spec if there is one. -->

## Checklist

- [ ] `make` passes locally.
- [ ] A bug fix carries a test that fails without it.
- [ ] A change to the manifest schema, the `/v1` API, a configuration
      variable, a webhook payload, or an event payload is reflected in its
      spec and in `docs/`.
- [ ] A change to an exported package keeps every existing call site compiling,
      or the CHANGELOG names the break.
