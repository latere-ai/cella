// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// entry is one archive member, as a container's tar writes it.
type entry struct {
	name, body, link string
	mode             int64
	typ              byte
	size             int64
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		if typ != tar.TypeReg {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: mode, Size: size, Typeflag: typ, Linkname: e.link}); err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A truncated member is deliberate in the size case, so the close error is
	// the writer's complaint about it and not the test's problem.
	_ = tw.Close()
	return b.Bytes()
}

func read(t *testing.T, r io.Reader) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = entry{name: header.Name, body: string(body), mode: header.Mode, typ: header.Typeflag}
	}
}

func TestArchiveNames(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_export"
	h.created(t, spec(id))
	// The container's tar writes directories with a trailing slash and, for a
	// whole-directory archive, every name below "./".
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, _ io.Writer) error {
		_, err := stdout.Write(archive(t,
			entry{name: "./", typ: tar.TypeDir, mode: 0o755},
			entry{name: "./tree/", typ: tar.TypeDir, mode: 0o755},
			entry{name: "./tree/hello.txt", body: "hello", mode: 0o640},
			entry{name: "./tree/bin/", typ: tar.TypeDir, mode: 0o755},
			entry{name: "./tree/bin/run.sh", body: "#!/bin/sh\n", mode: 0o755},
		))
		return err
	}
	var out bytes.Buffer
	if err := h.ExportTar(t.Context(), id, nil, &out); err != nil {
		t.Fatal(err)
	}
	got := read(t, &out)
	names := slices.Sorted(maps.Keys(got))
	want := []string{"tree", "tree/bin", "tree/bin/run.sh", "tree/hello.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if e := got["tree/hello.txt"]; e.body != "hello" || e.mode&0o777 != 0o640 {
		t.Fatalf("file %+v", e)
	}
	if e := got["tree"]; e.typ != tar.TypeDir {
		t.Fatalf("directory entry %+v", e)
	}
	// A whole-workspace archive is taken from the workspace root.
	if argv := h.exec.last().argv; !reflect.DeepEqual(argv, []string{"tar", "-C", driver.DefaultWorkdir, "-cf", "-", "--", "."}) {
		t.Fatalf("argv %q", argv)
	}
	out.Reset()
	if err := h.ExportTar(t.Context(), id, []string{driver.DefaultWorkdir + "/tree", driver.DefaultWorkdir}, &out); err != nil {
		t.Fatal(err)
	}
	if argv := h.exec.last().argv; !reflect.DeepEqual(argv, []string{"tar", "-C", driver.DefaultWorkdir, "-cf", "-", "--", "tree", "."}) {
		t.Fatalf("argv %q", argv)
	}
}

func TestExportRefusesWhatIsNotTheWorkspace(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_exportrefuse"
	h.created(t, spec(id))
	for _, path := range []string{"/etc", driver.DefaultWorkdir + "/../etc", "relative", "/"} {
		if err := h.ExportTar(t.Context(), id, []string{path}, io.Discard); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("ExportTar %q = %v, want ErrInvalid", path, err)
		}
	}
	if h.exec.count() != 0 {
		t.Fatal("a refused path still opened a command in the container")
	}
}

func TestExportRefusesAnEntryThatIsNoFile(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_exportlink"
	h.created(t, spec(id))
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, _ io.Writer) error {
		_, err := stdout.Write(archive(t, entry{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"}))
		return err
	}
	if err := h.ExportTar(t.Context(), id, nil, io.Discard); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("ExportTar of a link = %v, want ErrInvalid", err)
	}
}

func TestImportSanitizesAndPipes(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_import"
	h.created(t, spec(id))
	var arrived map[string]entry
	h.exec.handle = func(_ context.Context, _ execCall, stdin io.Reader, _, _ io.Writer) error {
		arrived = read(t, stdin)
		return nil
	}
	body := archive(t,
		entry{name: "tree/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "tree/hello.txt", body: "hello", mode: 0o640},
	)
	if err := h.ImportTar(t.Context(), id, driver.DefaultWorkdir+"/sub", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if len(arrived) != 2 || arrived["tree/hello.txt"].body != "hello" {
		t.Fatalf("what reached the container: %+v", arrived)
	}
	if got := arrived["tree/hello.txt"].mode & 0o777; got != 0o640 {
		t.Fatalf("mode %o", got)
	}
	// The destination is made before the archive is unpacked, and the archive
	// never reaches a shell as text.
	want := []string{"sh", "-c", `mkdir -p "$0" && exec tar -C "$0" -xpf -`, driver.DefaultWorkdir + "/sub"}
	if argv := h.exec.last().argv; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv %q, want %q", argv, want)
	}
}

func TestImportRefuses(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_importrefuse"
	h.created(t, spec(id))
	for _, tc := range []struct {
		name  string
		entry entry
	}{
		{"traversal", entry{name: "../escape", body: "x"}},
		{"absolute", entry{name: "/abs", body: "x"}},
		{"symbolic link", entry{name: "link", typ: tar.TypeSymlink, link: "../../escape"}},
		{"hard link", entry{name: "hard", typ: tar.TypeLink, link: "tree/hello.txt"}},
		{"device", entry{name: "dev", typ: tar.TypeChar}},
		{"fifo", entry{name: "pipe", typ: tar.TypeFifo}},
		{"the directory itself", entry{name: ".", typ: tar.TypeDir}},
		{"a windows separator", entry{name: `a\b`, body: "x"}},
		{"too large", entry{name: "big", size: MaxArchiveBytes + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := h.ImportTar(t.Context(), id, driver.DefaultWorkdir, bytes.NewReader(archive(t, tc.entry)))
			if !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("ImportTar = %v, want ErrInvalid", err)
			}
		})
	}
	for _, dest := range []string{"/tmp", "/", driver.DefaultWorkdir + "/../etc", "relative"} {
		if err := h.ImportTar(t.Context(), id, dest, bytes.NewReader(archive(t))); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("ImportTar into %q = %v, want ErrInvalid", dest, err)
		}
	}
	if err := h.ImportTar(t.Context(), id, driver.DefaultWorkdir, strings.NewReader("not an archive at all")); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("ImportTar of a body that is no archive = %v, want ErrInvalid", err)
	}
}

func TestFilesWhileStopped(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_stoppedfiles"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var ran string
	h.exec.handle = func(_ context.Context, c execCall, stdin io.Reader, stdout, _ io.Writer) error {
		ran = c.pod
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		_, _ = stdout.Write(archive(t, entry{name: "kept.txt", body: "kept"}))
		return nil
	}
	if err := h.ImportTar(t.Context(), id, driver.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "kept.txt", body: "kept"}))); err != nil {
		t.Fatalf("ImportTar while stopped: %v", err)
	}
	helper := objectName(id) + "-files"
	if ran != helper {
		t.Fatalf("the transfer ran in %q, want the helper %q", ran, helper)
	}
	if _, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), helper, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the helper survived the transfer: %v", err)
	}
	// The sandbox is still stopped: the helper was never its Pod.
	state, err := h.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != driver.Stopped {
		t.Fatalf("phase %q after a transfer in a helper", state.Phase)
	}
	var out bytes.Buffer
	if err := h.ExportTar(t.Context(), id, nil, &out); err != nil {
		t.Fatalf("ExportTar while stopped: %v", err)
	}
	if read(t, &out)["kept.txt"].body != "kept" {
		t.Fatal("the export while stopped carried nothing")
	}
}

func TestHelperPodCarriesTheBaselineAndNoIdentity(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_helper"
	helper, err := h.helperPod(id, spec(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := helper.Labels[labelID]; ok {
		t.Fatal("the helper carries the sandbox's id label, so a list would read it as the sandbox's Pod")
	}
	if helper.Labels[labelHelper] != id {
		t.Fatalf("helper labels %v", helper.Labels)
	}
	if got := helper.Spec.Containers[0].Image; got != image {
		t.Fatalf("the helper runs %q, want the sandbox's own image", got)
	}
	if !reflect.DeepEqual(helper.Spec.Containers[0].Command, keepAlive) || helper.Spec.Containers[0].Env != nil {
		t.Fatalf("helper container %+v", helper.Spec.Containers[0])
	}
	if *helper.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem != true {
		t.Fatal("the helper does not carry the baseline")
	}
	if helper.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != objectName(id) {
		t.Fatalf("the helper mounts %+v", helper.Spec.Volumes[0])
	}
}

func TestHelperPodReplacesOneLeftBehind(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_leftover"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	leftover := podWith(objectName(id)+"-files", corev1.PodStatus{Phase: corev1.PodRunning})
	if _, err := h.cs.CoreV1().Pods(namespace).Create(t.Context(), leftover, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.ExportTar(t.Context(), id, nil, io.Discard); err != nil {
		t.Fatalf("ExportTar with a helper left behind: %v", err)
	}
	if _, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), leftover.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the helper survived: %v", err)
	}
}

func TestTransferReportsWhatTheContainerSaid(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_transferfail"
	h.created(t, spec(id))
	h.exec.handle = func(_ context.Context, _ execCall, stdin io.Reader, _, stderr io.Writer) error {
		if stdin != nil {
			_, _ = io.Copy(io.Discard, stdin)
		}
		_, _ = io.WriteString(stderr, "tar: can't open 'tree': No such file or directory")
		return exited(1)
	}
	err := h.ExportTar(t.Context(), id, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("ExportTar = %v, want the container's own message", err)
	}
	err = h.ImportTar(t.Context(), id, driver.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "x", body: "y"})))
	if err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("ImportTar = %v, want the container's own message", err)
	}
}

func TestTransferNeedsAClusterConnection(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_transfernoconn"
	h.created(t, spec(id))
	h.stream = nil
	if err := h.ExportTar(t.Context(), id, nil, io.Discard); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("ExportTar = %v, want ErrUnsupported", err)
	}
	if err := h.ImportTar(t.Context(), id, driver.DefaultWorkdir, bytes.NewReader(archive(t))); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("ImportTar = %v, want ErrUnsupported", err)
	}
}

func TestHelperPodThatNeverStarts(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_helperfail"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	h.status = pending("ImagePullBackOff", "")
	h.opts.ReadyTimeout = 300 * time.Millisecond
	err := h.ExportTar(t.Context(), id, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("ExportTar = %v, want the helper's own state", err)
	}
	if _, err := h.cs.CoreV1().Pods(namespace).Get(t.Context(), objectName(id)+"-files", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a helper that never started was left behind: %v", err)
	}
}

func TestRelativeToTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		path, root, want string
		fails            bool
	}{
		{path: "/workspace", root: "/workspace", want: "."},
		{path: "/workspace/tree", root: "/workspace", want: "tree"},
		{path: "/data/a/b", root: "/data", want: "a/b"},
		{path: "/workspacex", root: "/workspace", fails: true},
		{path: "/etc", root: "/workspace", fails: true},
	} {
		got, err := relativeTo(tc.path, tc.root)
		if tc.fails {
			if !errors.Is(err, driver.ErrInvalid) {
				t.Errorf("relativeTo(%q, %q) = %q %v, want ErrInvalid", tc.path, tc.root, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("relativeTo(%q, %q) = %q %v, want %q", tc.path, tc.root, got, err, tc.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"./tree/":         "tree",
		"tree/bin/run.sh": "tree/bin/run.sh",
		"./":              "",
		".":               "",
		"":                "",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTailIsBounded(t *testing.T) {
	var tl tail
	n, err := tl.Write(bytes.Repeat([]byte("x"), tailBytes+100))
	if err != nil || n != tailBytes+100 {
		t.Fatalf("Write = %d %v", n, err)
	}
	if _, err := tl.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if len(tl.String()) != tailBytes {
		t.Fatalf("the tail kept %d bytes, want at most %d", len(tl.String()), tailBytes)
	}
}
