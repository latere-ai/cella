// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"log/slog"
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
