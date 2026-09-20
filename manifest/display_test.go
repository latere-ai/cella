// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"slices"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// desktop is an environment whose driver provides a screen and accepts input,
// so a test about a field is not also a test about a capability.
func desktop(name string) v1.Environment {
	env := container(name)
	env.Status.Capabilities.Display = true
	env.Status.Capabilities.Input = true
	return env
}

func desktopOptions() Options { return environmentOptions(desktop("default")) }

func withDisplay(d *v1.Display) v1.Sandbox {
	obj := sandbox()
	obj.Spec.Display = d
	return obj
}

func withPorts(ports ...v1.Port) v1.Sandbox {
	obj := sandbox()
	obj.Spec.Network.Ports = ports
	return obj
}

// TestDisplayFields is the display row of spec 003's field table: the bounds,
// the both-or-neither rule, and the geometry surviving a resolve unchanged.
func TestDisplayFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		display *v1.Display
		code    string // empty means the manifest resolves
		path    string
	}{
		{"absent", nil, "", ""},
		{"theSmallest", &v1.Display{Width: 320, Height: 240}, "", ""},
		{"theLargest", &v1.Display{Width: 7680, Height: 4320}, "", ""},
		{"ordinary", &v1.Display{Width: 1280, Height: 800}, "", ""},
		{"empty", &v1.Display{}, "missing_field", "spec.display"},
		{"widthAlone", &v1.Display{Width: 1280}, "invalid_field", "spec.display.width"},
		{"heightAlone", &v1.Display{Height: 800}, "invalid_field", "spec.display.width"},
		{"tooNarrow", &v1.Display{Width: 319, Height: 240}, "invalid_field", "spec.display.width"},
		{"tooWide", &v1.Display{Width: 7681, Height: 240}, "invalid_field", "spec.display.width"},
		{"tooShort", &v1.Display{Width: 320, Height: 239}, "invalid_field", "spec.display.height"},
		{"tooTall", &v1.Display{Width: 320, Height: 4321}, "invalid_field", "spec.display.height"},
		{"negative", &v1.Display{Width: -1, Height: -1}, "invalid_field", "spec.display.width"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code == "" {
				got := resolve(t, withDisplay(tc.display), desktopOptions())
				if tc.display == nil {
					if got.Sandbox.Spec.Display != nil {
						t.Fatalf("display = %+v, want none", got.Sandbox.Spec.Display)
					}
					return
				}
				if *got.Sandbox.Spec.Display != *tc.display {
					t.Fatalf("display = %+v, want %+v", got.Sandbox.Spec.Display, tc.display)
				}
				return
			}
			err := refusal(t, withDisplay(tc.display), desktopOptions())
			if err.Code != tc.code || err.Path != tc.path {
				t.Fatalf("error = %+v, want %s at %s", err, tc.code, tc.path)
			}
		})
	}
}

// TestDisplayCapability is the stage-7 row: a screen with no way to act on it
// is not the field, so half a desktop refuses the whole.
func TestDisplayCapability(t *testing.T) {
	for _, tc := range []struct {
		name             string
		display, input   bool
		wantCapabilityNo bool
	}{
		{"neither", false, false, true},
		{"screenOnly", true, false, true},
		{"inputOnly", false, true, true},
		{"both", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := container("default")
			env.Status.Capabilities.Display = tc.display
			env.Status.Capabilities.Input = tc.input
			o := environmentOptions(env)
			obj := withDisplay(&v1.Display{Width: 1280, Height: 800})
			if !tc.wantCapabilityNo {
				resolve(t, obj, o)
				return
			}
			err := refusal(t, obj, o)
			if err.Code != "capability_unsupported" || err.Path != "spec.display" {
				t.Fatalf("error = %+v, want capability_unsupported at spec.display", err)
			}
		})
	}
}

// TestPortFields is the ports row of spec 003's field table.
func TestPortFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports []v1.Port
		code  string
		path  string
	}{
		{"none", nil, "", ""},
		{"one", []v1.Port{{Name: "web", Port: 8080}}, "", ""},
		{"twoDistinct", []v1.Port{{Name: "web", Port: 8080}, {Name: "api", Port: 9090}}, "", ""},
		{"exposeNone", []v1.Port{{Name: "web", Port: 8080, Expose: v1.ExposeNone}}, "", ""},
		{"lowestPort", []v1.Port{{Name: "web", Port: 1}}, "", ""},
		{"highestPort", []v1.Port{{Name: "web", Port: 65535}}, "", ""},
		{"noName", []v1.Port{{Port: 8080}}, "missing_field", pathPorts + "[0].name"},
		{"badName", []v1.Port{{Name: "Web_1", Port: 8080}}, "invalid_field", pathPorts + "[0].name"},
		{"repeatedName", []v1.Port{{Name: "web", Port: 8080}, {Name: "web", Port: 9090}}, "invalid_field", pathPorts + "[1].name"},
		{"repeatedPort", []v1.Port{{Name: "web", Port: 8080}, {Name: "api", Port: 8080}}, "invalid_field", pathPorts + "[1].port"},
		{"zeroPort", []v1.Port{{Name: "web"}}, "invalid_field", pathPorts + "[0].port"},
		{"portTooHigh", []v1.Port{{Name: "web", Port: 65536}}, "invalid_field", pathPorts + "[0].port"},
		{"unknownExpose", []v1.Port{{Name: "web", Port: 80, Expose: "world"}}, "invalid_field", pathPorts + "[0].expose"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code == "" {
				got := resolve(t, withPorts(tc.ports...), containerOptions())
				if !slices.Equal(got.Sandbox.Spec.Network.Ports, tc.ports) {
					t.Fatalf("ports = %+v, want %+v", got.Sandbox.Spec.Network.Ports, tc.ports)
				}
				return
			}
			err := refusal(t, withPorts(tc.ports...), containerOptions())
			if err.Code != tc.code || err.Path != tc.path {
				t.Fatalf("error = %+v, want %s at %s", err, tc.code, tc.path)
			}
		})
	}
}

// TestPortCapability is the stage-7 row for a reach beyond the control plane's
// own routes. No driver of this repository declares either capability today,
// so both reaches are refused; an environment that declares one accepts it
// with no other change.
func TestPortCapability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expose v1.Expose
		mesh   bool
		in     bool
		want   string
	}{
		{"meshWithoutAMesh", v1.ExposeMesh, false, false, "capability_unsupported"},
		{"meshWithOne", v1.ExposeMesh, true, false, ""},
		{"publicWithoutAnExposer", v1.ExposePublic, false, false, "capability_unsupported"},
		{"publicWithOne", v1.ExposePublic, false, true, ""},
		{"noneNeedsNothing", v1.ExposeNone, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := container("default")
			env.Status.Capabilities.Mesh = tc.mesh
			env.Status.Capabilities.Ingress = tc.in
			o := environmentOptions(env)
			obj := withPorts(v1.Port{Name: "web", Port: 8080, Expose: tc.expose})
			if tc.want == "" {
				resolve(t, obj, o)
				return
			}
			err := refusal(t, obj, o)
			if err.Code != tc.want || err.Path != pathPorts+"[0].expose" {
				t.Fatalf("error = %+v, want %s at the expose field", err, tc.want)
			}
		})
	}
}

// TestDisplayAndPortsAreImmutable holds both fields to spec 003's table: the X
// server sizes its frame buffer once, and a port list a running sandbox is
// probed against does not move under it.
func TestDisplayAndPortsAreImmutable(t *testing.T) {
	seed := withDisplay(&v1.Display{Width: 1280, Height: 800})
	seed.Spec.Network.Ports = []v1.Port{{Name: "web", Port: 8080}}
	existing := resolve(t, seed, desktopOptions()).Sandbox
	o := desktopOptions()
	o.Existing = &existing

	same := existing
	same.Spec.Display = &v1.Display{Width: 1280, Height: 800}
	resolve(t, same, o)

	for _, tc := range []struct {
		name string
		edit func(*v1.Sandbox)
		path string
	}{
		{"resize", func(s *v1.Sandbox) { s.Spec.Display = &v1.Display{Width: 1920, Height: 1080} }, "spec.display"},
		{"removeTheDisplay", func(s *v1.Sandbox) { s.Spec.Display = nil }, "spec.display"},
		{"addAPort", func(s *v1.Sandbox) {
			s.Spec.Network.Ports = append(slices.Clone(s.Spec.Network.Ports), v1.Port{Name: "api", Port: 9090})
		}, "spec.network.ports"},
		{"removeThePorts", func(s *v1.Sandbox) { s.Spec.Network.Ports = nil }, "spec.network.ports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := existing
			obj.Spec.Display = &v1.Display{Width: 1280, Height: 800}
			obj.Spec.Network.Ports = slices.Clone(existing.Spec.Network.Ports)
			tc.edit(&obj)
			err := refusal(t, obj, o)
			if err.Code != "immutable_field" || !slices.Contains(err.Paths, tc.path) {
				t.Fatalf("error = %+v, want immutable_field naming %s", err, tc.path)
			}
		})
	}
}
