// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"latere.ai/x/cella/internal/cellaclient"
	v1 "latere.ai/x/cella/manifest/v1"
)

// The output forms of design 011. `wide` adds columns, `name` is one
// `<kind>/<name>` per line, and `json` is the API's own bytes.
const (
	outputColumns = ""
	outputJSON    = "json"
	outputWide    = "wide"
	outputName    = "name"
)

// outputs is every form -o takes here. YAML is not among them: no handler of
// this API reads Accept, so a caller asking for YAML would be handed JSON
// under another name.
var outputs = []string{outputJSON, outputWide, outputName}

// writeRaw writes a response's own bytes, which is what -o json promises:
// never decoded and re-encoded, so the field order is the API's.
func writeRaw(w io.Writer, raw []byte) error {
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		_, err := io.WriteString(w, "\n")
		return err
	}
	return nil
}

// writeItems writes a list as one envelope: every item's bytes unchanged and
// the cursor empty, because the pages have all been followed.
func writeItems(w io.Writer, items []json.RawMessage) error {
	if items == nil {
		items = []json.RawMessage{}
	}
	body, err := json.Marshal(map[string]any{"items": items, "next": ""})
	if err != nil {
		return err
	}
	return writeRaw(w, body)
}

// writeValue writes a value the command composed rather than received,
// which is what --json answers for a route whose answer carries nothing.
func writeValue(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeRaw(w, body)
}

// table writes rows under a header, aligned.
func table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// sandboxColumns is the row of design 011's output table for a Sandbox.
func sandboxColumns(wide bool) []string {
	header := []string{"NAME", "PHASE", "READY", "IMAGE", "AGE", "OWNER"}
	if wide {
		header = append(header, "ID", "ENVIRONMENT", "DRIVER")
	}
	return header
}

func sandboxRow(obj v1.Sandbox, wide bool, now time.Time) []string {
	row := []string{
		dash(obj.Metadata.Name), dash(obj.Status.Phase), conditionOf(obj, v1.ConditionReady),
		dash(obj.Spec.Image), age(obj.Status.CreatedAt, now), dash(obj.Status.Owner),
	}
	if wide {
		row = append(row, dash(obj.Status.ID), dash(obj.Status.Environment), dash(obj.Status.Driver))
	}
	return row
}

// secretColumns is the row for a Secret. No column carries a value, because
// no answer does.
func secretColumns(wide bool) []string {
	header := []string{"NAME", "KIND", "HOSTS", "VERSION", "MOUNTED", "OWNER"}
	if wide {
		header = append(header, "ID", "UPDATED")
	}
	return header
}

func secretRow(obj v1.Secret, wide bool, now time.Time) []string {
	row := []string{
		dash(obj.Metadata.Name), dash(obj.Spec.Kind), dash(strings.Join(obj.Spec.Scope.Hosts, ",")),
		strconv.Itoa(obj.Status.Version), strconv.Itoa(obj.Status.MountedBy), dash(obj.Status.Owner),
	}
	if wide {
		row = append(row, dash(obj.Status.ID), age(obj.Status.UpdatedAt, now))
	}
	return row
}

// writeSandboxes writes the columns a person reads.
func writeSandboxes(w io.Writer, items []v1.Sandbox, wide bool, now time.Time) error {
	rows := make([][]string, 0, len(items))
	for _, obj := range items {
		rows = append(rows, sandboxRow(obj, wide, now))
	}
	return table(w, sandboxColumns(wide), rows)
}

func writeSecrets(w io.Writer, items []v1.Secret, wide bool, now time.Time) error {
	rows := make([][]string, 0, len(items))
	for _, obj := range items {
		rows = append(rows, secretRow(obj, wide, now))
	}
	return table(w, secretColumns(wide), rows)
}

// writeNames is -o name: one `<kind>/<name>` per line, which is what a
// shell loop reads.
func writeNames(w io.Writer, kind cellaclient.Kind, names []string) error {
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "%s/%s\n", kind, name); err != nil {
			return err
		}
	}
	return nil
}

// writeEntries writes a directory listing.
func writeEntries(w io.Writer, entries []cellaclient.FileEntry) error {
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		kind := "file"
		if e.IsDir {
			kind = "dir"
		}
		rows = append(rows, []string{e.Mode, kind, strconv.FormatInt(e.Size, 10), e.ModTime.UTC().Format(time.RFC3339), e.Name})
	}
	return table(w, []string{"MODE", "TYPE", "SIZE", "MODIFIED", "NAME"}, rows)
}

// conditionOf renders one condition's status, which is what the READY
// column is.
func conditionOf(obj v1.Sandbox, kind string) string {
	for _, c := range obj.Status.Conditions {
		if c.Type == kind {
			return c.Status
		}
	}
	return v1.ConditionUnknown
}

// age is how long ago an instant was, in the one unit that reads at that
// scale. An instant that is not set is a dash.
func age(at, now time.Time) string {
	if at.IsZero() {
		return "-"
	}
	d := now.Sub(at)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// dash renders an empty field as one character rather than as nothing, so a
// column that is missing is visible in the row.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
