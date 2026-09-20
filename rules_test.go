// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"latere.ai/x/cella/internal/metrics"
)

// rulesPath is the alert rules an installation applies, which sit beside the
// base kustomization and outside it: the kind they declare is the Prometheus
// Operator's, and a cluster without that operator refuses the apply.
const rulesPath = "deploy/base/prometheusrule.yaml"

// rule is one alert as the document holds it.
type rule struct {
	Alert       string            `json:"alert"`
	Expr        string            `json:"expr"`
	For         string            `json:"for"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// rulesDocument is the PrometheusRule's own shape, read as far as the checks
// below need it.
type rulesDocument struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []rule `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

func readRules(t *testing.T) rulesDocument {
	t.Helper()
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("reading the alert rules: %v", err)
	}
	var doc rulesDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s does not parse: %v", rulesPath, err)
	}
	if doc.Kind != "PrometheusRule" || doc.APIVersion != "monitoring.coreos.com/v1" {
		t.Fatalf("%s is %s/%s, want monitoring.coreos.com/v1 PrometheusRule", rulesPath, doc.APIVersion, doc.Kind)
	}
	return doc
}

func allRules(t *testing.T) []rule {
	t.Helper()
	var out []rule
	for _, group := range readRules(t).Spec.Groups {
		out = append(out, group.Rules...)
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no alert; the checks would pass vacuously", rulesPath)
	}
	return out
}

// selector matches one metric reference with its optional label matchers.
var selector = regexp.MustCompile(`(cella_[a-z_]+)(\{([^}]*)\})?`)

// matcher matches one label matcher inside a selector.
var matcher = regexp.MustCompile(`([a-z_]+)\s*(=~|!~|!=|=)\s*"`)

// aggregation matches a by or without clause and its label list.
var aggregation = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)

// base strips the suffix the exposition gives a histogram's series, so the
// name a rule reads is looked up as the table declares it.
func base(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok {
			return trimmed
		}
	}
	return name
}

// TestAlertsNameKnownMetrics is design 017's rule for the rules file: every
// cella_ metric an alert reads is in the table, and every label it matches on
// is one that metric carries. A metric another exporter publishes does not
// carry the prefix and is not checked.
func TestAlertsNameKnownMetrics(t *testing.T) {
	for _, r := range allRules(t) {
		for _, m := range selector.FindAllStringSubmatch(r.Expr, -1) {
			name := base(m[1])
			if !metrics.Declared(name) {
				t.Errorf("%s reads %s, which design 017's table does not name", r.Alert, m[1])
				continue
			}
			declared, _ := metrics.LabelsOf(name)
			for _, lm := range matcher.FindAllStringSubmatch(m[3], -1) {
				if !slices.Contains(declared, lm[1]) {
					t.Errorf("%s matches %s on the label %q, which it does not carry: %v", r.Alert, name, lm[1], declared)
				}
			}
		}
	}
}

// TestAlertsAggregateOnLabelsThatExist holds a by or without clause to the
// same table. An aggregation on a label no series carries silently collapses
// every series into one, which is an alert that fires on the wrong thing.
func TestAlertsAggregateOnLabelsThatExist(t *testing.T) {
	known := map[string]bool{"le": true} // le is the exposition's own, on every histogram bucket.
	for _, row := range metrics.Table {
		for _, label := range row.Labels {
			known[label] = true
		}
	}
	for _, r := range allRules(t) {
		if !strings.Contains(r.Expr, "cella_") {
			continue
		}
		for _, m := range aggregation.FindAllStringSubmatch(r.Expr, -1) {
			for label := range strings.SplitSeq(m[1], ",") {
				label = strings.TrimSpace(label)
				if label != "" && !known[label] {
					t.Errorf("%s aggregates by %q, which no metric in design 017's table carries", r.Alert, label)
				}
			}
		}
	}
}

// TestEveryAlertCarriesItsRunbookSentence is what an operator reads at three
// in the morning: what is wrong, how long it has held, how bad it is, and
// what to look at next.
func TestEveryAlertCarriesItsRunbookSentence(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range allRules(t) {
		switch {
		case r.Alert == "":
			t.Errorf("an alert has no name: %+v", r)
			continue
		case seen[r.Alert]:
			t.Errorf("%s is declared twice", r.Alert)
		}
		seen[r.Alert] = true
		if r.For == "" {
			t.Errorf("%s has no for: an alert with no duration fires on one scrape", r.Alert)
		}
		if s := r.Labels["severity"]; s != "critical" && s != "warning" {
			t.Errorf("%s has severity %q, want critical or warning", r.Alert, s)
		}
		if r.Annotations["summary"] == "" {
			t.Errorf("%s has no summary", r.Alert)
		}
		if d := r.Annotations["description"]; d == "" || !strings.HasSuffix(strings.TrimSpace(d), ".") {
			t.Errorf("%s has no runbook sentence: %q", r.Alert, d)
		}
	}
}

// TestAlertsCoverTheTableRowsAnOperatorWatches holds the file to the alerts
// design 017 names. A row of the table with no alert is a number nobody is
// told about.
func TestAlertsCoverTheTableRowsAnOperatorWatches(t *testing.T) {
	var expressions strings.Builder
	for _, r := range allRules(t) {
		expressions.WriteString(r.Expr)
		expressions.WriteString("\n")
	}
	for _, name := range []string{
		"cella_decisions_total",
		"cella_events_pending",
		"cella_events_delivered_total",
		"cella_lease_held",
		"cella_sandbox_create_duration_seconds",
		"cella_sandboxes",
		"cella_environments",
		"cella_operations_redelivered_total",
	} {
		if !strings.Contains(expressions.String(), name) {
			t.Errorf("no alert reads %s, which design 017's alert table names", name)
		}
	}
}

// TestRulesCarryNoLatereCoordinates keeps the file an installation's own: the
// job label, the namespace and the thresholds are the operator's.
func TestRulesCarryNoLatereCoordinates(t *testing.T) {
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, coordinate := range []string{"cella.latere.ai", "dash0", "sandbox-base", "doks"} {
		if strings.Contains(strings.ToLower(string(data)), coordinate) {
			t.Errorf("%s names %q, which is a Latere coordinate and not an operator's value", rulesPath, coordinate)
		}
	}
}

// TestRulesAreOutsideTheBase is design 048's placement: the kind is the
// Prometheus Operator's CRD and a cluster without it refuses the apply, so
// the file is applied on its own and never by the base kustomization.
func TestRulesAreOutsideTheBase(t *testing.T) {
	data, err := os.ReadFile("deploy/base/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(data, &kustomization); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(kustomization.Resources, "prometheusrule.yaml") {
		t.Error("the base kustomization includes prometheusrule.yaml; a cluster without the operator would fail the apply")
	}
}
