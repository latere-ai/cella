// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"testing"

	"latere.ai/x/cella/client"
	"latere.ai/x/cella/internal/config"
)

// getenvOf is a config.Getenv over a map, so a case states the whole
// environment each loader reads.
func getenvOf(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

// TestControlPlaneURLRule is spec 013's control plane model, row 3: a
// non-loopback http:// CELLA_URL is refused by the worker, the gateway, and
// the client, and accepted by all three on loopback, over https, or with the
// escape hatch set. The worker and the gateway share the rule in
// internal/config; the client holds the same shape of its own
// (client.Config.Insecure, read from the same variable by
// client.Environment), so a program outside this module gets the one
// deployments get.
func TestControlPlaneURLRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		insecure bool
		refused  bool
	}{
		{"httpOffLoopback", "http://control.example.test", false, true},
		{"httpOnLoopbackIP", "http://127.0.0.1:8080", false, false},
		{"httpOnLocalhost", "http://localhost:8080", false, false},
		{"https", "https://control.example.test", false, false},
		{"httpOffLoopbackWithTheHatch", "http://control.example.test", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			insecureValue := ""
			if tc.insecure {
				insecureValue = "true"
			}

			_, workerErr := config.LoadWorker(getenvOf(map[string]string{
				"CELLA_URL": tc.url, "CELLA_ENVIRONMENT_KEY": "a-key", "CELLA_RUNTIME": "podman",
				"CELLA_INSECURE_CONTROL_PLANE": insecureValue,
			}))
			_, eggressErr := config.LoadEgress(getenvOf(map[string]string{
				"CELLA_URL": tc.url, "CELLA_ENVIRONMENT_KEY": "a-key",
				"CELLA_INSECURE_CONTROL_PLANE": insecureValue,
			}))
			_, clientErr := client.New(client.Config{URL: tc.url, Insecure: tc.insecure})

			for _, role := range []struct {
				name string
				err  error
			}{
				{"worker", workerErr}, {"gateway", eggressErr}, {"client", clientErr},
			} {
				switch {
				case tc.refused && role.err == nil:
					t.Errorf("the %s accepted %s", role.name, tc.url)
				case !tc.refused && role.err != nil:
					t.Errorf("the %s refused %s: %v", role.name, tc.url, role.err)
				}
			}
		})
	}
}
