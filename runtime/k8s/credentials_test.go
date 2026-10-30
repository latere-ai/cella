// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	driver "latere.ai/x/cella/runtime"
)

// The values the tests below hand a sandbox as its credentials. Each is a
// string no other field of a fixture spells, so finding one in metadata is
// finding the credential.
const (
	envValue     = "env-value-7f3a"
	rotatedValue = "env-value-rotated-91c2"
	adoptedValue = "env-value-adopted-4d0e"
	credValue    = "gateway-credential-b81d"
	adoptedCred  = "gateway-credential-adopted-c55f"
)

// credentialSpec is a sandbox with every kind of credential a record could
// carry: an environment value, the gateway's credential, a token and the
// gateway's authority, and a mesh, so the mesh's objects are written too.
func credentialSpec(id string) driver.CreateSpec {
	s := gatewaySpec(id)
	s.Env = map[string]string{"MODEL_KEY": envValue, "PLAIN": "visible"}
	s.Egress.Credential = credValue
	s.Mesh = driver.Mesh{ID: "mesh_creds"}
	return s
}

// leaks names every object the driver wrote in the namespace, other than a
// Secret, that holds one of the given secrets anywhere: its spec, its labels,
// its annotations. A Secret is checked for its labels and annotations only,
// since its data is where the values are kept.
func (h *harness) leaks(t *testing.T, secrets ...string) []string {
	t.Helper()
	ctx := t.Context()
	whole := map[string]any{}
	claims, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range claims.Items {
		whole["claim "+claims.Items[i].Name] = &claims.Items[i]
	}
	pods, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		whole["pod "+pods.Items[i].Name] = &pods.Items[i]
	}
	policies, err := h.cs.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range policies.Items {
		whole["policy "+policies.Items[i].Name] = &policies.Items[i]
	}
	services, err := h.cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range services.Items {
		whole["service "+services.Items[i].Name] = &services.Items[i]
	}
	stored, err := h.cs.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range stored.Items {
		meta := stored.Items[i].ObjectMeta
		whole["secret metadata "+meta.Name] = map[string]any{"labels": meta.Labels, "annotations": meta.Annotations}
	}
	if len(pods.Items) == 0 || len(claims.Items) == 0 {
		t.Fatal("the namespace holds no sandbox to check")
	}
	var found []string
	for name, object := range whole {
		encoded, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				found = append(found, name+" holds "+secret)
			}
		}
	}
	slices.Sort(found)
	return found
}

// referencesOnly fails the test for a workload variable of a sandbox's Pod
// that carries its value inline rather than a reference to the sandbox's
// Secret.
func (h *harness) referencesOnly(t *testing.T, id string) {
	t.Helper()
	for _, e := range h.podOf(t, id).Spec.Containers[0].Env {
		ref := e.ValueFrom
		if e.Value != "" || ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != tokenSecretName(id) {
			t.Errorf("the Pod of %s carries %s as %+v, not a reference to its Secret", id, e.Name, e)
		}
	}
}

// ran runs one command in a sandbox, waits for it, and returns the argv the
// cluster was asked to run, which carries the environment the driver wraps.
func (h *harness) ran(t *testing.T, id string, env map[string]string) []string {
	t.Helper()
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	return h.exec.last().argv
}

// assignments is what an argv's leading env sets.
func assignments(argv []string) map[string]string {
	out := map[string]string{}
	if len(argv) == 0 || argv[0] != "env" {
		return out
	}
	for _, a := range argv[1:] {
		name, value, ok := strings.Cut(a, "=")
		if !ok {
			break
		}
		out[name] = value
	}
	return out
}

// commandEnv is the environment a command in a sandbox runs with: what its
// container started with, which the command inherits, under what the argv
// sets.
func (h *harness) commandEnv(t *testing.T, id string, env map[string]string) map[string]string {
	t.Helper()
	argv := h.ran(t, id, env)
	out := h.envOf(t, h.podOf(t, id))
	maps.Copy(out, assignments(argv))
	return out
}

// startsWith checks that a sandbox's Pod carries references only and that
// they resolve to the environment the driver renders for the spec.
func (h *harness) startsWith(t *testing.T, id string, s driver.CreateSpec, token bool) {
	t.Helper()
	h.referencesOnly(t, id)
	if got, want := h.envOf(t, h.podOf(t, id)), h.workloadEnv(s, token); !maps.Equal(got, want) {
		t.Errorf("the container of %s starts with %v, want %v", id, got, want)
	}
}

// TestNoObjectButTheSecretCarriesACredential walks every object the driver
// writes, through a create, an update of the environment, a stop and a
// start, and an adoption and its next start, and finds no environment value
// and no gateway credential anywhere but the sandbox's Secret: not in a
// label or an annotation, and not in a Pod's spec, which references every
// variable of the workload. The proxy variables, which carry the
// credential, are references too.
func TestNoObjectButTheSecretCarriesACredential(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_creds"
	h.created(t, credentialSpec(id))
	secrets := []string{envValue, credValue, rotatedValue, adoptedValue, adoptedCred}
	if found := h.leaks(t, secrets...); len(found) > 0 {
		t.Fatalf("after create: %v", found)
	}
	h.referencesOnly(t, id)
	stored := h.secretOf(t, id)
	if !strings.Contains(string(stored.Data[envKey]), envValue) || string(stored.Data[credentialKey]) != credValue {
		t.Fatalf("the Secret does not keep the credentials: %v", stored.Data)
	}

	env := map[string]string{"MODEL_KEY": rotatedValue}
	labels := map[string]string{"team": "a"}
	if err := h.Update(t.Context(), id, driver.Change{Env: &env, Labels: &labels}); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if found := h.leaks(t, secrets...); len(found) > 0 {
		t.Fatalf("after an update and a restart: %v", found)
	}
	h.referencesOnly(t, id)

	prewarm(t, h, "sbx_entry")
	adoption := driver.Adoption{
		Owner: "bob@example.com", Name: "adopted",
		Env:    map[string]string{"MODEL_KEY": adoptedValue},
		Token:  []byte("adopted-token"),
		Egress: driver.Egress{ProxyAddr: "gateway.example.com:3128", Credential: adoptedCred},
	}
	if err := h.Update(t.Context(), "sbx_entry", driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	if found := h.leaks(t, secrets...); len(found) > 0 {
		t.Fatalf("after an adoption: %v", found)
	}
	if err := h.Stop(t.Context(), "sbx_entry"); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), "sbx_entry"); err != nil {
		t.Fatal(err)
	}
	if found := h.leaks(t, secrets...); len(found) > 0 {
		t.Fatalf("after an adopted sandbox's start: %v", found)
	}
	h.referencesOnly(t, "sbx_entry")
}

// TestTheProjectionMountsNoCredential: the keys the credentials and the
// variables are kept under are not in the projection, so the sandbox's file
// system never holds them through the mount.
func TestTheProjectionMountsNoCredential(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	h.created(t, credentialSpec("sbx_mount"))
	pod := h.podOf(t, "sbx_mount")
	mounted := 0
	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, source := range v.Projected.Sources {
			if source.Secret == nil {
				continue
			}
			for _, item := range source.Secret.Items {
				mounted++
				if item.Key == envKey || item.Key == credentialKey || isEnvVarKey(item.Key) {
					t.Errorf("the projection mounts %s at %s", item.Key, item.Path)
				}
			}
		}
	}
	if mounted == 0 {
		t.Fatal("the fixture mounts no projection to check")
	}
}

// TestTheContainerStartsWithTheSameEnvironment: the references of the Pod a
// create and a start render resolve to the environment the driver renders
// for the sandbox, the token's path and the gateway's variables included; an
// update of the environment leaves the running container as it started and
// reaches its next start; and a start removes the keys of the start before
// it, so a variable an update dropped is not kept.
func TestTheContainerStartsWithTheSameEnvironment(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_same"
	s := credentialSpec(id)
	h.created(t, s)
	h.startsWith(t, id, s, true)
	if got := h.envOf(t, h.podOf(t, id))["HTTPS_PROXY"]; !strings.Contains(got, credValue) {
		t.Fatalf("the proxy variable resolves to %q, without the credential", got)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	h.startsWith(t, id, s, true)

	env := map[string]string{"MODEL_KEY": rotatedValue}
	if err := h.Update(t.Context(), id, driver.Change{Env: &env}); err != nil {
		t.Fatal(err)
	}
	// The container keeps the environment it started with.
	h.startsWith(t, id, s, true)
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	updated := s
	updated.Env = env
	h.startsWith(t, id, updated, true)
	referenced := map[string]bool{}
	for _, e := range h.podOf(t, id).Spec.Containers[0].Env {
		referenced[e.ValueFrom.SecretKeyRef.Key] = true
	}
	for key := range h.secretOf(t, id).Data {
		if isEnvVarKey(key) && !referenced[key] {
			t.Errorf("the Secret keeps %s, which no running container references", key)
		}
	}
}

// TestACommandCarriesOnlyWhatTheContainerLacks: a command inherits the
// environment its container started with, so the exec request repeats none
// of the sandbox's own values on its argv until an update has changed one,
// and then that one; the command's environment is what it was when every
// value was repeated. The request's own variables are always on the argv.
func TestACommandCarriesOnlyWhatTheContainerLacks(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_argv"
	h.created(t, credentialSpec(id))
	argv := h.ran(t, id, map[string]string{"REQUEST": "per-call"})
	if set := assignments(argv); len(set) != 1 || set["REQUEST"] != "per-call" {
		t.Errorf("a fresh sandbox's command is %q, want the request's variable alone", argv)
	}
	if got := h.commandEnv(t, id, nil); got["MODEL_KEY"] != envValue || got["PLAIN"] != "visible" {
		t.Errorf("the command runs with %v", got)
	}

	env := map[string]string{"MODEL_KEY": rotatedValue}
	if err := h.Update(t.Context(), id, driver.Change{Env: &env}); err != nil {
		t.Fatal(err)
	}
	if set := assignments(h.ran(t, id, nil)); !maps.Equal(set, env) {
		t.Errorf("after an update the command sets %v, want the changed variable alone", set)
	}
	// As before, a variable the update dropped is not taken from a command
	// whose container started with it.
	if got := h.commandEnv(t, id, nil); got["MODEL_KEY"] != rotatedValue || got["PLAIN"] != "visible" {
		t.Errorf("after an update the command runs with %v", got)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if set := assignments(h.ran(t, id, nil)); len(set) != 0 {
		t.Errorf("after the next start the command sets %v", set)
	}
	if got := h.commandEnv(t, id, nil); got["MODEL_KEY"] != rotatedValue {
		t.Errorf("after the next start the command runs with %v", got)
	}
	if _, kept := h.commandEnv(t, id, nil)["PLAIN"]; kept {
		t.Error("the dropped variable survived the next start")
	}
}

// TestAnEmptyEnvironmentIsWritten: an update to no environment at all leaves
// the Secret saying so, and a command sets none of the sandbox's variables.
func TestAnEmptyEnvironmentIsWritten(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_empty"
	h.created(t, credentialSpec(id))
	none := map[string]string{}
	if err := h.Update(t.Context(), id, driver.Change{Env: &none}); err != nil {
		t.Fatal(err)
	}
	if got := string(h.secretOf(t, id).Data[envKey]); got != "{}" {
		t.Errorf("the Secret's environment is %q, want {}", got)
	}
	if set := assignments(h.ran(t, id, nil)); len(set) != 0 {
		t.Errorf("the command after the environment was cleared sets %v", set)
	}
}

// TestAnAdoptedSandboxStartsWithTheAdoptersCredentials: the adopter's
// environment is what a command in the running entry carries, on its argv
// since the entry's container started without it, and the adopter's
// environment and credential are what the next start renders.
func TestAnAdoptedSandboxStartsWithTheAdoptersCredentials(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_adopt"
	prewarm(t, h, id)
	adoption := driver.Adoption{
		Owner: "bob@example.com", Name: "adopted",
		Env:    map[string]string{"MODEL_KEY": adoptedValue},
		Token:  []byte("adopted-token"),
		Egress: driver.Egress{ProxyAddr: "gateway.example.com:3128", ReverseAddr: "gateway.example.com:8080", Credential: adoptedCred},
	}
	if err := h.Update(t.Context(), id, driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	if got := h.commandEnv(t, id, nil); got["MODEL_KEY"] != adoptedValue {
		t.Errorf("the command in the adopted sandbox runs with %v", got)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pvc := h.claimOf(t, id)
	adopted, _, err := h.fullSpec(t.Context(), id, pvc)
	if err != nil {
		t.Fatal(err)
	}
	h.startsWith(t, id, adopted, true)
	env := h.envOf(t, h.podOf(t, id))
	if env["MODEL_KEY"] != adoptedValue || env["CELLA_GATEWAY_CREDENTIAL"] != adoptedCred {
		t.Errorf("the adopted sandbox restarts with %v", env)
	}
}

// legacy makes a sandbox as a driver from before the move left it: the whole
// spec, credentials included, in the claim's record, a Pod whose container
// carries every variable inline, and a Secret holding the token and the trust
// file alone.
func legacy(t *testing.T, h *harness, s driver.CreateSpec) {
	t.Helper()
	h.created(t, s)
	ctx := t.Context()
	pvc := h.claimOf(t, s.ID)
	whole, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	pvc.Annotations[annSpec] = string(whole)
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := h.podOf(t, s.ID)
	pod.Spec.Containers[0].Env = env(h.workloadEnv(s, len(s.Token) > 0))
	if _, err := h.cs.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	secret := h.secretOf(t, s.ID)
	maps.DeleteFunc(secret.Data, func(key string, _ []byte) bool {
		return key == envKey || key == credentialKey || isEnvVarKey(key)
	})
	if _, err := h.cs.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if !inline(h.claimOf(t, s.ID)) {
		t.Fatal("the legacy fixture's record carries no credentials")
	}
}

// moved reports whether a sandbox's record has been rewritten without its
// credentials and the Secret holds them.
func (h *harness) moved(t *testing.T, id string, env map[string]string, credential string) {
	t.Helper()
	pvc := h.claimOf(t, id)
	if inline(pvc) {
		t.Fatalf("the record of %s still carries credentials: %s", id, pvc.Annotations[annSpec])
	}
	stored := h.secretOf(t, id)
	var got map[string]string
	if err := json.Unmarshal(stored.Data[envKey], &got); err != nil {
		t.Fatalf("the Secret's environment of %s: %v", id, err)
	}
	if !maps.Equal(got, env) || string(stored.Data[credentialKey]) != credential {
		t.Fatalf("the Secret of %s holds %v and %q, want %v and %q", id, got, stored.Data[credentialKey], env, credential)
	}
	if string(stored.Data[tokenKey]) == "" {
		t.Errorf("the move lost the token of %s", id)
	}
}

// TestMoveCredentialsRewritesAnOldRecord: a sandbox an earlier driver left
// with its credentials in its record and inline in its Pod runs commands as
// before, Preflight writes nothing, MoveCredentials rewrites the record and
// moves the values into the Secret, a second run finds nothing to do, and the
// sandbox's next start renders a Pod that references every variable.
func TestMoveCredentialsRewritesAnOldRecord(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	versioned(h.cs)
	s := credentialSpec("sbx_legacy")
	legacy(t, h, s)
	plain := spec("sbx_plain")
	h.created(t, plain)

	// Before the rewrite, a command runs with the record's values, which
	// the old Pod's container holds and so the argv does not repeat.
	argv := h.ran(t, s.ID, nil)
	if _, repeated := assignments(argv)["MODEL_KEY"]; repeated {
		t.Errorf("the command on an old Pod repeats a value its container holds: %q", argv)
	}
	if got := h.commandEnv(t, s.ID, nil); got["MODEL_KEY"] != envValue {
		t.Errorf("the command on an old record runs with %v", got)
	}

	if err := h.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !inline(h.claimOf(t, s.ID)) {
		t.Fatal("Preflight rewrote a record; it writes nothing")
	}
	rewritten, err := h.MoveCredentials(t.Context())
	if err != nil || rewritten != 1 {
		t.Fatalf("the move rewrote %d records: %v", rewritten, err)
	}
	h.moved(t, s.ID, s.Env, credValue)
	for _, key := range []string{envKey, credentialKey} {
		if _, held := h.secretOf(t, plain.ID).Data[key]; held {
			t.Errorf("the move gave a sandbox with no credentials %s", key)
		}
	}
	if rewritten, err = h.MoveCredentials(t.Context()); err != nil || rewritten != 0 {
		t.Errorf("a second move rewrote %d records: %v", rewritten, err)
	}

	if err := h.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if found := h.leaks(t, envValue, credValue); len(found) > 0 {
		t.Fatalf("after the move and the next start: %v", found)
	}
	h.startsWith(t, s.ID, s, true)
	state, err := h.Inspect(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Name != s.Name || state.MeshID != s.Mesh.ID {
		t.Errorf("the rewritten sandbox reads back as %+v", state)
	}
}

// TestAnOldPodsExpandedValueIsRepeated: the kubelet expands "$(NAME)" and
// "$$" in a value a Pod carries inline, so a command on an old Pod is given
// such a value on its argv as written, as it always was, rather than
// inheriting the expanded one.
func TestAnOldPodsExpandedValueIsRepeated(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	s := credentialSpec("sbx_dollar")
	s.Env["TEMPLATE"] = "a$$b$(PLAIN)"
	legacy(t, h, s)
	set := assignments(h.ran(t, s.ID, nil))
	if set["TEMPLATE"] != "a$$b$(PLAIN)" {
		t.Errorf("the command on an old Pod sets %v, without the value the kubelet expanded", set)
	}
	if _, repeated := set["MODEL_KEY"]; repeated {
		t.Errorf("the command on an old Pod repeats a value its container holds: %v", set)
	}
}

// TestAWriteRewritesAnOldRecord: a sandbox the sweep did not reach has its
// record rewritten by the next write of it, whichever write that is, and an
// update of the environment on an old record keeps the gateway credential the
// record held.
func TestAWriteRewritesAnOldRecord(t *testing.T) {
	labels := map[string]string{"team": "b"}
	env := map[string]string{"MODEL_KEY": rotatedValue}
	for _, tc := range []struct {
		name  string
		write func(h *harness, id string) error
		env   map[string]string
	}{
		{"stop", func(h *harness, id string) error { return h.Stop(t.Context(), id) }, nil},
		{"labels", func(h *harness, id string) error {
			return h.Update(t.Context(), id, driver.Change{Labels: &labels})
		}, nil},
		{"environment", func(h *harness, id string) error {
			return h.Update(t.Context(), id, driver.Change{Env: &env})
		}, env},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWith(t, withGateway)
			versioned(h.cs)
			s := credentialSpec("sbx_write")
			legacy(t, h, s)
			if err := tc.write(h, s.ID); err != nil {
				t.Fatal(err)
			}
			want := s.Env
			if tc.env != nil {
				want = tc.env
			}
			h.moved(t, s.ID, want, credValue)
		})
	}
}

// TestTheSecretIsOwnedByTheClaim: the Secret names the claim as its owner,
// so a claim removed by anything other than the driver takes the
// credentials with it, and the reference does not block the claim's
// deletion. An older Secret takes the reference at its next write.
func TestTheSecretIsOwnedByTheClaim(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	h.cs.PrependReactor("create", "persistentvolumeclaims", func(action k8stesting.Action) (bool, kruntime.Object, error) {
		pvc := action.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		pvc.UID = types.UID("uid-" + pvc.Name)
		return false, nil, nil
	})
	s := credentialSpec("sbx_owned")
	h.created(t, s)
	owned := func() {
		t.Helper()
		refs := h.secretOf(t, s.ID).OwnerReferences
		if len(refs) != 1 {
			t.Fatalf("the Secret's owners are %+v", refs)
		}
		ref := refs[0]
		if ref.Kind != "PersistentVolumeClaim" || ref.Name != objectName(s.ID) || ref.UID != types.UID("uid-"+objectName(s.ID)) || ref.BlockOwnerDeletion != nil {
			t.Errorf("the Secret's owner is %+v", ref)
		}
	}
	owned()
	// A second write names the owner once.
	if err := h.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	owned()

	secret := h.secretOf(t, s.ID)
	secret.OwnerReferences = nil
	if _, err := h.cs.CoreV1().Secrets(namespace).Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MODEL_KEY": rotatedValue}
	if err := h.Update(t.Context(), s.ID, driver.Change{Env: &env}); err != nil {
		t.Fatal(err)
	}
	owned()
}

// TestASecretReadFailureIsNotAnEmptyEnvironment: a command whose Secret
// cannot be read is refused rather than run without its environment, and an
// environment the Secret holds unreadably is named.
func TestASecretReadFailureIsNotAnEmptyEnvironment(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_unread"
	h.created(t, credentialSpec(id))
	h.cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is restarting")
	})
	if _, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}}); err == nil {
		t.Fatal("a command ran with no read of its environment")
	}

	h = newHarnessWith(t, withGateway)
	h.created(t, credentialSpec(id))
	secret := h.secretOf(t, id)
	secret.Data[envKey] = []byte("{")
	if _, err := h.cs.CoreV1().Secrets(namespace).Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an unreadable environment is %v, want ErrInvalid", err)
	}
}

// TestAMoveThatCannotWriteLeavesTheRecord: a record the move cannot rewrite
// is reported by its sandbox and left as it stands, values included, so the
// record never loses a value the Secret does not hold and the sandbox still
// runs with it. A namespace the move cannot list is reported as such.
func TestAMoveThatCannotWriteLeavesTheRecord(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	s := credentialSpec("sbx_stuck")
	legacy(t, h, s)
	h.cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), tokenSecretName(s.ID), errors.New("no update"))
	})
	rewritten, err := h.MoveCredentials(t.Context())
	if err == nil || rewritten != 0 {
		t.Fatalf("a move that could not write reports %d rewritten and %v", rewritten, err)
	}
	if !strings.Contains(err.Error(), s.ID) {
		t.Errorf("the failure %q does not name the sandbox", err)
	}
	if !inline(h.claimOf(t, s.ID)) {
		t.Error("the record lost its credentials before the Secret held them")
	}
	if got := h.commandEnv(t, s.ID, nil); got["MODEL_KEY"] != envValue {
		t.Errorf("the command on a record the move left runs with %v", got)
	}

	h.cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is restarting")
	})
	if _, err := h.MoveCredentials(t.Context()); err == nil {
		t.Error("a move that could not list the claims reported nothing")
	}
}

// TestALostSecretWriteIsRebuilt: a Secret another writer changed between its
// read and this write, as a move running beside an update or a start of the
// same sandbox does, is a lost race and not a failed change. The write reads
// the Secret again and lands, and one that keeps losing fails after the same
// bound a lost patch does.
func TestALostSecretWriteIsRebuilt(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_race"
	s := credentialSpec(id)
	h.created(t, s)
	lost := 1
	h.cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, kruntime.Object, error) {
		if lost == 0 {
			return false, nil, nil
		}
		lost--
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), tokenSecretName(id), errors.New("the object has been modified"))
	})
	env := map[string]string{"MODEL_KEY": rotatedValue}
	if err := h.Update(t.Context(), id, driver.Change{Env: &env}); err != nil {
		t.Fatalf("an update that lost one Secret write: %v", err)
	}
	if got := string(h.secretOf(t, id).Data[envKey]); !strings.Contains(got, rotatedValue) {
		t.Errorf("the update left the environment %q", got)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	lost = 1
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatalf("a start that lost one Secret write: %v", err)
	}
	updated := s
	updated.Env = env
	h.startsWith(t, id, updated, true)

	lost = patchAttempts
	if err := h.Update(t.Context(), id, driver.Change{Env: &env}); !apierrors.IsConflict(err) {
		t.Errorf("an update whose every Secret write was lost is %v, want the conflict", err)
	}
}

// TestEachVariableHasItsOwnKey: a variable's key names the start and the
// variable, two variables and two starts never share one, and a name outside
// the key grammar is kept under a hash of it that no name spells.
func TestEachVariableHasItsOwnKey(t *testing.T) {
	keys := map[string]string{}
	for _, gen := range []string{"a1", "a2"} {
		for _, name := range []string{"MODEL_KEY", "json", "token", "a.b", "a-b", "Ü", strings.Repeat("N", 300)} {
			key := envVarKey(gen, name)
			if !isEnvVarKey(key) || len(key) > 253 || !regexp.MustCompile(`^[-._a-zA-Z0-9]+$`).MatchString(key) {
				t.Errorf("%s of %q is no Secret key this driver owns", key, name)
			}
			if other, taken := keys[key]; taken {
				t.Errorf("%q and %q share the key %s", other, gen+"/"+name, key)
			}
			keys[key] = gen + "/" + name
		}
	}
	for _, fixed := range []string{tokenKey, authorityKey, envKey, credentialKey} {
		if isEnvVarKey(fixed) {
			t.Errorf("the key %s reads as a variable's", fixed)
		}
	}
	if got := envVarKey("g", "MODEL_KEY"); got != "env-g-MODEL_KEY" {
		t.Errorf("a POSIX name's key is %s", got)
	}
}
