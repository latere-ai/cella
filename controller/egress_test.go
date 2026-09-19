// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// gateway is the environment's connected gateways as the controller sees
// them: what was pushed, in what order against the driver's own calls, and
// what was purged.
type gateway struct {
	mu       sync.Mutex
	order    *[]string
	sent     []egress.Map
	purged   []string
	sendErr  error
	ca       string
	sendOnly bool // the map is taken and never acknowledged
}

func (g *gateway) Send(_ context.Context, m egress.Map) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.order != nil {
		*g.order = append(*g.order, "egress.Send")
	}
	if g.sendErr != nil {
		return g.sendErr
	}
	g.sent = append(g.sent, m)
	if g.sendOnly {
		return ErrNoGateway
	}
	return nil
}

func (g *gateway) Purge(_ context.Context, principal string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.purged = append(g.purged, principal)
}

func (g *gateway) CA() string { return g.ca }

func (g *gateway) maps() []egress.Map {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sent)
}

// gatewayDriver notes every create against the shared order, and declares
// the egress modes a test wants it to enforce.
type gatewayDriver struct {
	runtimetest.Nop
	mu     sync.Mutex
	order  *[]string
	modes  []v1.EgressMode
	specs  []driver.CreateSpec
	create error
}

func (d *gatewayDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Egress: d.modes}
}

func (d *gatewayDriver) Create(_ context.Context, s driver.CreateSpec) (driver.Ref, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.order != nil {
		*d.order = append(*d.order, "driver.Create")
	}
	d.specs = append(d.specs, s)
	if d.create != nil {
		return driver.Ref{}, d.create
	}
	return driver.Ref{ID: s.ID}, nil
}

func (d *gatewayDriver) Inspect(_ context.Context, id string) (driver.State, error) {
	return driver.State{ID: id, Phase: driver.Running}, nil
}

func (d *gatewayDriver) created(t *testing.T) driver.CreateSpec {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.specs) != 1 {
		t.Fatalf("the driver was called %d times, want once", len(d.specs))
	}
	return d.specs[0]
}

// bounded is a sandbox whose boundary a gateway must hold.
func bounded() v1.Sandbox {
	obj := workspace()
	obj.Spec.Network.Egress = v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"api.example.com"}}
	return obj
}

func openController(t *testing.T, o Options) *Controller {
	t.Helper()
	if o.DataDir == "" && o.Store == nil {
		o.DataDir = t.TempDir()
	}
	if o.Environment == "" {
		o.Environment = "default"
	}
	c, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func conditionOf(obj v1.Sandbox, kind string) v1.Condition {
	for _, c := range obj.Status.Conditions {
		if c.Type == kind {
			return c
		}
	}
	return v1.Condition{}
}

// TestCreatePushesTheMapBeforeTheDriver is the order spec 018 fixes: a
// sandbox never starts before a gateway knows it.
func TestCreatePushesTheMapBeforeTheDriver(t *testing.T) {
	var order []string
	gw := &gateway{order: &order, ca: "-----BEGIN CERTIFICATE-----\nauthority\n-----END CERTIFICATE-----\n"}
	d := &gatewayDriver{order: &order, modes: []v1.EgressMode{v1.EgressAllowlist}}
	c := openController(t, Options{Driver: d, Egress: gw, Gateway: GatewayAddresses{Proxy: "gateway.example.internal:3128", Reverse: "gateway.example.internal:8080"}})
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"egress.Send", "driver.Create"}) {
		t.Fatalf("order = %v, want the map before the create", order)
	}
	sent := gw.maps()
	if len(sent) != 1 {
		t.Fatalf("%d maps pushed, want one", len(sent))
	}
	m := sent[0]
	if m.Principal != egress.Principal(obj.Status.ID) || m.Version != 1 || m.Credential == "" {
		t.Fatalf("map = %+v, want the sandbox's principal at version 1 with a credential", m)
	}
	if !slices.Equal(m.Allow, []string{"api.example.com"}) {
		t.Fatalf("allow = %v", m.Allow)
	}
	// The driver receives the boundary, the doors, the credential and the
	// authority, so the workload is pointed at the gateway that holds it.
	spec := d.created(t)
	if spec.Egress.Credential != m.Credential || spec.Egress.Mode != string(v1.EgressAllowlist) {
		t.Fatalf("create spec egress = %+v", spec.Egress)
	}
	if spec.Egress.ProxyAddr != "gateway.example.internal:3128" || spec.Egress.ReverseAddr != "gateway.example.internal:8080" {
		t.Fatalf("doors = %q, %q", spec.Egress.ProxyAddr, spec.Egress.ReverseAddr)
	}
	if !strings.Contains(spec.Egress.CAPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("authority = %q, want the gateway's", spec.Egress.CAPEM)
	}
	if cond := conditionOf(obj, v1.ConditionEgressEnforced); cond.Status != v1.ConditionTrue || cond.Reason != v1.ReasonEnforced {
		t.Fatalf("condition = %+v, want enforced", cond)
	}
}

// TestCreateWaitsForTheGateway holds the refusal: a boundary no gateway will
// hold leaves no sandbox behind.
func TestCreateWaitsForTheGateway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		egress Egress
	}{
		{"noGatewayAtAll", nil},
		{"noneAcknowledges", &gateway{sendOnly: true}},
		{"theStreamFailed", &gateway{sendErr: errors.New("the stream is closed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}
			c := openController(t, Options{Driver: d, Egress: tc.egress})
			if _, err := c.Create(t.Context(), bounded(), "alice", 0); err == nil {
				t.Fatal("a boundary with no gateway was created")
			}
			if len(c.List()) != 0 {
				t.Fatalf("the refused sandbox is still listed: %+v", c.List())
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.specs) != 0 {
				t.Fatal("the driver was called for a sandbox whose map reached no gateway")
			}
		})
	}
	// The error the API turns into an unavailable environment.
	c := openController(t, Options{Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}})
	if _, err := c.Create(t.Context(), bounded(), "alice", 0); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("Create = %v, want %v", err, ErrNoGateway)
	}
}

// TestOpenBoundaryNeedsNoGateway is the carve-out: a sandbox that reaches
// everything and substitutes nothing has nothing for a gateway to hold, so
// an installation that runs none still creates it, with the condition saying
// what it got.
func TestOpenBoundaryNeedsNoGateway(t *testing.T) {
	d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressOpen}}
	c := openController(t, Options{Driver: d})
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	cond := conditionOf(obj, v1.ConditionEgressEnforced)
	if cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonNoGateway {
		t.Fatalf("condition = %+v, want false with no gateway", cond)
	}
	// A denied host is something to enforce, so that one does wait.
	denied := workspace()
	denied.Spec.Network.Egress = v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"api.example.com"}}
	denied.Metadata.Name = "denied"
	if _, err = c.Create(t.Context(), denied, "alice", 0); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("Create = %v, want a denied host to need a gateway", err)
	}
}

// TestEgressEnforcedNeedsBothPoints holds the condition to the conjunction:
// the driver confines the workload and the gateway holds the map.
func TestEgressEnforcedNeedsBothPoints(t *testing.T) {
	gw := &gateway{}
	d := &gatewayDriver{} // declares no egress enforcement
	c := openController(t, Options{Driver: d, Egress: gw})
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	cond := conditionOf(obj, v1.ConditionEgressEnforced)
	if cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonNotEnforcedByDriver {
		t.Fatalf("condition = %+v, want false because the driver enforces nothing", cond)
	}
}

// TestCreatePurgesWhenTheDriverRefuses keeps a gateway from holding a map for
// a sandbox that does not exist.
func TestCreatePurgesWhenTheDriverRefuses(t *testing.T) {
	gw := &gateway{}
	d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}, create: errors.New("no room")}
	c := openController(t, Options{Driver: d, Egress: gw})
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err == nil {
		t.Fatal("a failed create was reported as a success")
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if !slices.Equal(gw.purged, []string{egress.Principal(obj.Status.ID)}) {
		t.Fatalf("purged = %v, want the sandbox's principal", gw.purged)
	}
}

func TestDeletePurgesTheMap(t *testing.T) {
	gw := &gateway{}
	d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}
	c := openController(t, Options{Driver: d, Egress: gw})
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if !slices.Equal(gw.purged, []string{egress.Principal(obj.Status.ID)}) {
		t.Fatalf("purged = %v, want the deleted sandbox's principal", gw.purged)
	}
}

// TestTheCredentialIsDesiredStateAndNotAnAnswer holds the two halves of where
// the credential lives: in the object the store keeps, and in no response.
func TestTheCredentialIsDesiredStateAndNotAnAnswer(t *testing.T) {
	dir := t.TempDir()
	gw := &gateway{}
	open := func() *Controller {
		store, err := OpenFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Open(Options{Store: store, Environment: "default", Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: gw})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	credential := gw.maps()[0].Credential
	for _, read := range []v1.Sandbox{obj, mustGet(t, c, obj.Status.ID), c.List()[0]} {
		if read.Status.EgressState != nil {
			t.Fatalf("a response carries the boundary's own record: %+v", read.Status.EgressState)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	// A control plane that restarts hands a gateway the same credential the
	// running sandbox holds in its own environment.
	second := open()
	defer func() { _ = second.Close() }()
	maps := second.EgressMaps()
	if len(maps) != 1 || maps[0].Credential != credential {
		t.Fatalf("maps after a restart = %+v, want the same credential %q", maps, credential)
	}
	if maps[0].Version != 1 {
		t.Fatalf("version = %d, want the stored one; reading maps must not bump it", maps[0].Version)
	}
}

// TestEgressMapsLeavesOutWhatIsGoing keeps a deleting sandbox out of a
// snapshot, so a gateway that connects mid-delete does not admit it again.
func TestEgressMapsLeavesOutWhatIsGoing(t *testing.T) {
	gw := &gateway{}
	c := openController(t, Options{Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: gw})
	obj, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EgressMaps()) != 1 {
		t.Fatalf("maps = %+v, want the live sandbox", c.EgressMaps())
	}
	c.mu.Lock()
	stored := c.objects[obj.Status.ID]
	stored.Status.Phase = "Deleting"
	c.objects[obj.Status.ID] = stored
	c.mu.Unlock()
	if maps := c.EgressMaps(); len(maps) != 0 {
		t.Fatalf("maps = %+v, want none for a sandbox that is going", maps)
	}
	// An object whose stored boundary does not compile is left out rather
	// than handed over half made.
	c.mu.Lock()
	stored.Status.Phase = driver.Running
	stored.Spec.Network.Egress.Mode = ""
	c.objects[obj.Status.ID] = stored
	c.mu.Unlock()
	if maps := c.EgressMaps(); len(maps) != 0 {
		t.Fatalf("maps = %+v, want none for a boundary that does not compile", maps)
	}
}

func TestSetCondition(t *testing.T) {
	now := workspace().Status.CreatedAt
	first := v1.Condition{Type: v1.ConditionEgressEnforced, Status: v1.ConditionTrue, Reason: v1.ReasonEnforced, Since: now}
	conditions := setCondition(nil, first)
	if len(conditions) != 1 {
		t.Fatalf("conditions = %+v", conditions)
	}
	// The same answer keeps the instant it was first given.
	later := first
	later.Since = now.AddDate(0, 0, 1)
	conditions = setCondition(conditions, later)
	if !conditions[0].Since.Equal(first.Since) {
		t.Fatalf("since = %v, want the instant the answer last changed", conditions[0].Since)
	}
	// A different answer takes the new instant and replaces the old row.
	changed := v1.Condition{Type: v1.ConditionEgressEnforced, Status: v1.ConditionFalse, Reason: v1.ReasonNoGateway, Since: later.Since}
	conditions = setCondition(conditions, changed)
	if len(conditions) != 1 || conditions[0].Status != v1.ConditionFalse || !conditions[0].Since.Equal(later.Since) {
		t.Fatalf("conditions = %+v, want the new answer at the new instant", conditions)
	}
	// A condition of another type joins rather than replaces.
	if conditions = setCondition(conditions, v1.Condition{Type: v1.ConditionReady, Status: v1.ConditionTrue}); len(conditions) != 2 {
		t.Fatalf("conditions = %+v, want two", conditions)
	}
}

func mustGet(t *testing.T, c *Controller, id string) v1.Sandbox {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return obj
}
