// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json"
	"errors"
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

// metadataLeaks names every label and annotation, on every object the driver
// wrote in the namespace, whose value holds one of the given secrets.
func (h *harness) metadataLeaks(t *testing.T, secrets ...string) []string {
	t.Helper()
	ctx := t.Context()
	var objects []metav1.Object
	claims, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range claims.Items {
		objects = append(objects, &claims.Items[i])
	}
	pods, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		objects = append(objects, &pods.Items[i])
	}
	stored, err := h.cs.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range stored.Items {
		objects = append(objects, &stored.Items[i])
	}
	policies, err := h.cs.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range policies.Items {
		objects = append(objects, &policies.Items[i])
	}
	services, err := h.cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range services.Items {
		objects = append(objects, &services.Items[i])
	}
	if len(objects) == 0 {
		t.Fatal("the namespace holds no object to check")
	}
	var leaks []string
	for _, o := range objects {
		for kind, m := range map[string]map[string]string{"label": o.GetLabels(), "annotation": o.GetAnnotations()} {
			for key, value := range m {
				for _, secret := range secrets {
					if strings.Contains(value, secret) {
						leaks = append(leaks, o.GetName()+" "+kind+" "+key)
					}
				}
			}
		}
	}
	slices.Sort(leaks)
	return leaks
}

// ran runs one command in a sandbox, waits for it, and returns the argv the
// cluster was asked to run, which carries the environment the driver wraps.
func (h *harness) ran(t *testing.T, id string) []string {
	t.Helper()
	e, err := h.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	return h.exec.last().argv
}

// containerEnv reads one variable off the workload container of a Pod.
func containerEnv(pod *corev1.Pod, name string) (string, bool) {
	for _, c := range pod.Spec.Containers {
		if c.Name != Container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, true
			}
		}
	}
	return "", false
}

// TestNoMetadataCarriesACredential walks every object the driver writes,
// through a create, an update of the environment, a stop and a start, and an
// adoption, and finds no environment value and no gateway credential in any
// label or annotation. The values are in the sandbox's Secret.
func TestNoMetadataCarriesACredential(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_creds"
	h.created(t, credentialSpec(id))
	secrets := []string{envValue, credValue, rotatedValue, adoptedValue, adoptedCred}
	if leaks := h.metadataLeaks(t, secrets...); len(leaks) > 0 {
		t.Fatalf("after create, metadata carries a credential: %v", leaks)
	}
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
	if leaks := h.metadataLeaks(t, secrets...); len(leaks) > 0 {
		t.Fatalf("after an update and a restart, metadata carries a credential: %v", leaks)
	}

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
	if leaks := h.metadataLeaks(t, secrets...); len(leaks) > 0 {
		t.Fatalf("after an adoption, metadata carries a credential: %v", leaks)
	}
}

// TestTheProjectionMountsNoCredential: the keys the credentials are kept
// under are not in the projection, so the sandbox's file system never holds
// them through the mount.
func TestTheProjectionMountsNoCredential(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	h.created(t, credentialSpec("sbx_mount"))
	pod := h.podOf(t, "sbx_mount")
	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, source := range v.Projected.Sources {
			if source.Secret == nil {
				continue
			}
			for _, item := range source.Secret.Items {
				if item.Key == envKey || item.Key == credentialKey {
					t.Errorf("the projection mounts %s at %s", item.Key, item.Path)
				}
			}
		}
	}
}

// TestStartRendersTheCredentialsFromTheSecret: the Pod a start renders, and
// the command an exec wraps, carry the environment and the credential the
// Secret keeps, including an environment an update replaced.
func TestStartRendersTheCredentialsFromTheSecret(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	const id = "sbx_start"
	h.created(t, credentialSpec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pod := h.podOf(t, id)
	if got, _ := containerEnv(pod, "MODEL_KEY"); got != envValue {
		t.Errorf("the restarted Pod's MODEL_KEY is %q", got)
	}
	if got, _ := containerEnv(pod, "CELLA_GATEWAY_CREDENTIAL"); got != credValue {
		t.Errorf("the restarted Pod's gateway credential is %q", got)
	}

	env := map[string]string{"MODEL_KEY": rotatedValue}
	if err := h.Update(t.Context(), id, driver.Change{Env: &env}); err != nil {
		t.Fatal(err)
	}
	argv := h.ran(t, id)
	if !slices.Contains(argv, "MODEL_KEY="+rotatedValue) || slices.Contains(argv, "PLAIN=visible") {
		t.Errorf("the exec after an update of the environment runs %q", argv)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pod = h.podOf(t, id)
	if got, _ := containerEnv(pod, "MODEL_KEY"); got != rotatedValue {
		t.Errorf("the Pod after an update of the environment has MODEL_KEY %q", got)
	}
	if _, kept := containerEnv(pod, "PLAIN"); kept {
		t.Error("a variable the update dropped is still in the Pod")
	}
	if got, _ := containerEnv(pod, "CELLA_GATEWAY_CREDENTIAL"); got != credValue {
		t.Errorf("the update of the environment lost the gateway credential: %q", got)
	}
}

// TestAnEmptyEnvironmentIsWritten: an update to no environment at all leaves
// the Secret saying so, and a command runs with none.
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
	if argv := h.ran(t, id); slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "MODEL_KEY=") }) {
		t.Errorf("the exec after the environment was cleared runs %q", argv)
	}
}

// TestAnAdoptedSandboxStartsWithTheAdoptersCredentials: the adopter's
// environment and credential are what an exec carries and what the next
// start renders.
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
	if argv := h.ran(t, id); !slices.Contains(argv, "MODEL_KEY="+adoptedValue) {
		t.Errorf("the exec in the adopted sandbox runs %q", argv)
	}
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	pod := h.podOf(t, id)
	if got, _ := containerEnv(pod, "MODEL_KEY"); got != adoptedValue {
		t.Errorf("the adopted sandbox restarts with MODEL_KEY %q", got)
	}
	if got, _ := containerEnv(pod, "CELLA_GATEWAY_CREDENTIAL"); got != adoptedCred {
		t.Errorf("the adopted sandbox restarts with the gateway credential %q", got)
	}
}

// legacy makes a sandbox as a driver from before the move left it: the whole
// spec, credentials included, in the claim's record, and a Secret holding the
// token and the trust file alone.
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
	secret := h.secretOf(t, s.ID)
	delete(secret.Data, envKey)
	delete(secret.Data, credentialKey)
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
	if !mapsEqual(got, env) || string(stored.Data[credentialKey]) != credential {
		t.Fatalf("the Secret of %s holds %v and %q, want %v and %q", id, got, stored.Data[credentialKey], env, credential)
	}
	if string(stored.Data[tokenKey]) == "" {
		t.Errorf("the move lost the token of %s", id)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestMoveCredentialsRewritesAnOldRecord: a record an earlier driver stamped
// with credentials is read as it stands until it is rewritten, Preflight
// writes nothing, MoveCredentials rewrites the record and moves the values
// into the Secret, the sandbox restarts with them, and a second run finds
// nothing to do.
func TestMoveCredentialsRewritesAnOldRecord(t *testing.T) {
	h := newHarnessWith(t, withGateway)
	versioned(h.cs)
	s := credentialSpec("sbx_legacy")
	legacy(t, h, s)
	plain := spec("sbx_plain")
	h.created(t, plain)

	// Before the rewrite, a command still runs with the record's values.
	if argv := h.ran(t, s.ID); !slices.Contains(argv, "MODEL_KEY="+envValue) {
		t.Errorf("the exec on an old record runs %q", argv)
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
	if leaks := h.metadataLeaks(t, envValue, credValue); len(leaks) > 0 {
		t.Fatalf("after the move, metadata carries a credential: %v", leaks)
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Get(t.Context(), tokenSecretName(plain.ID), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("a sandbox with no credentials was given a Secret: %v", err)
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
	pod := h.podOf(t, s.ID)
	if got, _ := containerEnv(pod, "MODEL_KEY"); got != envValue {
		t.Errorf("the rewritten sandbox restarts with MODEL_KEY %q", got)
	}
	if got, _ := containerEnv(pod, "CELLA_GATEWAY_CREDENTIAL"); got != credValue {
		t.Errorf("the rewritten sandbox restarts with the gateway credential %q", got)
	}
	state, err := h.Inspect(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Name != s.Name || state.MeshID != s.Mesh.ID {
		t.Errorf("the rewritten sandbox reads back as %+v", state)
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
	if argv := h.ran(t, s.ID); !slices.Contains(argv, "MODEL_KEY="+envValue) {
		t.Errorf("the exec on a record the move left runs %q", argv)
	}

	h.cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, kruntime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is restarting")
	})
	if _, err := h.MoveCredentials(t.Context()); err == nil {
		t.Error("a move that could not list the claims reported nothing")
	}
}
