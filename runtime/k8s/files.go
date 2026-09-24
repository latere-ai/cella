// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// MaxArchiveBytes limits the sum of file bytes accepted per import.
const MaxArchiveBytes int64 = 256 << 20

// labelHelper marks the short-lived Pod that carries a transfer for a stopped
// sandbox. It is not the sandbox's id label, so a helper is never mistaken for
// the sandbox's own Pod.
const labelHelper = prefix + "helper-of"

// tailBytes is how much of a failed transfer's diagnostic output is kept.
const tailBytes = 2048

// ExportTar writes a tar of the named paths. The archive the container's tar
// produces is rewritten on the way out: names are workspace-relative, with no
// leading "./" and no trailing slash on a directory, which is the one shape
// every driver of this contract writes.
func (d *Driver) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	spec, err := d.transferSpec(ctx, id)
	if err != nil {
		return err
	}
	root := workspacePath(spec)
	relative := []string{"."}
	if len(paths) > 0 {
		relative = relative[:0]
		for _, p := range paths {
			rel, err := relativeTo(p, root)
			if err != nil {
				return err
			}
			relative = append(relative, rel)
		}
	}
	argv := append([]string{"tar", "-C", root, "-cf", "-", "--"}, relative...)
	return d.transfer(ctx, id, spec, execOpts{argv: argv}, nil, func(r io.Reader) error {
		return rewrite(r, dst)
	})
}

// ImportTar unpacks an archive below dest. Every entry is read here before it
// reaches the container: an absolute name, a traversal, a link of either kind
// and a special file are refused, so the archive cannot write outside the
// directory the caller named.
func (d *Driver) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	spec, err := d.transferSpec(ctx, id)
	if err != nil {
		return err
	}
	root := workspacePath(spec)
	if _, err := relativeTo(dest, root); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var (
		once     sync.Once
		refusal  error
		recorded = func(err error) { once.Do(func() { refusal = err }) }
	)
	go func() {
		err := sanitize(src, pw)
		if err != nil {
			recorded(err)
		}
		_ = pw.CloseWithError(err)
	}()
	// The destination may not exist yet, and tar refuses a missing -C.
	argv := []string{"sh", "-c", `mkdir -p "$0" && exec tar -C "$0" -xpf -`, dest}
	err = d.transfer(ctx, id, spec, execOpts{argv: argv, stdin: true}, pr, nil)
	_, _ = io.Copy(io.Discard, pr)
	_ = pr.Close()
	if refusal != nil {
		return refusal
	}
	return err
}

// transferSpec is the read both transfers share. Files is declared, so a
// stopped sandbox is a legal subject and only a missing claim is not.
func (d *Driver) transferSpec(ctx context.Context, id string) (driver.CreateSpec, error) {
	pvc, err := d.getClaim(ctx, id)
	if err != nil {
		return driver.CreateSpec{}, err
	}
	return specOf(pvc)
}

// transfer runs one archive command in the sandbox's container, or, when the
// sandbox is stopped, in a helper Pod that mounts the same claim and is
// deleted afterwards. read consumes the command's output when the caller
// wants it.
func (d *Driver) transfer(ctx context.Context, id string, spec driver.CreateSpec, o execOpts, stdin io.Reader, read func(io.Reader) error) error {
	if d.stream == nil {
		return fmt.Errorf("%w: this driver was built without a cluster connection", driver.ErrUnsupported)
	}
	pod, release, err := d.transferPod(ctx, id, spec)
	if err != nil {
		return err
	}
	defer release()

	var diagnostic tail
	if read == nil {
		return d.wrapExec(d.stream.stream(ctx, pod, o, stdin, &diagnostic, &diagnostic), &diagnostic)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := d.stream.stream(ctx, pod, o, stdin, pw, &diagnostic)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	readErr := read(pr)
	_, _ = io.Copy(io.Discard, pr)
	_ = pr.Close()
	if err := d.wrapExec(<-done, &diagnostic); err != nil {
		return err
	}
	return readErr
}

func (d *Driver) wrapExec(err error, diagnostic *tail) error {
	if err == nil {
		return nil
	}
	if text := strings.TrimSpace(diagnostic.String()); text != "" {
		return fmt.Errorf("archive transfer: %w: %s", err, text)
	}
	return fmt.Errorf("archive transfer: %w", err)
}

// transferPod names the Pod a transfer runs in. A running sandbox is its own;
// a stopped one gets a helper built from its own image with the same hardening
// and the same claim, which is deleted when the transfer ends.
func (d *Driver) transferPod(ctx context.Context, id string, spec driver.CreateSpec) (string, func(), error) {
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return "", nil, err
	}
	if pod != nil && pod.DeletionTimestamp == nil {
		if _, _, ended := terminated(pod); !ended {
			return pod.Name, func() {}, nil
		}
	}
	helper, err := d.helperPod(id, spec)
	if err != nil {
		return "", nil, err
	}
	// The helper runs the sandbox's image, so it runs under the sandbox's
	// rule, written again here for a sandbox stopped before it had one.
	if err := d.putSandboxRule(ctx, id, spec.Mesh.ID); err != nil {
		return "", nil, err
	}
	release := func() {
		clean := context.WithoutCancel(ctx)
		_ = d.deletePodNamed(clean, helper.Name)
	}
	if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Create(ctx, helper, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", nil, fmt.Errorf("helper pod create: %w", err)
		}
		// A helper the previous transfer left behind is not reused: its
		// readiness is unknown and its deletion may be under way.
		if err := d.deletePodNamed(ctx, helper.Name); err != nil {
			return "", nil, err
		}
		if _, err := d.cs.CoreV1().Pods(d.opts.Namespace).Create(ctx, helper, metav1.CreateOptions{}); err != nil {
			return "", nil, fmt.Errorf("helper pod create: %w", err)
		}
	}
	if err := d.waitPodReady(ctx, helper.Name); err != nil {
		release()
		return "", nil, err
	}
	return helper.Name, release, nil
}

// helperPod renders the transfer Pod: the sandbox's image and baseline, its
// claim, and a command that waits. It carries no id label, so List never reads
// it as the sandbox's Pod, and the sandbox label, so the sandbox's network
// rule confines it as it confines the sandbox.
func (d *Driver) helperPod(id string, spec driver.CreateSpec) (*corev1.Pod, error) {
	// It carries no identity and no gateway either: a transfer reaches the
	// claim's files and never the sandbox's token, credential or authority.
	bare := spec
	bare.Egress = driver.Egress{}
	pod, err := d.pod(bare, d.opts.Now().UTC(), false)
	if err != nil {
		return nil, err
	}
	pod.Name = objectName(id) + "-files"
	pod.Labels = map[string]string{labelManagedBy: managedValue, labelHelper: id, labelSandbox: id}
	pod.Annotations = map[string]string{annOwner: spec.Owner, annImage: spec.Image}
	pod.Spec.Containers[0].Command = keepAlive
	pod.Spec.Containers[0].Args = nil
	pod.Spec.Containers[0].Env = nil
	return pod, nil
}

// relativeTo turns an absolute path inside the workspace into the name tar
// takes after -C, and refuses anything outside it.
func relativeTo(p, root string) (string, error) {
	if err := checkPath(p); err != nil {
		return "", err
	}
	if !under(p, root) {
		return "", fmt.Errorf("%w: %q is outside the workspace %q", driver.ErrInvalid, p, root)
	}
	if p == root {
		return ".", nil
	}
	return strings.TrimPrefix(p, strings.TrimSuffix(root, "/")+"/"), nil
}

// rewrite copies one archive to another, normalizing each name and refusing
// what the contract does not carry: a link, a device, a socket.
func rewrite(src io.Reader, dst io.Writer) error {
	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name := normalize(header.Name)
		if name == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			header.Size = 0
		case tar.TypeReg:
		default:
			return fmt.Errorf("%w: archive entry %q is not a file or a directory", driver.ErrInvalid, header.Name)
		}
		header.Name = name
		header.Uname, header.Gname = "", ""
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// normalize is the one name shape of this contract: relative to the
// workspace, no "./" prefix, no trailing slash.
func normalize(name string) string {
	name = strings.TrimSuffix(name, "/")
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == "" {
		return ""
	}
	return name
}

// sanitize copies an archive entry by entry, refusing everything the
// destination must not receive, and is what makes an untrusted archive safe to
// hand to tar inside the container.
func sanitize(src io.Reader, dst io.Writer) error {
	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)
	var total int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tw.Close()
		}
		if err != nil {
			return fmt.Errorf("%w: reading the archive: %w", driver.ErrInvalid, err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !fs.ValidPath(name) || name == "." || strings.Contains(name, `\`) || strings.Contains(name, "\x00") {
			return fmt.Errorf("%w: archive path %q", driver.ErrInvalid, header.Name)
		}
		out := tar.Header{Name: name, Mode: header.Mode & 0o777, ModTime: header.ModTime, Typeflag: header.Typeflag}
		switch header.Typeflag {
		case tar.TypeDir:
			out.Name = name + "/"
		case tar.TypeReg:
			if header.Size < 0 || header.Size > MaxArchiveBytes-total {
				return fmt.Errorf("%w: archive size", driver.ErrInvalid)
			}
			total += header.Size
			out.Size = header.Size
		default:
			// A symbolic or hard link would resolve inside the container,
			// where the check this side made no longer holds.
			return fmt.Errorf("%w: archive entry %q is a link or a special file", driver.ErrInvalid, header.Name)
		}
		if out.ModTime.IsZero() {
			out.ModTime = time.Unix(0, 0)
		}
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if out.Size > 0 {
			if _, err := io.CopyN(tw, tr, out.Size); err != nil {
				return err
			}
		}
	}
}

// tail keeps the last of a stream's diagnostic output, bounded, so a failed
// transfer says what the container said without holding the stream.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := tailBytes - len(t.buf); room > 0 {
		t.buf = append(t.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
