// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"testing"
)

// TestCapabilitiesEqual: two declarations are equal when every flag, every
// egress mode in order, and every autosize flag agree, and unequal when any
// one of them differs.
func TestCapabilitiesEqual(t *testing.T) {
	full := Capabilities{
		Egress: []EgressMode{EgressNone, EgressAllowlist}, Mesh: true, Ingress: true, Volumes: true,
		VolumeImages: true, Snapshots: true, Attach: true, Dial: true, Display: true, Input: true, Resize: true,
		Pool: true, Files: true, Detach: true, Autosize: Autosize{CPU: true, Memory: true, Disk: true},
	}
	same := full
	same.Egress = []EgressMode{EgressNone, EgressAllowlist}
	if !full.Equal(same) || !(Capabilities{}).Equal(Capabilities{}) {
		t.Fatal("two declarations that say the same thing are unequal")
	}
	for name, change := range map[string]func(*Capabilities){
		"one egress mode fewer":    func(c *Capabilities) { c.Egress = c.Egress[:1] },
		"another egress mode":      func(c *Capabilities) { c.Egress = []EgressMode{EgressNone, EgressOpen} },
		"mesh":                     func(c *Capabilities) { c.Mesh = false },
		"ingress":                  func(c *Capabilities) { c.Ingress = false },
		"volumes":                  func(c *Capabilities) { c.Volumes = false },
		"volume images":            func(c *Capabilities) { c.VolumeImages = false },
		"snapshots":                func(c *Capabilities) { c.Snapshots = false },
		"attach":                   func(c *Capabilities) { c.Attach = false },
		"dial":                     func(c *Capabilities) { c.Dial = false },
		"display":                  func(c *Capabilities) { c.Display = false },
		"input":                    func(c *Capabilities) { c.Input = false },
		"resize":                   func(c *Capabilities) { c.Resize = false },
		"pool":                     func(c *Capabilities) { c.Pool = false },
		"files":                    func(c *Capabilities) { c.Files = false },
		"detach":                   func(c *Capabilities) { c.Detach = false },
		"the autosize of cpu":      func(c *Capabilities) { c.Autosize.CPU = false },
		"the autosize of memory":   func(c *Capabilities) { c.Autosize.Memory = false },
		"the autosize of the disk": func(c *Capabilities) { c.Autosize.Disk = false },
	} {
		other := full
		other.Egress = []EgressMode{EgressNone, EgressAllowlist}
		change(&other)
		if full.Equal(other) || other.Equal(full) {
			t.Errorf("two declarations that differ in %s are equal", name)
		}
	}
}

// TestAutosizeIsThreeFlagsOnTheWire: the autosize declaration is an object
// of three flags that is always written, a false one included, so a reader
// tells a resource the driver holds fixed from a declaration it never got;
// and And keeps a flag only where both declarations hold it.
func TestAutosizeIsThreeFlagsOnTheWire(t *testing.T) {
	for want, caps := range map[string]Capabilities{
		`{"cpu":false,"memory":false,"disk":false}`: {},
		`{"cpu":true,"memory":true,"disk":false}`:   {Autosize: Autosize{CPU: true, Memory: true}},
	} {
		encoded, err := json.Marshal(caps)
		if err != nil {
			t.Fatal(err)
		}
		var read struct {
			Autosize json.RawMessage `json:"autosize"`
		}
		if err := json.Unmarshal(encoded, &read); err != nil {
			t.Fatal(err)
		}
		if string(read.Autosize) != want {
			t.Errorf("capabilities.autosize is written %s, want %s", read.Autosize, want)
		}
		var back Capabilities
		if err := json.Unmarshal(encoded, &back); err != nil || back.Autosize != caps.Autosize {
			t.Errorf("read back %+v, %v, want %+v", back.Autosize, err, caps.Autosize)
		}
	}
	all := Autosize{CPU: true, Memory: true, Disk: true}
	for _, tc := range []struct{ a, b, want Autosize }{
		{all, all, all},
		{all, Autosize{}, Autosize{}},
		{Autosize{CPU: true, Memory: true}, Autosize{Memory: true, Disk: true}, Autosize{Memory: true}},
	} {
		if got := tc.a.And(tc.b); got != tc.want || tc.b.And(tc.a) != tc.want {
			t.Errorf("%+v and %+v is %+v, want %+v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestASandboxWithNoCeilingWritesNone: the three members of a sandbox's
// resources and status that a ceiling adds are absent from the bytes of a
// sandbox that names none, and present, in the names the contract gives
// them, on one that does.
func TestASandboxWithNoCeilingWritesNone(t *testing.T) {
	fixed := Sandbox{Spec: SandboxSpec{Resources: Resources{CPU: "1", Memory: "1Gi", Disk: "5Gi"}}}
	encoded, err := json.Marshal(fixed)
	if err != nil {
		t.Fatal(err)
	}
	var plain struct {
		Spec struct {
			Resources map[string]json.RawMessage `json:"resources"`
		} `json:"spec"`
		Status map[string]json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(encoded, &plain); err != nil {
		t.Fatal(err)
	}
	if _, has := plain.Spec.Resources["max"]; has || len(plain.Spec.Resources) != 3 {
		t.Errorf("the resources of a sandbox with no ceiling are written %s", encoded)
	}
	for _, member := range []string{"resources", "usage", "moves"} {
		if _, has := plain.Status[member]; has {
			t.Errorf("the status of a sandbox with no ceiling carries %s: %s", member, encoded)
		}
	}
	sized := fixed
	sized.Spec.Resources.Max = ResourceMax{CPU: "2", Memory: "6Gi"}
	sized.Status.Resources = Size{CPU: "2", Memory: "768Mi", Disk: "5Gi"}
	sized.Status.Usage = &Usage{Memory: 240 << 20}
	sized.Status.Moves = 1
	if encoded, err = json.Marshal(sized); err != nil {
		t.Fatal(err)
	}
	var written struct {
		Spec struct {
			Resources struct {
				Max map[string]string `json:"max"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Resources map[string]string `json:"resources"`
			Usage     struct {
				Memory int64  `json:"memory"`
				ReadAt string `json:"readAt"`
			} `json:"usage"`
			Moves int `json:"moves"`
		} `json:"status"`
	}
	if err := json.Unmarshal(encoded, &written); err != nil {
		t.Fatal(err)
	}
	if len(written.Spec.Resources.Max) != 2 || written.Spec.Resources.Max["memory"] != "6Gi" {
		t.Errorf("the ceiling is written %v", written.Spec.Resources.Max)
	}
	if written.Status.Resources["memory"] != "768Mi" || written.Status.Usage.Memory != 240<<20 || written.Status.Usage.ReadAt == "" || written.Status.Moves != 1 {
		t.Errorf("the status of a sized sandbox is written %s", encoded)
	}
}
