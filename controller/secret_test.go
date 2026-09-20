// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// testSealer is the store of design 010's envelope as this package's seam
// reads it. The controller cannot import that store, because the store
// imports the controller, so the test brings its own AES-256-GCM: the point
// of the seam is that the crypto lives on the other side of it, and a fake
// that did not encrypt would let the snapshot assertions below pass on a
// plaintext file.
type testSealer struct{ aead cipher.AEAD }

func newSealer(t *testing.T) testSealer {
	t.Helper()
	block, err := aes.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return testSealer{aead: aead}
}

func (s testSealer) Seal(plaintext []byte) ([]byte, []byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return []byte("data-key"), s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (s testSealer) Open(_, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("the sealed value is shorter than its nonce")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// sealedController is a controller over a snapshot store that holds values,
// with a gateway that acknowledges every map.
func sealedController(t *testing.T) (*Controller, *gateway, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenSealedFileStore(dir, newSealer(t))
	if err != nil {
		t.Fatal(err)
	}
	gw := &gateway{}
	c := openController(t, Options{
		Store: store, Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: gw,
	})
	return c, gw, dir
}

// aSecret is a resolved Secret as the API hands one to the controller.
func aSecret(name, host, value string) v1.Secret {
	return v1.Secret{
		APIVersion: v1.APIVersion, Kind: v1.KindSecret, Metadata: v1.Metadata{Name: name},
		Spec: v1.SecretSpec{
			Kind:   v1.SecretStatic,
			Scope:  v1.SecretScope{Hosts: []string{host}, Ports: []int{443}},
			Inject: v1.SecretInject{Header: "Authorization", Scheme: v1.SchemeBearer},
			Value:  value,
		},
	}
}

// mounting is a resolved sandbox that mounts one secret by the name given.
func mounting(name, env string) v1.Sandbox {
	obj := workspace()
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist}
	obj.Spec.Secrets = []v1.SecretMount{{Name: name, Env: env}}
	return obj
}

// TestSecretCollection is the kind's own lifecycle over the controller: a
// create takes an id and a version, a read returns no value, an update
// carrying one bumps the version, and a delete takes the object and its
// value with it.
func TestSecretCollection(t *testing.T) {
	c, _, dir := sealedController(t)
	ctx := t.Context()
	if !c.SecretsEnabled() {
		t.Fatal("a sealed store reports no secrets")
	}

	created, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Status.ID, v1.SecretIDPrefix) || created.Status.Version != 1 || created.Spec.Value != "" {
		t.Fatalf("created = %+v", created)
	}
	if _, err = c.CreateSecret(ctx, aSecret("github", "api.github.com", "x"), "alice"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a second create of one name: %v", err)
	}
	if _, err = c.CreateSecret(ctx, aSecret("github", "api.github.com", "x"), ""); err == nil {
		t.Fatal("a secret with no owner was created")
	}
	// Another owner may hold the same name.
	if _, err = c.CreateSecret(ctx, aSecret("github", "api.github.com", "bobs"), "bob"); err != nil {
		t.Fatal(err)
	}

	t.Run("readsAndLists", func(t *testing.T) {
		for _, key := range []string{"github", created.Status.ID} {
			got, err := c.GetSecret(ctx, key, "alice")
			if err != nil || got.Status.ID != created.Status.ID {
				t.Fatalf("%s: %+v %v", key, got, err)
			}
		}
		if _, err := c.GetSecret(ctx, "absent", "alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an absent name: %v", err)
		}
		if _, err := c.GetSecret(ctx, v1.SecretIDPrefix+"nothing", "alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an absent id: %v", err)
		}
		if got := c.ListSecrets(); len(got) != 2 || !slices.IsSortedFunc(got, func(a, b v1.Secret) int {
			return strings.Compare(a.Status.ID, b.Status.ID)
		}) {
			t.Fatalf("the list is %+v", got)
		}
	})

	t.Run("theSnapshotHoldsNoPlaintext", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(dir, "objects.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "ghp_canary") {
			t.Fatal("the snapshot holds the value in the clear")
		}
	})

	t.Run("anUpdate", func(t *testing.T) {
		rotated, err := c.UpdateSecret(ctx, aSecret("github", "api.github.com", "ghp_rotated"), created.Status.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rotated.Status.Version != 2 {
			t.Fatalf("version = %d, want 2", rotated.Status.Version)
		}
		unchanged, err := c.UpdateSecret(ctx, aSecret("github", "api.github.com", ""), created.Status.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Status.Version != 2 {
			t.Fatalf("an update with no value moved the version to %d", unchanged.Status.Version)
		}
		if _, err = c.UpdateSecret(ctx, aSecret("github", "api.github.com", "x"), "sec_absent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an update of an absent secret: %v", err)
		}
	})

	t.Run("aDelete", func(t *testing.T) {
		if _, err := c.DeleteSecret(ctx, created.Status.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := c.DeleteSecret(ctx, created.Status.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a second delete: %v", err)
		}
		if _, err := c.GetSecret(ctx, "github", "alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the deleted secret is still readable: %v", err)
		}
	})
}

// TestASecretNeedsAStoreThatHoldsOne is the deployment with no key: every
// other kind is served and a value is refused with one error the API turns
// into the code that says the server cannot provide it.
func TestASecretNeedsAStoreThatHoldsOne(t *testing.T) {
	c, _ := newController(t)
	ctx := t.Context()
	if _, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "x"), "alice"); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("a create with no key: %v", err)
	}
	// Nothing was written, so the collection is as it was.
	if got := c.ListSecrets(); len(got) != 0 {
		t.Fatalf("the list is %+v", got)
	}
	if _, err := c.SecretFor(ctx, "github", "alice"); err == nil {
		t.Fatal("a lookup answered a secret this control plane does not hold")
	}
}

// plainStore is a store of design 026 and nothing more: desired state, no
// Secret collection. A control plane over one serves every other kind and
// answers the Secret kind with the refusal the API turns into
// capability_unsupported.
type plainStore struct{ objects map[string]v1.Sandbox }

func (s *plainStore) Load() (map[string]v1.Sandbox, error) { return s.objects, nil }
func (s *plainStore) Save(objects map[string]v1.Sandbox) error {
	s.objects = objects
	return nil
}
func (s *plainStore) Close() error { return nil }

// TestAStoreWithNoSecretCollectionServesNone is the other half: a store that
// does not hold the kind at all.
func TestAStoreWithNoSecretCollectionServesNone(t *testing.T) {
	c := openController(t, Options{Store: &plainStore{objects: map[string]v1.Sandbox{}},
		Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: &gateway{}})
	ctx := t.Context()
	if c.SecretsEnabled() {
		t.Fatal("a store with no Secret collection reports one")
	}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"create", func() error {
			_, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "x"), "alice")
			return err
		}},
		{"update", func() error {
			_, err := c.UpdateSecret(ctx, aSecret("github", "api.github.com", "x"), "sec_1")
			return err
		}},
		{"delete", func() error { _, err := c.DeleteSecret(ctx, "sec_1"); return err }},
	} {
		if err := tc.run(); !errors.Is(err, ErrNoSecretKey) {
			t.Fatalf("%s: %v, want ErrNoSecretKey", tc.name, err)
		}
	}
}

// TestTheMapCarriesTheValue is the compile path: the value the store holds
// reaches the map the gateway is handed, the placeholder reaches the
// workload's own environment, and the secret's scope joins the allow list.
func TestTheMapCarriesTheValue(t *testing.T) {
	c, gw, _ := sealedController(t)
	ctx := t.Context()
	if _, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice"); err != nil {
		t.Fatal(err)
	}
	obj, err := c.Create(ctx, mounting("github", "GITHUB_TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(obj.Status.Secrets.Mounted, []string{"github"}) || len(obj.Status.Secrets.NotInjectable) != 0 {
		t.Fatalf("status.secrets = %+v", obj.Status.Secrets)
	}
	pushed := gw.maps()
	m := pushed[len(pushed)-1]
	if len(m.Entries) != 1 || m.Entries[0].Value != "ghp_canary" {
		t.Fatalf("the map is %+v", m)
	}
	if !slices.Contains(m.Allow, "api.github.com") {
		t.Fatalf("allow = %v, want the secret's scope joined", m.Allow)
	}
	placeholder := m.Entries[0].Placeholder
	if !egress.IsPlaceholder(placeholder) {
		t.Fatalf("placeholder = %q", placeholder)
	}
	// The workload holds the placeholder under the key its manifest named,
	// and the manifest's own environment is untouched.
	spec := c.driver.(*gatewayDriver).specs[0]
	if spec.Env["GITHUB_TOKEN"] != placeholder {
		t.Fatalf("the sandbox's environment is %v", spec.Env)
	}
	if _, companion := spec.Env["GITHUB_TOKEN_HEADER"]; companion {
		t.Fatal("a secret that injects into Authorization was given a companion key")
	}
	if len(obj.Spec.Env) != 0 {
		t.Fatalf("the manifest's own environment was written to: %v", obj.Spec.Env)
	}
	// The record binds the mount to the object by id, so the placeholder
	// survives a recompile.
	held := c.objects[obj.Status.ID]
	if len(held.Status.EgressState.Secrets) != 1 || held.Status.EgressState.Secrets[0].Placeholder != placeholder {
		t.Fatalf("the mount record is %+v", held.Status.EgressState.Secrets)
	}
	if got := c.EgressMaps(ctx); len(got) != 1 || got[0].Entries[0].Placeholder != placeholder {
		t.Fatalf("a rebuild from desired state produced %+v", got)
	}
}

// TestACompanionKeySaysWhereToPutIt is the other half of what the sandbox
// sees: a secret that injects anywhere but Authorization names the place.
func TestACompanionKeySaysWhereToPutIt(t *testing.T) {
	c, _, _ := sealedController(t)
	ctx := t.Context()
	header := aSecret("vendor", "api.vendor.example", "k-1")
	header.Spec.Inject = v1.SecretInject{Header: "X-Api-Key", Scheme: v1.SchemeRaw}
	if _, err := c.CreateSecret(ctx, header, "alice"); err != nil {
		t.Fatal(err)
	}
	query := aSecret("search", "search.example.com", "q-1")
	query.Spec.Inject = v1.SecretInject{Query: "api_key", Scheme: v1.SchemeRaw}
	if _, err := c.CreateSecret(ctx, query, "alice"); err != nil {
		t.Fatal(err)
	}
	obj := mounting("vendor", "VENDOR_KEY")
	obj.Spec.Secrets = append(obj.Spec.Secrets, v1.SecretMount{Name: "search", Env: "SEARCH_KEY"})
	if _, err := c.Create(ctx, obj, "alice", 0); err != nil {
		t.Fatal(err)
	}
	spec := c.driver.(*gatewayDriver).specs[0]
	if spec.Env["VENDOR_KEY_HEADER"] != "X-Api-Key" {
		t.Fatalf("the header companion is %q", spec.Env["VENDOR_KEY_HEADER"])
	}
	if spec.Env["SEARCH_KEY_QUERY"] != "api_key" {
		t.Fatalf("the query companion is %q", spec.Env["SEARCH_KEY_QUERY"])
	}
}

// TestRotationAndRevocationReachTheGateway is spec 018's live update: a value
// written after the sandbox exists reaches the gateway at a higher version,
// and a delete leaves the placeholder in place and names the mount
// notInjectable.
func TestRotationAndRevocationReachTheGateway(t *testing.T) {
	c, gw, _ := sealedController(t)
	ctx := t.Context()
	secret, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_first"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := c.Create(ctx, mounting("github", "GITHUB_TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetSecret(ctx, "github", "alice"); err != nil || got.Status.MountedBy != 1 {
		t.Fatalf("mountedBy = %+v %v", got.Status, err)
	}
	first := lastMap(t, gw, obj.Status.ID)

	if _, err = c.UpdateSecret(ctx, aSecret("github", "api.github.com", "ghp_second"), secret.Status.ID); err != nil {
		t.Fatal(err)
	}
	second := lastMap(t, gw, obj.Status.ID)
	if second.Entries[0].Value != "ghp_second" || second.Version <= first.Version {
		t.Fatalf("the rotated map is %+v, the first was version %d", second, first.Version)
	}
	if second.Entries[0].Placeholder != first.Entries[0].Placeholder {
		t.Fatal("the rotation changed the placeholder the workload holds")
	}

	if _, err = c.DeleteSecret(ctx, secret.Status.ID); err != nil {
		t.Fatal(err)
	}
	third := lastMap(t, gw, obj.Status.ID)
	if len(third.Entries) != 0 || third.Version <= second.Version {
		t.Fatalf("the map after the delete is %+v", third)
	}
	held := c.objects[obj.Status.ID]
	if !slices.Equal(held.Status.Secrets.NotInjectable, []string{"github"}) {
		t.Fatalf("status.secrets = %+v", held.Status.Secrets)
	}
	// The workload's own environment does not change under it: the
	// placeholder stays and leaves as the inert string it always was.
	if len(held.Status.EgressState.Secrets) != 1 ||
		held.Status.EgressState.Secrets[0].Placeholder != first.Entries[0].Placeholder {
		t.Fatalf("the mount record is %+v", held.Status.EgressState.Secrets)
	}
}

// TestASecretTheBoundaryDeniesIsNotInjectable is the compiler's own answer,
// written into the sandbox's status beside the controller's.
func TestASecretTheBoundaryDeniesIsNotInjectable(t *testing.T) {
	c, _, _ := sealedController(t)
	ctx := t.Context()
	if _, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice"); err != nil {
		t.Fatal(err)
	}
	obj := mounting("github", "GITHUB_TOKEN")
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"api.github.com"}}
	created, err := c.Create(ctx, obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created.Status.Secrets.NotInjectable, []string{"github"}) {
		t.Fatalf("status.secrets = %+v", created.Status.Secrets)
	}
}

// TestAMountOfAnAbsentSecretIsNotInjectable is the state a sandbox reaches
// when its store lost the object under it: the sandbox still runs, the
// placeholder is still in its environment, and the status says the request
// will leave unauthenticated.
func TestAMountOfAnAbsentSecretIsNotInjectable(t *testing.T) {
	c, _, _ := sealedController(t)
	created, err := c.Create(t.Context(), mounting("absent", "TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created.Status.Secrets.NotInjectable, []string{"absent"}) {
		t.Fatalf("status.secrets = %+v", created.Status.Secrets)
	}
	spec := c.driver.(*gatewayDriver).specs[0]
	if !egress.IsPlaceholder(spec.Env["TOKEN"]) {
		t.Fatalf("the sandbox's environment is %v", spec.Env)
	}
}

// TestSecretsSurviveAReopen is what the snapshot store is for: the objects
// and their values are there after the process that wrote them is gone.
func TestSecretsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	sealer := newSealer(t)
	ctx := t.Context()
	first, err := OpenSealedFileStore(dir, sealer)
	if err != nil {
		t.Fatal(err)
	}
	c := openController(t, Options{Store: first, Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: &gateway{}})
	secret, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenSealedFileStore(dir, sealer)
	if err != nil {
		t.Fatal(err)
	}
	next := openController(t, Options{Store: second, Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: &gateway{}})
	got, err := next.GetSecret(ctx, "github", "alice")
	if err != nil || got.Status.ID != secret.Status.ID || got.Status.Version != 1 {
		t.Fatalf("after a reopen: %+v %v", got, err)
	}
	value, version, err := second.(Secrets).OpenValue(ctx, secret.Status.ID)
	if err != nil || string(value) != "ghp_canary" || version != 1 {
		t.Fatalf("the value after a reopen: %q %d %v", value, version, err)
	}
	if _, _, err = second.(Secrets).OpenValue(ctx, "sec_absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an absent value: %v", err)
	}
}

// TestASnapshotStoreWithNoSealerRefusesAValue keeps the one refusal the store
// itself owns, for a deployment that runs with no key.
func TestASnapshotStoreWithNoSealerRefusesAValue(t *testing.T) {
	store, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	secrets := store.(Secrets)
	if _, err = secrets.WriteSecret(context.Background(), aSecret("github", "api.github.com", "x"), []byte("x"), MutationSecretCreated); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("a write with no sealer: %v", err)
	}
	// An object with no value is still an object, and a removal of one that
	// is not there is not an error.
	if _, err = secrets.WriteSecret(context.Background(), aSecret("github", "api.github.com", ""), nil, MutationSecretCreated); err != nil {
		t.Fatal(err)
	}
	if err = secrets.RemoveSecret(context.Background(), "sec_absent", MutationSecretDeleted); err != nil {
		t.Fatal(err)
	}
	if _, _, err = secrets.OpenValue(context.Background(), "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a value that was never written: %v", err)
	}
}

// lastMap is the newest map the gateway was handed for one sandbox.
func lastMap(t *testing.T, gw *gateway, id string) egress.Map {
	t.Helper()
	principal := egress.Principal(id)
	for _, m := range slices.Backward(gw.maps()) {
		if m.Principal == principal {
			return m
		}
	}
	t.Fatalf("the gateway holds no map for %s", id)
	return egress.Map{}
}
