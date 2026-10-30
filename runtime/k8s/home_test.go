// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestTheImagesHomeIsMadeWritable: with a home named, the workload mounts
// the directory .home of its claim at the image's home, an init container of
// the same image under the same baseline and limits fills it first, and HOME
// names it unless the sandbox names its own. A home that holds the workspace
// or lies inside it mounts nothing and leaves HOME on the workspace.
func TestTheImagesHomeIsMadeWritable(t *testing.T) {
	h := newHarnessWith(t, func(o *Options) { o.Home = "/home/agent" })
	s := spec("sbx_home")
	h.created(t, s)
	pod := h.podOf(t, s.ID)
	main := pod.Spec.Containers[0]
	if !slices.Contains(main.VolumeMounts, corev1.VolumeMount{Name: "workspace", MountPath: "/home/agent", SubPath: homeDir}) {
		t.Errorf("the workload mounts %v, want the claim's %s at /home/agent", main.VolumeMounts, homeDir)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("the Pod has %d init containers, want the one that fills the home", len(pod.Spec.InitContainers))
	}
	fill := pod.Spec.InitContainers[0]
	if fill.Name != HomeContainer || fill.Image != main.Image || !reflect.DeepEqual(fill.Resources, main.Resources) {
		t.Errorf("the fill runs as %q from %q under %+v, want the workload's image and limits", fill.Name, fill.Image, fill.Resources)
	}
	if !reflectEqualContext(fill.SecurityContext, hardened()) {
		t.Errorf("the fill's security context is %+v, not the baseline", fill.SecurityContext)
	}
	if want := []string{"sh", "-c", fillHome, HomeContainer, "/home/agent", "/workspace/.home"}; !slices.Equal(fill.Command, want) {
		t.Errorf("the fill runs %q, want %q", fill.Command, want)
	}
	if want := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}; !reflect.DeepEqual(fill.VolumeMounts, want) {
		t.Errorf("the fill mounts %v, want the claim at the workspace alone", fill.VolumeMounts)
	}
	if got := h.envOf(t, pod)[homeEnv]; got != "/home/agent" {
		t.Errorf("HOME is %q, want the image's home", got)
	}
	h.referencesOnly(t, s.ID)

	named := spec("sbx_named_home")
	named.Env = map[string]string{homeEnv: "/workspace/me"}
	h.created(t, named)
	if got := h.envOf(t, h.podOf(t, named.ID))[homeEnv]; got != "/workspace/me" {
		t.Errorf("HOME the sandbox named is %q", got)
	}

	for _, ws := range []string{"/home/agent/work", "/home"} {
		nested := spec("sbx_nested")
		nested.Workspace.Path = ws
		now := h.clock.now()
		pod, err := h.pod(nested, now, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(pod.Spec.InitContainers) != 0 || slices.ContainsFunc(pod.Spec.Containers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.SubPath == homeDir }) {
			t.Errorf("a workspace at %s mounted the home: %v", ws, pod.Spec.Containers[0].VolumeMounts)
		}
		if got := resolved(pod, h.podEnvData(nested, now, false))[homeEnv]; got != ws {
			t.Errorf("a workspace at %s left HOME %q", ws, got)
		}
	}
}

// TestNoHomeMountsNothing: a driver that names no home renders the Pod it
// always did, with no init container and HOME on the workspace.
func TestNoHomeMountsNothing(t *testing.T) {
	h := newHarness(t)
	s := spec("sbx_plain_home")
	now := h.clock.now()
	pod, err := h.pod(s, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("init containers %v", pod.Spec.InitContainers)
	}
	if got := resolved(pod, h.podEnvData(s, now, false))[homeEnv]; got != "/workspace" {
		t.Errorf("HOME is %q, want the workspace", got)
	}
}

// TestFillHomeFillsOnceAndKeepsWhatWasWritten runs the init container's
// program with the system's shell: the first start copies the image's home,
// its dotfiles and links included; a later start leaves what the workload
// changed; a start that died half way is filled again; and an image with no
// such home gets an empty one.
func TestFillHomeFillsOnceAndKeepsWhatWasWritten(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the fill with")
	}
	root := t.TempDir()
	image := filepath.Join(root, "image-home")
	must(t, os.MkdirAll(filepath.Join(image, ".npm-global", "bin"), 0o755))
	must(t, os.WriteFile(filepath.Join(image, ".bashrc"), []byte("PS1=image\n"), 0o644))
	must(t, os.Symlink("/usr/local/bin/playwright", filepath.Join(image, ".npm-global", "bin", "playwright")))
	must(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o755))
	claim := filepath.Join(root, "workspace", homeDir)
	fill := func(from string) {
		t.Helper()
		if out, err := exec.Command(sh, "-c", fillHome, HomeContainer, from, claim).CombinedOutput(); err != nil {
			t.Fatalf("the fill failed: %v: %s", err, out)
		}
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(claim, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	fill(image)
	if got := read(".bashrc"); got != "PS1=image\n" {
		t.Errorf("the first start's .bashrc is %q", got)
	}
	if got, err := os.Readlink(filepath.Join(claim, ".npm-global", "bin", "playwright")); err != nil || got != "/usr/local/bin/playwright" {
		t.Errorf("the image's link reads %q, %v", got, err)
	}

	must(t, os.WriteFile(filepath.Join(claim, ".bashrc"), []byte("PS1=mine\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(claim, ".npm-global", "bin", "claude"), []byte("#!/bin/sh\n"), 0o755))
	fill(image)
	if got := read(".bashrc"); got != "PS1=mine\n" {
		t.Errorf("a later start overwrote .bashrc with %q", got)
	}
	if _, err := os.Stat(filepath.Join(claim, ".npm-global", "bin", "claude")); err != nil {
		t.Errorf("a later start lost an install: %v", err)
	}

	must(t, os.RemoveAll(claim))
	must(t, os.MkdirAll(filepath.Join(claim+".fill", "half"), 0o755))
	fill(image)
	if got := read(".bashrc"); got != "PS1=image\n" {
		t.Errorf("the start after a half fill reads .bashrc %q", got)
	}
	if _, err := os.Stat(filepath.Join(claim, "half")); !os.IsNotExist(err) {
		t.Errorf("the half fill's leftovers reached the home: %v", err)
	}
	if _, err := os.Stat(claim + ".fill"); !os.IsNotExist(err) {
		t.Errorf("the half fill's directory is still there: %v", err)
	}

	must(t, os.RemoveAll(claim))
	fill(filepath.Join(root, "absent"))
	if entries, err := os.ReadDir(claim); err != nil || len(entries) != 0 {
		t.Errorf("an image with no home filled %v, %v; want an empty directory", entries, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
