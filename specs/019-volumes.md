---
title: "Volumes: the Volume kind, attachment, the workspace as a volume, what persists and how"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
affects: [manifest/v1/, runtime/, controller/, internal/api/, internal/store/]
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
---

# Volumes

## Overview

State that outlives a sandbox is a `Volume`: storage with a life of its
own, created and deleted by its owner, attached to a sandbox by a mount
in the manifest, and detached when the sandbox ends. It is how an
application an agent built keeps its database when its sandbox is
replaced, how an analysis run receives the tools, files, and
configuration it needs without baking them into an image, and how a
workspace survives a sandbox that a data plane lost. Volumes are
control plane objects, not a file plane: a file service or a git host
is reached from inside a sandbox, and a volume is where their contents
land.

## Current state

Not built. The hosted platform persisted a workspace by a tier field
and had no caller-declarable attachment; the mount of a remote file plane it once
planned is withdrawn. This spec puts persistence where the caller can
see and name it.

## Design

### The Volume kind

```yaml
apiVersion: cella.latere.ai/v1
kind: Volume
metadata:
  name: app-state
  labels: {app: notes}
spec:
  environment: default        # where it lives; a volume is bound to one environment
  size: 20Gi                  # Kubernetes quantity; grows, never shrinks
  class: ""                   # the environment's default storage class when empty
  access: single              # single | shared-read
  source:                     # optional; how it is filled the first time
    kind: empty               # empty | snapshot | image | archive
    snapshot: ""              # another Volume's snapshot id
    image: ""                 # an OCI image whose root file system is copied in
    archive: ""               # an https URL of a tar the control plane fetches through its own egress rule
  retain: true                # survives its last sandbox; false deletes with it
status:
  id: vol_01J9...
  owner: alice@example.com
  phase: Available            # Pending | Available | Attached | Deleting | Failed | Lost
  attachedTo: [01J9...]       # sandbox ids
  capacity: 20Gi
  createdAt: ...
```

Rules:

- `access: single` attaches read-write to one sandbox at a time; a
  second read-write attach is `volume_busy`. Any number of sandboxes
  may attach it read-only while no one holds it read-write.
  `shared-read` attaches read-only to any number and never read-write
  after the source fills it; it is how tools and reference data reach a
  fleet.
- `size` may grow on an environment with `Resize`; it never shrinks.
- `source.archive` is fetched by the control plane with the egress rule
  of `CELLA_SOURCE_ALLOW`, never by a sandbox, so a volume's initial
  content comes from a host the operator listed.
- Ownership is the applying subject; whether another subject may
  attach is the authorizer's `volume.attach` decision, with the volume
  as the resource. The built-in owner policy allows the owner and
  admins.
- A volume is bound to one environment because storage does not cross
  clusters. Moving state is a snapshot and a new volume.

### Attachment

A `Sandbox` mounts a volume by `spec.volumes[] {name, path, volume,
readOnly}`, or as its workspace by `workspace.source: volume`. At
create the controller attaches in order, after the egress map and
before the token; a volume that cannot attach fails the create with
`VolumesAttached: False` and the volume's name, and nothing else is
left behind. At delete the controller detaches every volume; a volume
with `retain: false` and no other attachment is deleted with its last
sandbox. While a sandbox is `Stopped` on an environment with `Volumes`,
volumes may be added and removed through an update, so an application's
data can be moved under a new sandbox without a restart of the volume.

### The workspace

Every sandbox has a managed workspace: a volume the controller creates
with the sandbox at `workspace.path`, sized by `resources.disk`, on the
environment's workspace storage class ([[021-data-plane-workers]]). It
lives exactly as long as the sandbox object: `Stop` keeps it, `Delete`
removes it, and recovery reattaches it where the driver still holds it.
`workspace.source: volume` replaces the managed one with a caller's
`Volume`, which then outlives the sandbox by its own rules. There is
one way to keep files beyond a sandbox, and it is a `Volume`; how
cheap or fast the managed workspace is belongs to the operator's
storage class, never to a field a caller sets.

### Snapshots

`POST /v1/volumes/{id}/snapshots` takes a point-in-time copy on an
environment whose driver supports it; the snapshot id is a `source` for
a new volume. Snapshots are the answer to "fork this application's
state for a test" and to backups. A driver without snapshots reports
the capability absent and the route is 422.

### Drivers

| Driver | A Volume is |
|---|---|
| k8s | a PersistentVolumeClaim; `class` a StorageClass; `shared-read` needs a `ReadOnlyMany` class; snapshots through the VolumeSnapshot API |
| podman | a named volume; snapshots by copy |
| vm | a block device or a virtiofs share |
| local, native | a directory under `CELLA_DATA_DIR/volumes/<id>`, bind-mounted; `size` is a quota where the file system has one and advisory otherwise, said in a warning |

### What a volume is not

Not a mount of a remote file service; not a secret store; not shared
read-write between running sandboxes, because two writers on one file
system need a coordination the control plane does not provide. A
sandbox that wants to share live state with a peer uses the mesh
([[022-mesh-and-spawn]]).

## Not in this spec

The lifecycle that drives attach and detach ([[005-lifecycle-controller]]);
the routes ([[008-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A `single` volume refuses a second read-write attach with `volume_busy` and accepts read-only attaches; `shared-read` refuses read-write after fill | `TestAccessModes` | not built |
| A sandbox deleted with a `retain: true` volume leaves the volume `Available`; with `retain: false` and no other attachment the volume is gone | `TestRetain` | not built |
| A sandbox recreated over a `workspace.source: volume` sees the files the previous one wrote | `TestWorkspaceVolumeSurvivesTheSandbox` on every driver | not built |
| `source.archive` is fetched through the control plane's own egress rule and refused for an unlisted host | `TestArchiveSourceIsAllowlisted` | not built |
| A volume attach that fails leaves no sandbox and no dangling attachment | `TestAttachFailureIsClean` | not built |
| Growing a volume works where `Resize` holds; shrinking is `invalid_field` | `TestGrowOnly` | not built |
| The conformance suite's volume cases pass on every driver that declares `Volumes` | `runtimetest` | not built |
