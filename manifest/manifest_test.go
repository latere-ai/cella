// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

func base() v1.Sandbox {
	return v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "test"}}
}
func TestDecode(t *testing.T) {
	b, _ := json.Marshal(base())
	obj, err := Decode(b, "application/json; charset=utf-8")
	if err != nil || obj.Kind != "Sandbox" {
		t.Fatal(obj, err)
	}
	for _, tc := range []struct{ body, media string }{{string(b), "text/yaml"}, {"{", "application/json"}, {string(b) + " {}", "application/json"}, {`{"unknown":1}`, "application/json"}, {`{"apiVersion":"other"}`, "application/json"}, {`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret"}`, "application/json"}, {`null`, "application/json"}} {
		if _, err := Decode([]byte(tc.body), tc.media); err == nil {
			t.Errorf("accepted %s", tc.body)
		}
	}
	obj = base()
	obj.Status.Owner = "forged"
	b, _ = json.Marshal(obj)
	got, err := Decode(b, "application/json")
	if err != nil || got.Status.Owner != "" {
		t.Fatal(got, err)
	}
}
func TestResolve(t *testing.T) {
	obj, err := ResolveNative(base(), "default")
	if err != nil || obj.Spec.Environment != "default" || obj.Spec.Workdir != "/workspace" {
		t.Fatal(obj, err)
	}
	tests := []func(*v1.Sandbox){
		func(o *v1.Sandbox) { o.APIVersion = "bad" }, func(o *v1.Sandbox) { o.Metadata.Name = "INVALID" }, func(o *v1.Sandbox) { o.Metadata.Name = strings.Repeat("a", 64) },
		func(o *v1.Sandbox) { o.Metadata.Labels = map[string]string{"cella.latere.ai/owner": "evil"} }, func(o *v1.Sandbox) { o.Metadata.Labels = map[string]string{"bad key": "v"} }, func(o *v1.Sandbox) { o.Metadata.Labels = map[string]string{"key": "bad value"} }, func(o *v1.Sandbox) { o.Metadata.Annotations = map[string]string{"key": strings.Repeat("v", 4097)} },
		func(o *v1.Sandbox) { o.Spec.Environment = "other" }, func(o *v1.Sandbox) { o.Spec.Image = "ubuntu" }, func(o *v1.Sandbox) { o.Spec.Command = []string{"sh"} }, func(o *v1.Sandbox) { o.Spec.Args = []string{"a"} }, func(o *v1.Sandbox) { o.Spec.Workdir = "/etc" },
		func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"bad-key": "v"} }, func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"A": "\x00"} }, func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"CELLA_OWNER": "v"} }, func(o *v1.Sandbox) { o.Spec.Env = map[string]string{"A": strings.Repeat("a", 32769)} },
	}
	for i, mut := range tests {
		obj := base()
		mut(&obj)
		if _, err := ResolveNative(obj, "default"); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	obj = base()
	obj.Metadata.Labels = map[string]string{"example.org/team": "research", "empty": ""}
	obj.Metadata.Annotations = map[string]string{"example.org/note": "any value"}
	obj.Spec.Env = map[string]string{"A": "b"}
	if _, err := ResolveNative(obj, "default"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a/b/c", "a/", strings.Repeat("a", 64), strings.Repeat("a", 254) + "/b", "BAD.org/key", strings.Repeat("a", 64) + ".org/key"} {
		if validKey(key) {
			t.Error(key)
		}
	}
	for _, key := range []string{"CELLA_X", "HTTP_PROXY", "https_proxy", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if !ReservedEnv(key) {
			t.Error(key)
		}
	}
	if ReservedEnv("PATH") {
		t.Fatal("PATH reserved")
	}
}
func TestExecValidation(t *testing.T) {
	for _, tc := range []struct {
		cmd []string
		env map[string]string
		cwd string
	}{{nil, nil, ""}, {[]string{""}, nil, ""}, {[]string{"echo", "\x00"}, nil, ""}, {[]string{"echo"}, map[string]string{"NO_PROXY": "x"}, ""}, {[]string{"echo"}, map[string]string{"a-b": "x"}, ""}, {[]string{"echo"}, nil, "/workspace/../etc"}, {[]string{"echo"}, nil, "/etc"}, {[]string{"echo"}, nil, "relative"}} {
		if err := ValidateExec(tc.cmd, tc.env, tc.cwd); err == nil {
			t.Fatal(tc)
		}
	}
	for _, cwd := range []string{"", "/workspace", "/workspace/sub"} {
		if err := ValidateExec([]string{"echo"}, map[string]string{"A": "b"}, cwd); err != nil {
			t.Fatal(err)
		}
	}
	if (&Error{"code", "detail"}).Error() != "code: detail" {
		t.Fatal("error formatting")
	}
}
