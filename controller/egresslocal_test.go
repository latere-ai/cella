// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"log/slog"
	"slices"
	"testing"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// openWithLocalEgress is a controller over the default environment's driver,
// a gateway with its doors, and two worker environments: "laptop", whose
// driver enforces its own allow list, and "plain", whose driver declares the
// same modes and enforces them through a gateway.
func openWithLocalEgress(t *testing.T) (*Controller, *gatewayDriver, *gatewayDriver, *gatewayDriver, *gateway) {
	t.Helper()
	clock := newClock()
	modes := []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}
	here := &gatewayDriver{modes: modes}
	laptop := &gatewayDriver{modes: modes, local: true}
	plain := &gatewayDriver{modes: modes}
	w := &worker{driver: newDriver(clock)}
	gw := &gateway{ca: "-----BEGIN CERTIFICATE-----\nauthority\n-----END CERTIFICATE-----\n"}
	c, err := Open(t.Context(), Options{
		Driver: here, Clock: clock, Environment: "default", DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		Egress: gw, Gateway: GatewayAddresses{Proxy: "gateway.example.internal:3128", Reverse: "gateway.example.internal:8080"},
		NewDriver: func(obj v1.Environment) (driver.Driver, error) {
			if obj.Metadata.Name == "laptop" {
				return laptop, nil
			}
			return plain, nil
		},
		Registrations: func(string) []Registration { return w.live() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, name := range []string{"laptop", "plain"} {
		applyWorker(t, c, name)
		ready(t, c, w, name)
	}
	return c, here, laptop, plain, gw
}

// TestEgressLocalNeedsNoGatewayAtTheController: an allow list on a worker
// environment whose driver enforces its own needs no gateway, is
// EgressEnforced True with DriverEnforced, is handed no door, credential or
// authority, and its map reaches no gateway; the same boundary on a worker
// environment that enforces through a gateway is refused, since no gateway
// serves that environment; and the control plane's own environment keeps
// its gateway and its projection.
func TestEgressLocalNeedsNoGatewayAtTheController(t *testing.T) {
	c, here, laptop, plain, gw := openWithLocalEgress(t)
	obj := bounded()
	obj.Metadata.Name = "on-laptop"
	obj.Spec.Environment = "laptop"
	created, err := realized(t.Context(), c, obj, "alice", 0)
	if err != nil {
		t.Fatalf("an allow list on the driver-enforced environment was refused: %v", err)
	}
	if cond := conditionOf(created, v1.ConditionEgressEnforced); cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonDriverEnforced {
		t.Errorf("EgressEnforced is %+v", cond)
	}
	if e := laptop.created(t).Egress; e.Mode != string(v1.EgressAllowlist) || e.ProxyAddr != "" || e.ReverseAddr != "" ||
		e.Credential != "" || e.CAPEM != "" {
		t.Errorf("the driver was handed %+v", e)
	}
	if len(gw.maps()) != 0 {
		t.Errorf("the default environment's gateway was handed %d maps of another environment", len(gw.maps()))
	}
	for _, m := range c.EgressMaps(t.Context()) {
		if m.Principal == egress.Principal(created.Status.ID) {
			t.Errorf("the gateway's seed carries the driver-enforced sandbox")
		}
	}

	// A boundary the driver cannot hold alone needs a gateway, which no
	// environment but the default has.
	denied := workspace()
	denied.Metadata.Name = "denied"
	denied.Spec.Environment = "laptop"
	denied.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"evil.example.com"}}
	if _, err := realized(t.Context(), c, denied, "alice", 0); !errors.Is(err, ErrNoGateway) {
		t.Errorf("open with a denied host on the driver-enforced environment answered %v", err)
	}
	gatewayed := bounded()
	gatewayed.Metadata.Name = "on-plain"
	gatewayed.Spec.Environment = "plain"
	if _, err := realized(t.Context(), c, gatewayed, "alice", 0); !errors.Is(err, ErrNoGateway) {
		t.Errorf("an allow list on a worker environment no gateway serves answered %v", err)
	}
	if len(plain.specs) != 0 {
		t.Errorf("the refused boundary reached the driver")
	}

	home := bounded()
	home.Metadata.Name = "at-home"
	if _, err := realized(t.Context(), c, home, "alice", 0); err != nil {
		t.Fatalf("an allow list on the default environment with a gateway failed: %v", err)
	}
	if e := here.created(t).Egress; e.ProxyAddr != "gateway.example.internal:3128" || e.Credential == "" || e.CAPEM == "" {
		t.Errorf("the default environment's sandbox lost its projection: %+v", e)
	}
	if len(gw.maps()) != 1 {
		t.Errorf("the default environment's map reached %d gateways", len(gw.maps()))
	}
}

// TestAnUpdatedBoundaryReachesTheDriverThatEnforcesIt: a driver that
// confines the workload with its own proxy is the one place its boundary is
// enforced, so an update that narrows the allow list hands the driver the
// new one, with no door or credential, and an update the driver did not
// take fails and keeps the boundary the driver still holds. An apply that
// leaves the boundary alone hands nothing, so it does not depend on the
// driver. A driver that enforces through a gateway is handed no boundary.
func TestAnUpdatedBoundaryReachesTheDriverThatEnforcesIt(t *testing.T) {
	c, here, laptop, _, _ := openWithLocalEgress(t)
	ctx := t.Context()
	obj := bounded()
	obj.Metadata.Name = "on-laptop"
	obj.Spec.Environment = "laptop"
	obj.Spec.Network.Egress.AllowedHosts = []string{"api.example.com", "files.example.com"}
	created, err := realized(ctx, c, obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	narrowed := created
	narrowed.Spec.Network.Egress.AllowedHosts = []string{"api.example.com"}
	if _, err = c.Update(ctx, narrowed); err != nil {
		t.Fatal(err)
	}
	got := laptop.egressChanges()
	if len(got) != 1 || got[0].Mode != string(v1.EgressAllowlist) || !slices.Equal(got[0].AllowedHosts, []string{"api.example.com"}) ||
		got[0].ProxyAddr != "" || got[0].Credential != "" || got[0].CAPEM != "" {
		t.Fatalf("the driver was handed %+v, want the narrowed allow list alone", got)
	}

	laptop.failUpdates(errors.New("the device is not there"))
	labelled := narrowed
	labelled.Metadata.Labels = map[string]string{"team": "a"}
	if _, err = c.Update(ctx, labelled); err != nil {
		t.Fatalf("an apply that left the boundary alone needed the driver: %v", err)
	}
	if got := laptop.egressChanges(); len(got) != 1 {
		t.Fatalf("an apply that left the boundary alone handed %d boundaries", len(got)-1)
	}
	closed := labelled
	closed.Spec.Network.Egress = v1.Egress{Mode: v1.EgressNone}
	if _, err = c.Update(ctx, closed); err == nil {
		t.Fatal("an update the driver did not take succeeded")
	}
	held, err := c.Get(ctx, created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if e := held.Spec.Network.Egress; e.Mode != v1.EgressAllowlist || !slices.Equal(e.AllowedHosts, []string{"api.example.com"}) {
		t.Errorf("the failed update left the boundary %+v", e)
	}

	home := bounded()
	home.Metadata.Name = "at-home"
	created, err = realized(ctx, c, home, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	created.Spec.Network.Egress.AllowedHosts = []string{"api.example.com", "files.example.com"}
	if _, err = c.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	if got := here.egressChanges(); len(got) != 0 {
		t.Errorf("a driver that enforces through a gateway was handed %+v", got)
	}
}

// TestNeedsGatewayOn: a driver-enforced boundary needs no gateway, and every
// other one that needs a gateway still does.
func TestNeedsGatewayOn(t *testing.T) {
	c, _, _, _, _ := openWithLocalEgress(t)
	allow := bounded().Spec
	if c.needsGatewayOn("laptop", allow) || !c.needsGatewayOn("plain", allow) || !c.needsGatewayOn("default", allow) {
		t.Error("an allow list's need of a gateway reads wrong per environment")
	}
	secret := allow
	secret.Secrets = []v1.SecretMount{{Name: "token"}}
	if !c.needsGatewayOn("laptop", secret) {
		t.Error("a mounted secret on a driver-enforced environment needs no gateway")
	}
	if c.gatewayHolds("laptop") || !c.gatewayHolds("default") {
		t.Error("a gateway holds the wrong environment")
	}
}

// TestEgressLocalHoldsADenyListItDeclares: open with denied hosts on a
// worker environment whose driver enforces its own network and declares
// open, as a computer that admits any public host but those it blocks does,
// needs no gateway, is EgressEnforced True with DriverEnforced, and reaches
// the driver with its denied hosts; the same on a driver that does not
// declare open still needs a gateway.
func TestEgressLocalHoldsADenyListItDeclares(t *testing.T) {
	clock := newClock()
	anyHost := &gatewayDriver{modes: []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist, v1.EgressOpen}, local: true}
	listHost := &gatewayDriver{modes: []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}, local: true}
	w := &worker{driver: newDriver(clock)}
	c, err := Open(t.Context(), Options{
		Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist}}, Clock: clock, Environment: "default",
		DataDir: t.TempDir(), Log: slog.New(slog.DiscardHandler), Egress: &gateway{},
		NewDriver: func(obj v1.Environment) (driver.Driver, error) {
			if obj.Metadata.Name == "any-host" {
				return anyHost, nil
			}
			return listHost, nil
		},
		Registrations: func(string) []Registration { return w.live() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, name := range []string{"any-host", "list-host"} {
		applyWorker(t, c, name)
		ready(t, c, w, name)
	}
	denying := func(name, environment string) v1.Sandbox {
		obj := workspace()
		obj.Metadata.Name = name
		obj.Spec.Environment = environment
		obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"pastebin.com"}}
		return obj
	}
	created, err := realized(t.Context(), c, denying("on-any", "any-host"), "alice", 0)
	if err != nil {
		t.Fatalf("open with a denied host on a driver that holds a deny list was refused: %v", err)
	}
	if cond := conditionOf(created, v1.ConditionEgressEnforced); cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonDriverEnforced {
		t.Errorf("EgressEnforced is %+v", cond)
	}
	if e := anyHost.created(t).Egress; e.Mode != string(v1.EgressOpen) || !slices.Equal(e.DeniedHosts, []string{"pastebin.com"}) || e.ProxyAddr != "" || e.Credential != "" {
		t.Errorf("the driver was handed %+v", e)
	}
	if !c.driverEnforced("any-host", denying("x", "any-host").Spec) || c.driverEnforced("list-host", denying("x", "list-host").Spec) {
		t.Error("a deny list's driver enforcement reads wrong per environment")
	}
	if _, err := realized(t.Context(), c, denying("on-list", "list-host"), "alice", 0); !errors.Is(err, ErrNoGateway) {
		t.Errorf("open with a denied host on a driver that declares no open answered %v", err)
	}
}
