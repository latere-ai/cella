---
title: "Volumes: the Volume kind, access and attachment, sources and fill, snapshots, the managed workspace"
status: in-progress
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
affects: [manifest/v1/, runtime/, controller/, internal/api/, internal/store/, internal/config/]
effort: medium
created: 2026-09-12
updated: 2026-10-18
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
set's results outlive its replicas. Volumes are control plane objects,
not a file plane: a file service or a git host is reached from inside a
sandbox, and a volume is where their contents land. There is one way
to keep files beyond a sandbox, and it is a `Volume`.

## Current state

[[084-image-volumes]] built the part an evaluation's check sandbox
needs: the `Volume` kind with an `empty` and an `image` source, the
fill and its loop, a sandbox's `spec.volumes` with the access table,
retain and delete, the Lost rule and `VolumeMissing`, the events, the
`/v1/volumes` routes, and the podman and k8s drivers. Its Outcome
lists where the design below was amended while it was built. The
authorizer's view of a create, the resolved Volume as `proposed`, and
the owner's volume ceiling from `limits.max_volumes` followed in
v0.17.2, below.

Not built: the `snapshot` and `archive` sources with `Snapshotter`,
`CELLA_SOURCE_ALLOW` and `CELLA_MAX_SOURCE_BYTES`; a named `class`
(refused with `capability_unsupported`); growth through `ResizeVolume`
and `CELLA_MAX_VOLUME_SIZE`; the control plane's writer `Write`;
`workspace.source: volume`; `Change.Volumes` on a stopped sandbox; and
volumes on the native, local, vm and remote drivers. The managed
workspace is the driver's, as below, and has no test of its own yet.

## Design

### The Volume kind

```yaml
apiVersion: cella.latere.ai/v1beta1
kind: Volume
metadata:
  name: app-state
  labels: {app: notes}
spec:
  environment: default        # where it lives; a volume is bound to one environment
  size: 20Gi                  # Kubernetes quantity; grows, never shrinks
  class: ""                   # the environment's default storage class when empty
  access: single              # single | shared-read
  source:                     # how it is filled once, at create
    kind: empty               # empty | snapshot | image | archive
    snapshot: ""              # a snapshot id, same environment
    image: ""                 # an OCI image whose root file system is copied in
    archive: ""               # an https URL of a tar or tar.gz the driver fetches
    sha256: ""                # optional; the archive's digest, checked before extraction
  retain: true                # survives its last sandbox; false deletes with it
status:
  id: vol_01J9...
  owner: https://login.example.com|alice
  phase: Available            # Pending | Available | Failed | Deleting | Lost
  reason: ""                  # for Failed: SourceUnreachable, SourceTooLarge, SourceDigestMismatch, SourceUnsupported, ClassUnavailable
  message: ""                 # for Failed: the data plane's own sentence
  attachedTo: [sbx_01J9...]   # sandboxes whose desired state mounts it, Stopped included
  capacity: 20Gi
  snapshots: 2
  createdAt: ...
```

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `environment` | string | the default environment | no | an `Environment` the caller may use ([[006-identity]]); a volume never leaves it |
| `size` | quantity | none, required | grow only | Kubernetes quantity syntax as [[003-manifest-contract]] parses it; a smaller value is `invalid_field`; growth needs the driver's `ResizeVolume` to succeed, else `capability_unsupported`; above `CELLA_MAX_VOLUME_SIZE` is `ceiling_exceeded` |
| `class` | string | the environment's default | no | a storage class the environment's driver knows; unknown is `invalid_field`. Refused when set, `capability_unsupported`, until the drivers know classes: the operator chooses the storage, on k8s with `CELLA_K8S_VOLUME_CLASS` |
| `access` | enum | `single` | no | who may attach and how, below |
| `source.kind` | enum | `empty` | no | `empty`, `snapshot`, `image`, or `archive`; exactly the field for the kind is set, any other is `exclusive_fields` |
| `source.snapshot` | string | none | no | a `snp_` id of a snapshot the caller may read, in the same environment; another environment is `invalid_field` |
| `source.image` | string | none | no | an OCI reference; only on an environment whose driver declares `volumeImages`, else `capability_unsupported`. It passes the admission step as a sandbox's image does (below), and after it is pinned by digest, `name@sha256:` and 64 hex digits, else `invalid_field`: the volume stores the pinned reference, so a reader verifies its content by digest |
| `source.archive` | string | none | no | an `https://` URL whose host is on `CELLA_SOURCE_ALLOW` under the host rule; anything else is `invalid_field` |
| `source.sha256` | string | none | no | 64 hex characters; with `archive` only |
| `retain` | bool | `true` | yes | `false` deletes the volume with its last attached sandbox |

An image source passes [[007-admission]]'s step once, at create, as a
`Sandbox` that names the volume's environment, its labels and
`spec.image` set to the source, and nothing else, with the Volume in
the envelope's `volume` member. Only `spec.image` of the answer is
read, and it becomes the stored source; a refusal is
`admission_refused`. An endpoint that knows nothing of volumes decides
on the image as it would for a sandbox, catalog rules included.

The `status` fields are the controller's. `phase` moves `Pending` to
`Available` when the driver reports the fill complete, to `Failed` with
a `reason` when it cannot, to `Deleting` on delete, and to `Lost` when
`InspectVolume` on the reaper's tick reports the object gone; a lost
volume is never recreated, since its content is gone, and a sandbox
that needs it fails with `VolumeMissing` ([[005-lifecycle-controller]]).
`attachedTo` is every sandbox whose desired state mounts it, `Stopped`
included; `capacity` is what the driver granted; `snapshots` is a
count. Ids are `vol_` and `snp_` prefixed ULIDs ([[001-architecture]]).

### Authorization and the volume ceiling

A create asks `volume.create` after the body is resolved, with the
Volume it would store as `resource.proposed` ([[006-identity]]): the
defaults filled, `spec.source.kind` among them, the environment named,
`spec.size` as written, and an image source pinned by the admission
step above. An authorizer that allows image volumes only, or volumes up
to a size, decides on that proposal. The create then asks
`environment.use` on the volume's environment.

The allow of `volume.create` may carry `limits.max_volumes`, the
owner's volume ceiling. The controller counts the owner's volumes that
are not `Deleting`, in any environment and whatever their phase, so a
`Failed` or `Lost` volume holds its slot until its owner deletes it,
and refuses a create that would exceed the figure with
`volume_quota_exceeded`, 422, and nothing written. The count and the
write run under the controller's lock, on the replica that holds the
writer lease, so two concurrent creates at the ceiling yield one volume
and one refusal. An absent figure, or `0`, is no ceiling; no variable
of `cellad`'s sets one. The managed workspace below is not a Volume
and takes no slot.

### Access

`access` governs sandbox attachments; the control plane's own writer,
below, is outside it.

| `access` | Read-write attach | Read-only attach |
|---|---|---|
| `single` | one sandbox, while no sandbox holds it read-only | any number, while no sandbox holds it read-write |
| `shared-read` | never | any number |

A `Stopped` sandbox still holds its attachments, so moving a `single`
volume under a new sandbox is a detach from the old one through
`Change.Volumes` first ([[005-lifecycle-controller]]). An attach that
either row refuses is `volume_busy`, in both directions: the code's
sentence reads "The volume is attached in a way that excludes this"
([[008-api]]). Resolve decides `volume_busy` for a read-write mount of
a `shared-read` volume; the controller's binding of the mounts at
create, under its lock and before the sandbox's row is written or the
create answered, is authoritative for the rest, so a create that loses
a race for a `single` volume is refused `volume_busy` with nothing
written.

`volumes[].volume` and `workspace.volume` accept a name, resolved among
the caller's own volumes, or a `vol_` id, so a volume another subject
owns is attachable when the authorizer's `volume.attach` allows it
([[003-manifest-contract]], [[006-identity]]).

### The control plane's writer

`VolumeDriver` carries `Write(ctx, volumeID, dest string, src io.Reader)
error`: the control plane's own path into a volume, used by
[[020-scheduling-and-sets]] to land each replica's collected tar under
`<index>/` in the set's results volume, on any `access`, at any time
after `Available`. A sandbox never gets this path; it attaches. On k8s
it is a short-lived helper Pod, on podman a mount into a helper
container, on `vm` the guest agent, on `local` and `native` a write
into the directory.

### Sources and fill

A volume is `Pending` from create until its source is applied, and
every attach waits on `Available`: a sandbox bound to a `Pending`
volume stays placed until the fill settles, and one bound to a volume
that failed fails with `VolumesAttached` `False`. The driver fills it:

| Source | k8s | podman | vm | local, native | Cap and check |
|---|---|---|---|---|---|
| `empty` | the class's empty volume | an empty named volume | an empty disk | an empty directory | none |
| `snapshot` | a VolumeSnapshot clone | a copy | a disk clone | a copy | same environment only |
| `image` | a fill Pod runs the image with a static copier staged from `CELLA_K8S_FILL_IMAGE` (`cellad fill`) and copies its root file system; no fill image declares no `volumeImages` | the engine exports a never-started container's root file system into a second one that mounts the volume | a helper in the guest | `capability_unsupported` | the image's regular-file bytes against `size` |
| `archive` | a helper Pod fetches and extracts | a helper container | a helper in the guest | the driver's own process | below |

An archive fetch is the driver's, because the data plane holds the
network path, and it is bounded by what the control plane validated
and passed in `VolumeSpec.Source`: the host on `CELLA_SOURCE_ALLOW`,
which the driver re-applies to every redirect so a 302 to an unlisted
host is refused; at most `CELLA_MAX_SOURCE_BYTES` (default `10Gi`)
downloaded and at most `size` extracted; the `sha256` compared before
extraction when given; `tar` and `tar.gz` only, with entries confined
to the volume root. A failure sets `Failed` with the reason in the
table above and emits `volume.failed` ([[009-events]]).

### The managed workspace

Every sandbox has a managed workspace at `workspace.path`, sized by
`resources.disk`, on the environment's `workspaceClass`
([[021-data-plane-workers]]). It is the driver's, not a `Volume`
object: `CreateSpec.Workspace.VolumeID` empty means the driver makes
and owns it, so it appears on no `/v1/volumes` route, in no
`ListVolumes`, and against no volume ceiling. It lives exactly as long
as the sandbox object: `Stop` keeps it, `Delete` removes it, and
recovery reattaches it where the driver still holds it and recreates
it empty, with `WorkspaceReady: Recreated`, where it does not
([[005-lifecycle-controller]]). `workspace.source: volume` replaces it
with a caller's `Volume`, which then outlives the sandbox by its own
rules. How cheap or fast the managed workspace is belongs to the
operator's storage class, never to a field a caller sets.

### Attachment

A `Sandbox` mounts a volume by `spec.volumes[] {name, path, volume,
readOnly}`, or as its workspace by `workspace.source: volume`. At
create the controller attaches in [[005-lifecycle-controller]]'s step
4, the workspace first, after the egress map and before the token; a
volume that cannot attach fails the create with `VolumesAttached:
False` naming the volume, the undo detaches what was attached, and
nothing else is left behind. At delete the controller detaches every
volume, and a volume with `retain: false` and no other attachment is
deleted with its last sandbox. While a sandbox is `Stopped` on an
environment with `Volumes`, volumes may be added and removed through an
update, which the controller applies as the full list in
`Change.Volumes`. `DELETE /v1/volumes/{id}` on a volume with a
non-empty `attachedTo` is `volume_busy`; the caller detaches first.
Every transition emits its event of [[009-events]]: `volume.created`,
`.updated` (a resize or a `retain` change), `.attached`, `.detached`,
`.snapshotted`, `.failed`, `.deleted`.

### Snapshots

`POST /v1/volumes/{id}/snapshots` takes a crash-consistent point-in-time
copy through `Snapshotter`, on a driver that declares `Snapshots`, and
returns `{"id": "snp_...", "createdAt"}`; `GET` lists them; `DELETE
.../snapshots/{id}` removes one ([[008-api]]). A snapshot belongs to
its volume's owner, is readable by whoever may read the volume, is
bound to the volume's environment, has no retention of its own, and is
deleted with its volume. It is a `source` for a new volume in the same
environment, which is how an application's state is forked for a test
and how a backup is kept. State crosses environments as an archive:
export from a sandbox through the files route, apply a volume with an
`archive` source in the new environment.

### Drivers

| Driver | A Volume is | `shared-read` | Snapshots |
|---|---|---|---|
| k8s | a PersistentVolumeClaim, `ReadWriteOnce`, on `CELLA_K8S_VOLUME_CLASS` or the workspace class | read-only mounts of the claim; Pods of one node share it on a class that attaches to one node, and every node on a class with no node attach (NFS, CephFS) | the VolumeSnapshot API where the cluster has it |
| podman | a named volume | the volume mounted read-only | by copy |
| vm | a block device or a virtiofs share | read-only share | a disk clone |
| local | a directory under `CELLA_DATA_DIR/volumes/<id>`, granted to the stage as a readable or writable path at its host path, with the rewrite and warning of [[004-runtime-contract]] | read-only grant | by copy |
| native | a directory under `CELLA_DATA_DIR/volumes/<id>` | read-only bind | by copy |
| remote | the worker's driver's | the worker's | the worker's |

`size` is a quota where the file system has one and advisory otherwise,
said in a warning on `local` and `native`.

### What a volume is not

Not a mount of a remote file service; not a secret store; not shared
read-write between running sandboxes, because two writers on one file
system need a coordination the control plane does not provide. A
sandbox that wants to share live state with a peer uses the mesh
([[022-mesh-and-spawn]]).

## Not in this spec

The lifecycle that drives attach and detach ([[005-lifecycle-controller]]);
the routes' envelope and paging ([[008-api]]); the results collection
that uses `Write` ([[020-scheduling-and-sets]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every field rule in the table has a refusing case: shrink, unknown class, two source fields, a snapshot from another environment, an archive off the allow list or not `https://`, a bad digest, a size above the ceiling | `TestVolumeFieldRules`, table-driven | partial ([[084-image-volumes]]): shrink, two source fields, an image not pinned by digest, a named class (`capability_unsupported`), and the `snapshot` and `archive` sources refused whole; the snapshot environment, the archive host and digest, and the size ceiling wait on those features |
| `single`: a second read-write attach, and a read-write attach while a read-only one exists, are `volume_busy`; read-only attaches stack; `shared-read` refuses read-write | `TestAccessModes` | built ([[084-image-volumes]]) |
| A `Stopped` sandbox holds its attachment; after `Change.Volumes` detaches it, a new sandbox attaches read-write | `TestStoppedSandboxHoldsTheVolume` | partial ([[084-image-volumes]]): the stopped sandbox holds the volume against a delete and another writer, and a new writer attaches once it is deleted; `Change.Volumes` is not built |
| A volume that turns busy between resolve and create ends the sandbox `Failed VolumeBusy` with no attachment left | `TestAccessModes` | built, diverged ([[084-image-volumes]]): the binding runs before the create answers, so the loser is refused `volume_busy` with nothing written rather than failed |
| `Write` lands a tar under a path on a `shared-read` volume while sandboxes hold it read-only | `TestControlPlaneWriter` | not built |
| A volume is `Pending` until filled and every attach waits; each source kind fills on each driver that lists it and is `capability_unsupported` where it does not; each failure reason is produced by its cause | `TestFillLifecycle`, `TestAttachWaitsOnAvailable`, `TestVolumesOnARealEngine` | partial ([[084-image-volumes]]): `empty` and `image` on podman and k8s, `capability_unsupported` elsewhere; `SourceUnreachable`, `SourceTooLarge` and `SourceUnsupported` produced; `SourceDigestMismatch` and `ClassUnavailable` wait on the archive source and classes |
| An archive fetch refuses a redirect to an unlisted host, stops at the byte cap, refuses a digest mismatch, and confines entries to the volume root | `TestArchiveFetchIsBounded` | not built |
| The managed workspace is absent from `/v1/volumes` and `ListVolumes`, survives `Stop`, is removed at `Delete`, and is `Recreated` when the driver lost it | `TestManagedWorkspaceIsDriverInternal` | not built |
| A sandbox deleted with a `retain: true` volume leaves it `Available`; with `retain: false` and no other attachment the volume is gone; `DELETE` of an attached volume is `volume_busy` | `TestRetainAndDelete`, `TestASandboxMountsAVolume` | built ([[084-image-volumes]]) |
| A sandbox recreated over a `workspace.source: volume` sees the files the previous one wrote, on every driver | `TestWorkspaceVolumeSurvivesTheSandbox` | not built |
| A snapshot is listed, is a source for a new volume in its environment, is refused in another, is deleted with its volume, and has a `snp_` id | `TestSnapshots` | not built |
| Growth succeeds where `ResizeVolume` does and is `capability_unsupported` where the class cannot expand; a shrink is `invalid_field` | `TestVolumeUpdate` | partial ([[084-image-volumes]]): a shrink is `invalid_field` and a growth `capability_unsupported` on every driver, since none resizes yet |
| A `vol_` id in `volumes[].volume` attaches another subject's volume when the authorizer allows and is `not_found` when it does not | `TestAttachByIdUnderTheAuthorizer` | built ([[084-image-volumes]]) |
| `volume.create` carries the resolved Volume as `proposed`, the source kind defaulted and an image source pinned, and an authorizer refuses a create on it | `TestAVolumeCreateCarriesItsProposal`, `TestAVolumeCreateIsDecidedOnItsProposal` | built |
| `limits.max_volumes` refuses a create past the owner's count with `volume_quota_exceeded` and nothing written; `Available`, `Failed` and `Lost` volumes count and a `Deleting` one does not; another owner's volumes do not count; concurrent creates at the ceiling yield exactly the ceiling | `TestTheVolumeCeilingCountsTheOwnersVolumes`, `TestADeletingVolumeTakesNoSlot`, `TestTheVolumeCeilingHoldsUnderConcurrentCreates`, `TestAVolumeCreatePastTheCeilingIsVolumeQuotaExceeded` | built |
| Every transition emits its event | `TestVolumeEvents` | partial ([[084-image-volumes]]): created, updated, attached, detached, failed and deleted; snapshotted waits on snapshots |
| The conformance cases `VolumeLifecycle`, `VolumeAttachDetachWhileStopped`, `AttachFailureIsClean`, and `SnapshotAndRestore` pass on every driver that declares the capability | `TestPodmanConformance`, `TestClusterConformance` | partial ([[084-image-volumes]]): `VolumeLifecycle` and `AttachFailureIsClean` run on every driver that declares `Volumes`; `VolumeAttachDetachWhileStopped` and `SnapshotAndRestore` wait on `Change.Volumes` and snapshots |
