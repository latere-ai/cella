---
title: "Image volumes: the Volume kind with an empty or an image source, filled once, attached by a sandbox's spec.volumes under spec 019's access rules, its image admitted as a sandbox's is, on the podman and k8s drivers"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-runtime-contract.md
  - specs/005-lifecycle-controller.md
  - specs/007-admission.md
  - specs/008-api.md
  - specs/009-events.md
  - specs/010-state.md
  - specs/019-volumes.md
affects: [manifest/v1/, manifest/, runtime/, runtime/runtimetest/, runtime/podman/, runtime/k8s/, controller/, internal/admission/, internal/api/, internal/events/, internal/store/, internal/config/, internal/metrics/, cmd/cellad/, client/, api/openapi.yaml, docs/api.md, docs/manifest.md, docs/configuration.md, CHANGELOG.md]
effort: large
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Image volumes

## Overview

An evaluation runs a grader's checks in a sandbox of the task's image,
and the grader itself ships as an image of its own: a comparator, a
static tool. The check sandbox needs that image's files at a path,
read-only, on every executor, and a sandbox runs one image. [[019-volumes]]
answers it with a Volume whose source is an image, filled once and
mounted `shared-read` by any number of sandboxes. This slice builds the
part of 019 that answer needs: the `Volume` kind with an `empty` and an
`image` source, the fill, a sandbox's `spec.volumes`, the access table,
retain and delete, the Lost rule, the events, the routes, and the two
container drivers. A Volume's image passes the same admission step a
sandbox's image passes, so an installation whose sandboxes run catalog
images alone fills volumes from catalog images alone.

## Current state

Before this slice the contract named the kind and nothing built it:
`Capabilities.Volumes`, the `volume.*` actions of the authorizer and
their resource, the `VolumesAttached` condition, the `VolumeMissing`
reason, `volume_busy` in the error table, and conformance case
`case019VolumeLifecycle`, which skipped on every server.

## Design

### The kind

`manifest/v1` carries `Volume`, `VolumeSpec{Environment, Size, Class,
Access, Source{Kind, Snapshot, Image, Archive, SHA256}, Retain}` and
`VolumeStatus{ID, Owner, Phase, Reason, Message, AttachedTo, Capacity,
CreatedAt, UpdatedAt}`, ids `vol_` and a ULID. `manifest.DecodeVolume`
decodes it as every kind is decoded, and `manifest.ResolveVolume` holds
it to 019's field table where this slice serves the field:

| Field | Rule here |
|---|---|
| `size` | required, a positive quantity; an update that shrinks it is `invalid_field` and one that grows it `capability_unsupported`, since no driver resizes yet; the stored spelling is kept for an equal amount |
| `access` | `single` (default) or `shared-read`; immutable |
| `source.kind` | `empty` (default) or `image`; `snapshot` and `archive` are `capability_unsupported`; exactly the field the kind names, else `exclusive_fields`; immutable |
| `source.image` | required for `image`; after admission it is pinned by digest, `name@sha256:` and 64 hex digits, else `invalid_field`; stored as the pinned reference |
| `class` | refused when set, `capability_unsupported`: the operator picks the storage (below) |
| `retain` | `true` by default; mutable |
| `environment` | the default environment when absent; one whose driver declares `Volumes`, and for an image source `VolumeImages`, else `capability_unsupported`; immutable |

`v1.Capabilities` gains `volumeImages`: the driver fills a volume from an
image. `v1.PinnedImage` and `v1.ImageDigest` are the digest rule and the
digest a reader compares.

### The image is admitted as a sandbox's is

With an admission step configured, a create whose source is an image
calls it once, with a `Sandbox` that names the volume's environment,
its labels and `spec.image` set to the source, and nothing else, action
`create`, and `AdmitRequest.Volume` set to the Volume. The envelope of
[[007-admission]] gains the member `volume`, the Volume without its
status, and `null` on every Sandbox apply. Of the answer only
`spec.image` is read, and it becomes `spec.source.image`; a refusal is
`admission_refused` with the step's reason, as for a sandbox. An
endpoint that knows nothing of volumes decides on the image exactly as
it would for a sandbox, catalog and floating-tag rules included, and no
other rule of a sandbox's has a field to refuse on; one that knows reads
`volume` to tell the two apart. The digest rule runs after the step,
with or without one, so without a step the caller pins the image, and
with one a catalog name comes back pinned. An update that names the
image as the caller first wrote it is admitted again and accepted when
it pins the stored reference.

### The runtime

`runtime.VolumeDriver` is `CreateVolume(ctx, VolumeSpec) (Ref, error)`,
`InspectVolume(ctx, id) (VolumeState, error)` and `DeleteVolume(ctx, id)`.
`VolumeSpec{ID, Name, Owner, Size, Access, Source{Kind, Image}}` and
`VolumeState{ID, Phase, Reason, Message, Capacity}`. `CreateVolume` may
fill before it returns or after, is safe to call again (a volume made or
filling is `ErrAlreadyExists`, a half-filled one is filled again), and
reports a fill that failed before it returned as `*FillError{Reason,
Message}`; `ErrUnsupported` is a source the driver cannot apply. The
reasons are `SourceUnreachable`, `SourceTooLarge` and
`SourceUnsupported`. `CreateSpec.Volumes []Mount{Name, VolumeID, Path,
ReadOnly}` carries a sandbox's mounts. The native driver keeps no
volume and refuses a spec with a mount; the remote driver masks
`Volumes` and `VolumeImages`, since the worker stream carries no volume
operation yet, and refuses a mount too. `runtimetest.Volumes` wraps any
driver with volumes kept in memory, for a control plane's tests.

### The controller

`CreateVolume` writes the volume `Pending` and wakes `RunVolumes`, a
loop under the `volumes` lease whose pass hands every `Pending` and
`Deleting` volume to its driver in a goroutine of its own, with the
controller's lock released: a delete left to the loop is finished, and
a `Pending` volume is created and read. What the driver reports is
settled against the row as it then stands: `Available` with the
capacity, `Failed` with the reason and the message (`volume.failed`), a
volume deleted during its fill has what the fill made removed. A pass
after a restart takes a `Pending` volume again, which the drivers'
idempotent create makes safe. The reaper reads every `Available`
volume, and one its driver no longer holds is `Lost`, never recreated.

A sandbox's mounts are bound under the controller's lock in the create,
before its row is written: each to the volume it names (a `vol_` id, or
a name among the owner's own), in its environment, `Available` or
`Pending`, and attachable by 019's access table against the sandboxes
whose desired state already binds it, `Stopped` and `Failed` ones
included. A refusal is `volume_busy` with nothing written. The binding
is `status.volumes[] {name, volume, attached}`. A sandbox bound to a
`Pending` volume stays placed and the scheduler loop leaves it until the
fill settles, which wakes the loop; one bound to a volume that failed,
was lost or is gone fails `CreateFailed` with `VolumesAttached` `False`
before anything is pushed or minted. The driver's create carries the
mounts; its success marks each attached, writes `VolumesAttached`
`True`, and records `volume.attached`. A pool entry is never adopted by
a sandbox with mounts. `status.attachedTo` is derived at read. A delete
records `volume.detached` and marks a volume that does not retain and
that no other sandbox binds `Deleting`, which the loop finishes. A
volume any sandbox binds cannot be deleted (`volume_busy`). Recovery of
a lost sandbox whose volume is gone, failed or lost fails it
`VolumeMissing` rather than recreating it without the content.

### Storage, routes and events

The Volume is a row of the `objects` table with kind `Volume`, so the
durable store needed no migration; the snapshot store keeps a
`volumes` map. Each act journals with its row: `volume.created`,
`.updated`, `.attached` and `.detached` naming the sandbox in context
and the mode, `.failed` with its reason, `.deleted`; a status write is
journaled as `volume.status` and not delivered. The feed serves volume
records under `volume.list` and `volume.read`.

`PUT /v1/volumes/{name}` creates (`volume.create` and
`environment.use`, 201 `Pending` with its `Location`) or updates
(`volume.update`, 200); `GET` and `DELETE /v1/volumes/{name or id}`
answer `volume.read` and `volume.delete`; `GET /v1/volumes` lists under
`volume.list` with the owner and label selectors. A mount asks
`volume.attach` at resolve, and a refusal reads as `not_found`.
`errorEnvelope` answers `volume_busy` 409 and `path_conflict` 400. The
client gains `ApplyVolume`, `GetVolume`, `ListVolumes` and `KindVolume`.

### The drivers

podman: a volume is the named volume `cella-vol-<id>`, labeled with its
identity, and complete once a second, empty record volume exists, since
podman fixes labels at create. An image fill pulls the pinned image,
exports the root file system of a never-started container made from it,
re-frames the archive (directories, files, links; devices, fifos and
sockets dropped; names confined; regular-file bytes held to `size`,
else `SourceTooLarge`) and extracts it through the archive route into a
second never-started container that mounts the volume; both containers
are removed on every path. A pull that fails is `SourceUnreachable`.
`size` is advisory: a podman volume has no quota. A sandbox mounts each
volume read-only or read-write.

k8s: a volume is a PersistentVolumeClaim, `ReadWriteOnce` for every
access, on `CELLA_K8S_VOLUME_CLASS` or else the workspace class, with
its phase in an annotation. An empty volume is `Available` once the
claim exists, not once it is bound, since a class that binds on first
consumer leaves it `Pending` until a Pod uses it. An image fill is one
Pod: an init container from `CELLA_K8S_FILL_IMAGE`, an image whose
entrypoint is `cellad` (the release image is one), stages the static
binary into an `emptyDir` with `cellad fill stage`; the main container
runs the source image with `cellad fill copy / /cella-volume
--max-bytes <size>`, which copies the root file system, skipping every
path on another device (`/proc`, `/sys`, `/dev`, the kubelet's file
mounts, the mounts themselves) and every entry its non-root user cannot
read, and writes `SourceTooLarge: ...` to the termination message past
the cap. The image's own entrypoint never runs, so an image with no
shell or tar fills too. The Pod carries the sandbox node selector,
tolerations, pull secrets and hardened security context, no service
account token and no sandbox label; a pull that fails, or a fill past
its deadline, is `SourceUnreachable`. Without `CELLA_K8S_FILL_IMAGE` the
driver declares no `VolumeImages`. A sandbox's Pod mounts each claim
with `readOnly` on the volume and the mount, and a start re-renders the
mounts from the spec its claim records.

Every claim is `ReadWriteOnce`, so a `shared-read` volume is shared by
Pods of one node on a class whose volumes attach to one node at a time,
which block storage does. Pods on a second node wait on the attach.
Sharing across nodes is the class's property: a class whose driver
needs no node attach (an NFS or CephFS CSI driver) shares a
`ReadWriteOnce` claim across every node, and `CELLA_K8S_VOLUME_CLASS`
points volumes at one without moving workspaces.

## Not in this spec

| Item | Where it stays |
|---|---|
| `snapshot` and `archive` sources, `Snapshotter`, `CELLA_SOURCE_ALLOW`, `CELLA_MAX_SOURCE_BYTES` | [[019-volumes]] |
| a named `class`, growth through `ResizeVolume`, `CELLA_MAX_VOLUME_SIZE` | [[019-volumes]] |
| the control plane's writer, `Write`, which [[020-scheduling-and-sets]]'s results collection needs | [[019-volumes]] |
| `workspace.source: volume` and `workspace.volume` | [[019-volumes]], [[003-manifest-contract]] |
| `Change.Volumes`: adding and removing mounts while a sandbox is `Stopped` | [[005-lifecycle-controller]], [[019-volumes]] |
| volumes on the native, local, vm and remote drivers | [[004-runtime-contract]], [[021-data-plane-workers]] |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every field rule this slice serves has its refusing case, and absent fields take their defaults | `TestVolumeFieldRules`, `TestVolumeDefaults`, `TestDecodeVolume` | built |
| An image source passes the admission step through the Sandbox shape with the Volume beside it; the pinned reference the step answers is stored; a refusal is `admission_refused`; the digest rule holds with and without a step; the envelope carries `volume`, null on a Sandbox apply | `TestVolumeImageIsAdmitted`, `TestAVolumeImagePassesAdmission`, `TestAVolumeImageIsAdmittedThroughTheSandboxShape`, `TestEnvelopeMatchesTheEndpoint` | built |
| An update changes labels, annotations and retain, refuses every other field, refuses a shrink and a growth, and accepts the image as first written | `TestVolumeUpdate` | built |
| `spec.volumes` holds its name, volume and path rules, `path_conflict`, a volume of the environment that is `Available` or `Pending`, no write to `shared-read`, the capability, and no change on an update, while an update that keeps the mounts does not look their volumes up again, so a volume lost since refuses no change of labels; the corpus carries a mounted volume | `TestSandboxVolumeRules`, `TestGoldenCorpus`, `TestCorpusCoversTheSchema` | built |
| Boundary rule 4: a child mounts only what its parent mounts and never writes what its parent reads | `TestVolumeBoundary` | built |
| The access table at the controller, a refusal writing nothing, and a name reaching the caller's own volume alone | `TestAccessModes` | built |
| A stopped sandbox holds its volume against a delete; once it is gone a new writer attaches | `TestStoppedSandboxHoldsTheVolume` | built |
| A volume is `Pending` until filled; a fill still running stays `Pending`; a fill failed before the driver returned, and an unsupported source, are `Failed` with the reason; a pass after a restart reads a volume the driver holds; the loop takes a new volume at once | `TestFillLifecycle`, `TestRunVolumesFillsOnCreate` | built |
| A sandbox bound to a `Pending` volume waits and is created with it once filled, under the request that created it; one bound to a volume that failed fails with `VolumesAttached` `False` and nothing created | `TestAttachWaitsOnAvailable`, `TestAWaitingSandboxKeepsItsCreatesRequest` | built |
| retain keeps a volume after its sandbox; `retain: false` deletes it with its last sandbox; a delete removes it from the driver and the store; a delete during a fill removes what the fill made | `TestRetainAndDelete`, `TestADeleteDuringAFillRemovesWhatTheFillMade` | built |
| An `Available` volume its driver lost is `Lost` and never recreated; recovery of a sandbox whose volume is gone is `Failed VolumeMissing` | `TestVolumeLost`, `TestRecoveryFailsAVolumeMissing` | built |
| Every act is its record, an attach and a detach naming the sandbox and the mode, a failure its reason; the snapshot store's emitter and the durable store's bridge make one shape | `TestVolumeEvents`, `TestVolumeRecords`, `TestEmitVolumeJournalsTheAct`, `TestVolumeCollectionRoundTrips`, `TestAFailedVolumeCarriesItsReason` | built |
| A volume survives a restart on Postgres with no migration | `TestVolumesOutliveTheProcess` | built |
| The routes create, update, read, list and delete under their actions; a mount is given to the driver and holds the volume; `volume_busy` and `path_conflict` answer their rows; another subject's volume attaches by id only where `volume.attach` allows; an environment without volumes refuses both | `TestVolumeRoutes`, `TestASandboxMountsAVolume`, `TestAttachByIdUnderTheAuthorizer`, `TestVolumesRefusedWhereTheyAreNotKept`, `TestTheDocumentNamesTheStatusesAnswered` | built |
| The feed serves volume records under the volume actions | `TestRecordGateDecidesEachRecord` | built |
| The client's volume calls reach their routes | `TestTheRoutesOfEveryObjectCall`, `TestVolumesListLikeSandboxesDo` | built |
| The native and remote drivers refuse a mount, and the remote driver masks the volume capabilities | `TestVolumesNeedAStoreAndADriverThatKeepThem`, `TestVolumeCasesSkipWithoutTheCapability` | built |
| The driver conformance cases `VolumeLifecycle` and `AttachFailureIsClean` run on every driver that declares `Volumes`, pass on a keeper, skip without the capability, and fail a driver that declares it and keeps none | `TestVolumeCasesPassOnAKeeper`, `TestVolumeCasesSkipWithoutTheCapability`, `TestVolumeCasesCatchWhatTheyHoldTo`, `TestPodmanConformance` | built |
| podman: an empty volume and an image volume become `Available`; a second create is `ErrAlreadyExists`; a crash's half fill is refilled; the re-framed archive keeps files and links, drops devices, refuses names that leave the volume and stops at the size; each engine step that fails leaves nothing; a sandbox reads an image volume read-only and cannot write it; a delete under a container is refused and after it leaves nothing; no volume reads as a sandbox | `TestVolumesOnARealEngine`, `TestCreateVolumeFillsFromAnImage`, `TestCreateVolumeEmpty`, `TestCreateVolumeRefillsWhatACrashLeft`, `TestCreateVolumeFailures`, `TestCreateVolumeRefusesWhatNoDriverActsOn`, `TestReframeRootKeepsFilesAndDropsDevices`, `TestReframeRootRefusesWhatLeavesTheVolume`, `TestReframeRootStopsAtTheSize`, `TestReframeRootStopsWithItsContext`, `TestCreateMountsVolumes`, `TestCreateRefusesAMountItCannotHonor`, `TestVolumesNeverReadAsSandboxes`, `TestCapabilitiesDeclareVolumes` | built |
| k8s: the claim and the fill Pod render as designed and the claim owns the Pod; an empty volume is `Available` at once; each state of the fill Pod maps to its verdict, recorded on the claim; a create is idempotent and remakes a missing fill Pod; no fill image is `ErrUnsupported`; a delete removes the Pod and the claim; volume and sandbox names never meet; preflight names a missing volume class; a sandbox mounts its claims read-only where asked, again after a start; a mount of a volume not `Available` is refused with nothing made | `TestVolumeClaimAndFillPodRender`, `TestFillPodIsOwnedByItsClaim`, `TestEmptyVolumeIsAvailableAtOnce`, `TestFillVerdicts`, `TestFillMessageIsBounded`, `TestCreateVolumeIsIdempotent`, `TestCreateVolumeRefuses`, `TestDeleteVolume`, `TestVolumeAndSandboxNamesNeverMeet`, `TestPreflightChecksTheVolumeClass`, `TestSandboxMountsItsVolumes`, `TestMountPathsAreChecked`, `TestAttachNeedsAnAvailableVolume`, `TestVolumeCallsReportTheCluster`, `TestRecreatedFillReportsTheCluster`, `TestLoadK8sVolumes` | built |
| `cellad fill` stages its own binary and copies a root file system: modes, times and links kept, another device left out, unreadable entries skipped and counted, the byte cap (and a volume that fills up) a `SourceTooLarge:` termination message, every other failing step an error naming it, and a misuse exit 2 | `TestCopyKeepsWhatAnImageHolds`, `TestCopyStopsAtTheCap`, `TestCopyLeavesOutAnotherDevice`, `TestCopySkipsWhatItCannotRead`, `TestCopyRefusesARootThatIsNoDirectory`, `TestStageWritesAnExecutable`, `TestCopyReportsWhatItCannotWrite`, `TestStatDeviceNeedsAStatus`, `TestEveryFailureEndsTheCopy`, `TestEveryFailureEndsTheStage`, `TestFillDispatchesAndRefusesAMisuse`, `TestFillStagesAndCopies`, `TestFillLeavesItsReasonInTheTerminationLog` | built |

## Outcome

Built on 2026-10-18 as designed above, which differs from
[[019-volumes]] where 019 is now amended:

- `source.image` is stored pinned by digest, a rule 019 did not state: a
  volume is filled once and read back by what it holds, and a tag names
  whatever a registry points it at.
- The image passes the admission step through the Sandbox shape, which
  019 and [[007-admission]] had not given a Volume at all ("governed by
  the authorizer alone"); 007 now says so. No change to an existing
  endpoint is needed for the catalog rule to hold.
- `class` is refused rather than validated against a class list the
  driver knows, and the k8s driver requests `ReadWriteOnce` for every
  access rather than a `ReadOnlyMany` class for `shared-read`:
  block storage offers no `ReadOnlyMany`, and the fill writes the claim.
  `CELLA_K8S_VOLUME_CLASS` is the operator's lever instead.
- 019's attach race (`Failed VolumeBusy` after a 201) does not arise: the
  create binds the mounts under the controller's lock before it writes
  or answers, so the losing create is refused `volume_busy`.
- `status.message` carries the driver's sentence on a failed fill.
- A `Pending` volume is mountable and the sandbox waits on it, which is
  019's "every attach waits on Available" placed in the scheduler loop.

Points the drivers settled while they were built. On k8s the claim is
`volume.<name>` and the fill Pod `fill.<name>`, named from the volume
id with a dot no sandbox's claim carries, so the two never meet; the
fill Pod sets its hostname for that reason, and the claim owns it. The
copier's flag comes before its paths (`cellad fill copy --max-bytes N /
/cella-volume`), it replaces what it finds in the destination so a fill
Pod remade after a node loss refills a half-written claim, and a
container that cannot start (`StartError`, `ContainerCannotRun`, an
image built for another architecture) is `SourceUnsupported`. The k8s
and podman sandbox creates read each mounted volume and refuse one not
`Available` with nothing made, which is what the conformance case
`AttachFailureIsClean` holds them to. The file-transfer helper Pod on
k8s mounts no volume, so it is not held to a volume's node. On podman a
fill target mounts the volume with `nocopy`, and so does a sandbox, so
an empty volume stays empty under an image's own directory, and a
volume's delete is refused by the engine while any container mounts it.
The Role needs no new verb: the volume code uses the claim and Pod
verbs the sandbox code already has.

The kind tier runs `VolumeLifecycle` and `AttachFailureIsClean` against
the k8s driver; its conformance run sets no fill image, so the image
fill is held there by the hermetic tests alone.
