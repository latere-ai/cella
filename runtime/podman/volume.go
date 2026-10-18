// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// A Volume of spec 019 on this driver is two named volumes. The data volume
// cella-vol-<id> holds the files and the identity, as labels. The record
// volume cella-volrec-<id> is empty and is created only once the fill is
// complete, which is how a later read, in this process or another, tells a
// filled volume from one a fill is still writing or a crash left half
// written: podman fixes a volume's labels at create, so completion cannot be
// a label written afterwards.
//
// The size is advisory here. Podman's local volume driver keeps no quota, so
// a sandbox that mounts a volume read-write may write past it; what this
// driver holds to the size is the content an image fill copies in.
const (
	kindVolume       = "volume"
	kindVolumeRecord = "volume-record"
	kindFill         = "volume-fill"
	labelSize        = prefix + "size"
	labelAccess      = prefix + "access"
)

// fillMount is where the fill's target container mounts the data volume, and
// the path its archive is extracted below.
const fillMount = "/cella-fill"

// fillCommand is the command both fill containers are created with. Neither
// ever starts, so it need not exist in the image; it is there because the
// engine refuses to create a container from an image that names no command
// of its own, which a static program's image often does not.
var fillCommand = []string{"/cella-fill-never-runs"}

// controlPrefix is the control plane's own mount point inside a sandbox, where
// the token and the trust file are projected. No volume is mounted at or
// under it, and none over it.
var controlPrefix = path.Dir(driver.TokenPath)

func dataVolume(id string) string   { return "cella-vol-" + id }
func volumeRecord(id string) string { return "cella-volrec-" + id }
func fillSource(id string) string   { return "cella-fill-src-" + id }
func fillTarget(id string) string   { return "cella-fill-dst-" + id }

var _ driver.VolumeDriver = (*Driver)(nil)

// CreateVolume makes the data volume and fills it before it returns: an empty
// source fills at once, and an image source is the image's root file system
// copied in through two containers that never start. A volume this driver has
// already filled is ErrAlreadyExists; one a previous process left without its
// record is removed and filled again from the start. A fill that fails
// removes the data volume and returns a *runtime.FillError where the cause is
// one of spec 019's reasons.
func (d *Driver) CreateVolume(ctx context.Context, spec driver.VolumeSpec) (driver.Ref, error) {
	if err := ctx.Err(); err != nil {
		return driver.Ref{}, err
	}
	if err := spec.Check(); err != nil {
		return driver.Ref{}, err
	}
	if !validID.MatchString(spec.ID) {
		return driver.Ref{}, fmt.Errorf("%w: volume id %q", driver.ErrInvalid, spec.ID)
	}
	limit, err := volumeBytes(spec.Size)
	if err != nil {
		return driver.Ref{}, err
	}
	id := spec.ID
	unlock := d.lock(id)
	defer unlock()
	filled, err := d.volumeFilled(ctx, id)
	switch {
	case err == nil && filled:
		return driver.Ref{}, driver.ErrAlreadyExists
	case err == nil, errors.Is(err, driver.ErrNotFound):
		// A data volume with no record is a fill a previous process did not
		// finish, and a record with no data volume a delete it did not
		// finish. Neither is trusted: both go before the fill begins.
		if err := d.removeVolumeObjects(ctx, id); err != nil {
			return driver.Ref{}, err
		}
	default:
		return driver.Ref{}, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	labels := map[string]string{
		labelID: id, labelKind: kindVolume, labelSize: spec.Size, labelAccess: spec.Access,
		labelCreatedAt: now.Format(time.RFC3339Nano),
	}
	for key, value := range map[string]string{labelName: spec.Name, labelOwner: spec.Owner, labelImage: spec.Source.Image} {
		if value != "" {
			labels[key] = value
		}
	}
	if err := d.createVolume(ctx, dataVolume(id), labels); err != nil {
		if conflict(err) {
			return driver.Ref{}, driver.ErrAlreadyExists
		}
		return driver.Ref{}, fmt.Errorf("podman: creating the volume %s: %w", id, err)
	}
	if spec.Source.Kind == driver.SourceImage {
		if err := d.fillFromImage(ctx, spec, limit); err != nil {
			return driver.Ref{}, errors.Join(err, d.removeDataVolume(context.WithoutCancel(ctx), id))
		}
	}
	if err := d.createVolume(ctx, volumeRecord(id), map[string]string{labelID: id, labelKind: kindVolumeRecord}); err != nil {
		return driver.Ref{}, errors.Join(fmt.Errorf("podman: recording the filled volume %s: %w", id, err),
			d.removeDataVolume(context.WithoutCancel(ctx), id))
	}
	return driver.Ref{ID: id}, nil
}

// volumeFilled reports whether the data volume exists and, if it does,
// whether its record does. A data volume that is absent is ErrNotFound.
func (d *Driver) volumeFilled(ctx context.Context, id string) (bool, error) {
	if _, err := d.inspectVolume(ctx, dataVolume(id)); err != nil {
		return false, err
	}
	_, err := d.inspectVolume(ctx, volumeRecord(id))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, driver.ErrNotFound):
		return false, nil
	}
	return false, err
}

// InspectVolume reads one volume back: Available once its record exists, at
// the size it was created with, and Pending while a fill is still writing it.
func (d *Driver) InspectVolume(ctx context.Context, id string) (driver.VolumeState, error) {
	if err := ctx.Err(); err != nil {
		return driver.VolumeState{}, err
	}
	if !validID.MatchString(id) {
		return driver.VolumeState{}, fmt.Errorf("%w: volume id %q", driver.ErrInvalid, id)
	}
	vi, err := d.inspectVolume(ctx, dataVolume(id))
	if err != nil {
		return driver.VolumeState{}, err
	}
	state := driver.VolumeState{ID: id, Phase: driver.VolumePending}
	_, err = d.inspectVolume(ctx, volumeRecord(id))
	switch {
	case err == nil:
		state.Phase, state.Capacity = driver.VolumeAvailable, vi.Labels[labelSize]
	case !errors.Is(err, driver.ErrNotFound):
		return driver.VolumeState{}, err
	}
	return state, nil
}

// DeleteVolume removes the fill's containers, the data volume and the record.
// The data volume is removed without force, so one a sandbox's container
// still mounts, a stopped one included, is refused by the engine rather than
// taken from under it together with that container. A volume already gone is
// not an error.
func (d *Driver) DeleteVolume(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID.MatchString(id) {
		return fmt.Errorf("%w: volume id %q", driver.ErrInvalid, id)
	}
	unlock := d.lock(id)
	defer unlock()
	return d.removeVolumeObjects(ctx, id)
}

// removeVolumeObjects removes everything one volume is made of, data before
// record, so a removal the engine refuses leaves the volume reading as it
// did rather than as a fill in progress.
func (d *Driver) removeVolumeObjects(ctx context.Context, id string) error {
	if err := d.removeFillContainers(ctx, id); err != nil {
		return err
	}
	if err := d.removeDataVolume(ctx, id); err != nil {
		return err
	}
	if err := d.removeVolume(ctx, volumeRecord(id)); err != nil {
		return fmt.Errorf("podman: removing the record of the volume %s: %w", id, err)
	}
	return nil
}

// removeDataVolume removes the data volume without force. Absent is not an
// error.
func (d *Driver) removeDataVolume(ctx context.Context, id string) error {
	err := d.client().json(ctx, http.MethodDelete, "/volumes/"+dataVolume(id), nil, nil)
	if err == nil || notFound(err) {
		return nil
	}
	return fmt.Errorf("podman: removing the volume %s: %w", id, err)
}

// removeFillContainers removes the two containers a fill makes. Absent is
// not an error.
func (d *Driver) removeFillContainers(ctx context.Context, id string) error {
	for _, name := range []string{fillSource(id), fillTarget(id)} {
		err := d.client().json(ctx, http.MethodDelete, "/containers/"+name+"?"+query("force", "true", "timeout", "0"), nil, nil)
		if err != nil && !notFound(err) {
			return fmt.Errorf("podman: removing the fill container %s: %w", name, err)
		}
	}
	return nil
}

// fillFromImage copies an image's root file system into the data volume. The
// image is pulled first, a pull that fails being SourceUnreachable. A source
// container made from the image is exported, which is the engine's flattened
// root file system with no whiteout left to resolve, and the export is
// extracted below fillMount of a target container that mounts the data
// volume there. Neither container starts, so the image's own entrypoint never
// runs, and both are removed on every path.
func (d *Driver) fillFromImage(ctx context.Context, spec driver.VolumeSpec, limit int64) (err error) {
	id := spec.ID
	if _, err := d.imageDigest(ctx, spec.Source.Image); err != nil {
		return &driver.FillError{Reason: driver.ReasonSourceUnreachable, Message: err.Error()}
	}
	defer func() {
		err = errors.Join(err, d.removeFillContainers(context.WithoutCancel(ctx), id))
	}()
	labels := map[string]string{labelID: id, labelKind: kindFill}
	source := specGenerator{Image: spec.Source.Image, Name: fillSource(id), Command: fillCommand, Labels: labels}
	if err := d.client().json(ctx, http.MethodPost, "/containers/create", source, nil); err != nil {
		return fmt.Errorf("podman: creating the fill's source container for %s: %w", id, err)
	}
	// nocopy keeps the engine from copying whatever the image holds at the
	// mount point into the volume when the container is mounted, so the
	// volume holds the export and nothing else.
	target := specGenerator{Image: spec.Source.Image, Name: fillTarget(id), Command: fillCommand, Labels: labels,
		Volumes: []namedVolume{{Name: dataVolume(id), Dest: fillMount, Options: []string{"rw", "nocopy"}}}}
	if err := d.client().json(ctx, http.MethodPost, "/containers/create", target, nil); err != nil {
		return fmt.Errorf("podman: creating the fill's target container for %s: %w", id, err)
	}
	return d.copyRoot(ctx, id, limit)
}

// copyRoot streams the source container's export into the target container's
// mount of the data volume, through reframeRoot, which drops what a volume
// cannot hold and stops at the size.
func (d *Driver) copyRoot(ctx context.Context, id string, limit int64) error {
	resp, err := d.client().do(ctx, http.MethodGet, d.client().libpodURL("/containers/"+fillSource(id)+"/export"), nil)
	if err != nil {
		return fmt.Errorf("podman: exporting the image of the volume %s: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp); err != nil {
		return fmt.Errorf("podman: exporting the image of the volume %s: %w", id, err)
	}
	pr, pw := io.Pipe()
	var produce error
	done := make(chan struct{})
	go func() {
		defer close(done)
		produce = reframeRoot(ctx, resp.Body, pw, limit)
		_ = pw.CloseWithError(produce)
	}()
	defer func() {
		_ = pr.Close()
		<-done
	}()
	url := d.client().compatURL("/containers/" + fillTarget(id) + "/archive?" + query("path", fillMount))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	put, err := d.client().hc.Do(req)
	if err == nil {
		defer func() { _ = put.Body.Close() }()
		if err = statusErr(put); err == nil {
			_, err = io.Copy(io.Discard, put.Body)
		}
	}
	// The engine has answered, so what it did not read of the archive is not
	// wanted: the copy is stopped rather than left writing to a reader that
	// is gone, and then waited for.
	_ = pr.Close()
	<-done
	switch {
	case produce != nil && !errors.Is(produce, io.ErrClosedPipe):
		// The archive failing is why the request failed, or why it would
		// have: that is the cause to report.
		return produce
	case err != nil:
		return fmt.Errorf("podman: filling the volume %s: %w", id, err)
	case produce != nil:
		return fmt.Errorf("podman: the engine answered the fill of %s before it read the whole image", id)
	}
	return nil
}

// reframeRoot copies an image's exported root file system entry by entry.
// Directories, regular files, symbolic links and hard links are kept, with
// their modes, owners and times; device nodes, fifos and sockets are dropped,
// since a volume holds files and the sandbox makes its own devices; extended
// attributes are dropped, since a capability a file carried in an image is
// not one a mounted volume grants. A name or a hard link's target that leaves
// the volume is SourceUnsupported, and regular files past limit bytes in
// total are SourceTooLarge. A symbolic link's target is kept as written: it
// is followed inside the sandbox that reads it, where the image's absolute
// links point into that sandbox's own root.
func reframeRoot(ctx context.Context, src io.Reader, dst io.Writer, limit int64) error {
	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return fmt.Errorf("podman: reading the image's root file system: %w", err)
		}
		name, ok := rootEntryName(h.Name)
		if !ok {
			return &driver.FillError{Reason: driver.ReasonSourceUnsupported,
				Message: fmt.Sprintf("the image holds the entry %q, which leaves the volume", h.Name)}
		}
		out := &tar.Header{
			Name: name, Mode: h.Mode, Uid: h.Uid, Gid: h.Gid, ModTime: h.ModTime, Typeflag: h.Typeflag,
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if name == "" {
				continue
			}
			out.Name = name + "/"
		case tar.TypeReg:
			if name == "" {
				return &driver.FillError{Reason: driver.ReasonSourceUnsupported, Message: "the image's root is a file"}
			}
			if h.Size < 0 || h.Size > limit-total {
				return &driver.FillError{Reason: driver.ReasonSourceTooLarge,
					Message: fmt.Sprintf("the image's files hold more than the volume's %d bytes", limit)}
			}
			total += h.Size
			out.Size = h.Size
		case tar.TypeSymlink:
			out.Linkname = h.Linkname
		case tar.TypeLink:
			target, ok := rootEntryName(h.Linkname)
			if !ok || target == "" {
				return &driver.FillError{Reason: driver.ReasonSourceUnsupported,
					Message: fmt.Sprintf("the image holds a hard link to %q, which leaves the volume", h.Linkname)}
			}
			out.Linkname = target
		default:
			continue
		}
		if name == "" {
			continue
		}
		if err := tw.WriteHeader(out); err != nil {
			return err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, out.Size); err != nil {
				return fmt.Errorf("podman: copying %s from the image: %w", name, err)
			}
		}
	}
}

// rootEntryName is an export's entry name relative to the volume's root, with
// the leading slash or dot an engine may write taken off. The root itself is
// the empty name. A name that climbs out of the root is not ok.
func rootEntryName(raw string) (string, bool) {
	name := strings.TrimLeft(raw, "/")
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return "", true
	}
	if !fs.ValidPath(name) || strings.Contains(name, "\x00") {
		return "", false
	}
	return name, true
}

// volumeBytes is a volume's size in bytes, from its quantity.
func volumeBytes(size string) (int64, error) {
	milli, err := manifest.ParseQuantity(v1.Quantity(size))
	if err != nil {
		return 0, fmt.Errorf("%w: volume size: %w", driver.ErrInvalid, err)
	}
	if milli <= 0 {
		return 0, fmt.Errorf("%w: volume size %q is not positive", driver.ErrInvalid, size)
	}
	return milli / 1000, nil
}

// volumeMounts are a sandbox's volumes as the container is created with them.
// Each path is clean, absolute and below the root, and is neither the
// workspace, another mount, nor the control plane's own prefix, nor inside or
// over any of them. Each volume is one this driver filled: a mount of a
// volume with no record would mount a fill still writing, or none at all.
// nocopy keeps the engine from copying what the image holds at the path into
// the volume, so an empty volume is empty.
func (d *Driver) volumeMounts(ctx context.Context, root string, mounts []driver.Mount) ([]namedVolume, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	taken := []string{root, controlPrefix}
	out := make([]namedVolume, 0, len(mounts))
	for _, m := range mounts {
		p := m.Path
		if p == "" || strings.Contains(p, "\x00") || !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
			return nil, fmt.Errorf("%w: mount path %q", driver.ErrInvalid, p)
		}
		for _, other := range taken {
			if nests(p, other) {
				return nil, fmt.Errorf("%w: the mount at %s overlaps %s", driver.ErrInvalid, p, other)
			}
		}
		taken = append(taken, p)
		if !validID.MatchString(m.VolumeID) {
			return nil, fmt.Errorf("%w: volume id %q", driver.ErrInvalid, m.VolumeID)
		}
		filled, err := d.volumeFilled(ctx, m.VolumeID)
		switch {
		case errors.Is(err, driver.ErrNotFound), err == nil && !filled:
			return nil, fmt.Errorf("%w: the volume %s is not filled on this engine", driver.ErrInvalid, m.VolumeID)
		case err != nil:
			return nil, err
		}
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		out = append(out, namedVolume{Name: dataVolume(m.VolumeID), Dest: p, Options: []string{mode, "nocopy"}})
	}
	return out, nil
}

// nests reports whether two clean absolute paths are equal or one is inside
// the other.
func nests(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
