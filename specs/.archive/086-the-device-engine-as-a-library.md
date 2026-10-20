---
title: "The device engine as a library: `latere.ai/x/cella/device` serves a person's own computer as one of their environments for any program that embeds it, with the host policy file, the key in the keychain, approvals, the activity log, the lock and the control socket unchanged on disk, and what a hosted plane decides supplied by the embedder"
status: complete
track: core
depends_on:
  - specs/.archive/081-local-driver.md
  - specs/.archive/082-the-worker-as-a-library.md
  - specs/.archive/083-environments-a-subject-owns.md
  - specs/.archive/085-seatbelt-on-macos.md
affects: [device/ (new, from the latere command's internal/devices), go.mod, go.sum, docs/local.md, docs/README.md, CHANGELOG.md]
effort: large
created: 2026-10-20
updated: 2026-10-20
author: changkun
---

# The device engine as a library

## Overview

A person's own computer serves as one of their Cella environments through
the worker on the local driver ([[082-the-worker-as-a-library]],
[[081-local-driver]]) with a lot around it: the folders and network the
person allows, the environment key and its rotation, the person's approval
of each session, a log of every operation, one engine per computer, and a
way for a second program to watch and drive the one that serves. The latere
command built all of that as `latere devices`, in its own
`internal/devices`, so no other program can run it.

A desktop application has to run the same engine in its own process: it
shows the approvals, the activity and the connect screen itself, and it
must not disagree with a terminal about which computer is connected, under
which policy, or who answered a session. Two copies of the engine would
drift. This spec moves the engine here, beside the worker and the local
runtime it builds on, as the exported package `device`, so both programs
import one implementation.

It lives in Cella and not in `latere.ai/x/pkg`: Cella imports pkg, and the
engine imports Cella's worker, local runtime and client, so a home in pkg
would be a cycle. As an open core, the package carries no hosted plane's
values; each is supplied by the embedder.

## Design

### What moves

Everything that serves the computer, unchanged in behavior:

- **The engine** (`New`, `Run`): the worker on the local driver, built from
  the host policy; the key minted, kept, rotated and revoked; the
  confirmation that the device is still the person's; the sandboxes swept
  when it is removed; pause and resume.
- **The host policy** (`Policy`, `LoadPolicy`, `SavePolicy`, `PolicyChange`):
  the name, the folders granted read or write, the network (none, an
  allowlist, or open), how a session is let in (`allow` or `ask`), the read
  paths and the remembered triggers.
- **Approvals** (`Admit`, `Check`, `Subscribe`, `Attach`, `Answer`,
  `Waiting`): a session that asks to start waits for the person while
  someone is attached, and is refused with a reason when no one is.
- **The activity log** (`OpenActivity`, `ReadActivity`, `FollowActivity`):
  one NDJSON file per environment, rotated, carrying no content.
- **Credentials** (`Credentials`, `PutCredentials`): the tokens a client
  hands an engine started without a saved sign-in, checked, and asked for
  again a minute before they expire.
- **The control socket** (`ListenControl`, `ServeControl`, `ControlHandler`,
  `ControlPost`): the same API over HTTP on a Unix socket, mode 0600, for
  another process of the same user, checked by its peer credentials.
- **The lock** (`TakeLock`) and **the keys** (`Keys`, `Keychain`, `KeyFile`).
- **Connecting** (`ProposeConnect`, `Connect`, `ConnectFacts`, `Consent`):
  the proposal, the facts the person is shown, and the consent that must
  match them.

### On disk, unchanged

A computer connected by one embedder is served by the other, and an
upgrade from the latere command's own copy changes nothing:

| What | Where |
|---|---|
| host policy | `$XDG_CONFIG_HOME/latere/device.yaml`, else `~/.config/latere/device.yaml` |
| engine state | `$XDG_STATE_HOME/latere/devices/`, else `~/.local/state/latere/devices/`: `lock`, `control.sock`, `owner.json`, one activity directory per environment |
| sandboxes | `$XDG_CACHE_HOME/latere/devices/<environment>`, else `latere/devices/<environment>` in the user cache directory |
| key | the system keychain, service `latere-cli device key`, with `device-keys.json` beside the policy as the fallback, or the file alone under `LATERE_DEVICE_KEYS_FILE` |
| saved sign-in | `LATERE_AUTH_TOKEN_FILE`, else `auth-token.json` beside the policy, denied to every session |

`DefaultPaths` computes these. The policy file is written by the same YAML
encoder with the same field order, so a file written by either embedder
reads the same in the other.

### What the embedder supplies

The open core names no hosted plane. Four things come from the embedder:

- **`Config.Labels`** (`SessionLabels`): the label keys the plane that
  starts sessions puts on a sandbox for its session, agent, agent version,
  trigger and what the trigger fires on, and the annotation key of the
  agent's name. An empty key shows nothing.
- **`CredentialsOptions`**: the audience of the plane's own token
  (`PlatformAudience`; empty takes no platform token) and a `TokenReader`
  that reads a token's audiences, subject, expiry and whether it is in the
  person's own context. Without a reader, no token a client hands is taken.
- **`Account`**: what the engine asks of Cella as the person (mint, revoke,
  exists). `NewCellaAccount` answers it over a Cella client.
- **`Enroller`**: the steps of a connect, a rename and a removal.
  `NewEnrollment` is the shared one over Cella: it proposes the terms, saves
  the policy, creates the environment (`worker` mode, `process` isolation,
  one worker, eight sandboxes) under a fresh `dev-` id, and asks a
  `Platform` for the rest.

`Platform` is what only the plane knows: the labels every environment of
the person carries, the organization a sign-in acts in (a device is
personal), the device's name and its removal in the plane's records, and
how many devices one person may connect, which a refusal at the limit
tells.

### The embedder's own words

An `Error` carries a stable `Code`, a sentence, a hint and the detail; a
`Fact` carries a stable `Kind`. The sentences, hints and facts are the
latere command's words, kept as the defaults so a terminal reads the same
whichever embedder answers. An application with its own surface shows its
own words keyed on the code and the kind. The command's refusals that
concern only the command (an unverified build, a sign-out it cancelled or
refused) stay in the command.

### What stays in the latere command

The terminal client (the prompt, its answers, the status line), the cobra
commands, the saved sign-in as an `Account` and a `Platform` over the
plane's API, the root and release-key checks before `serve`, and the
command's own label keys, audience and token reader, which it passes in.

### Running it

The embedder takes the lock, opens the control socket, serves it, and runs
the engine. Whichever program holds the lock serves; the other attaches
through the socket and sees the same status, approvals and activity.
`docs/local.md` carries the example.

## Acceptance criteria

1. `latere.ai/x/cella/device` builds on Linux and macOS and imports nothing
   from the latere command.
2. The moved tests pass, the confinement tests among them under real
   Seatbelt on macOS through the test binary's own launcher.
3. Files on disk are unchanged: the paths in the table, the keychain
   service, and a policy file byte-identical for every shape the tests
   write.
4. `TestAnApplicationEmbedsTheEngine` drives a connect with the person's
   consent, a session's approval, a rename and a removal through the
   exported API alone, from a package outside `device`.
5. The connect steps every embedder shares are tested against a stub Cella
   and a fake plane: the environment created, a refused name, an
   organization's context, every create refusal the plane names, the key
   minted and revoked, and a removed device.
6. Without a token reader, a platform audience or label keys, the engine
   takes no client token, no platform token, and names no session, and says
   so.
7. The package meets the gate's coverage floor on Linux without a sandbox
   runtime and on macOS.

## Outcome

Built on 2026-10-20. The engine moved from the latere command's
`internal/devices` as it was; the edits are the overlay: `SessionLabels`
in place of the command's label constants, `TokenClaims` and a
`TokenReader` in place of reading the plane's JWT, the platform audience
an option, and `go.yaml.in/yaml/v3`, the module's own YAML library, which
writes the same bytes. `enroll.go` is new: the connect steps the command
did itself, minus the plane's parts, which are the `Platform` interface.
Criterion 4 is `device/embed_test.go`. Coverage is 90.2% on macOS under
Seatbelt and 90.3% on Linux without srt. The latere command switches to the package in
the release after the one that carries it, deleting `internal/devices`.
