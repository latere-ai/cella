// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"latere.ai/x/cella/internal/cellaclient"
	v1 "latere.ai/x/cella/manifest/v1"
)

// maxDocumentBytes bounds a manifest read from a file or from standard
// input. The API's own cap is smaller; this one keeps a mistyped path from
// reading a disk into memory.
const maxDocumentBytes = 1 << 20

// pollInterval is how often -w reads the object it is waiting for.
const pollInterval = 250 * time.Millisecond

// document is the header of a manifest: what the command needs to send it to
// the right route. The rest of the bytes travel unchanged.
type document struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// apply sends a manifest. The document says which kind it is and the bytes
// are the caller's own, so what the server refuses is what the caller wrote.
//
// The document is JSON: manifest.Decode and manifest.DecodeSecret accept
// application/json and nothing else, so a YAML file has no route to send it
// to and is refused here rather than at the server.
func apply(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella apply -f <file> [-w] [--value-from-env <name> | --value-file <path>]")
	file := fs.String("f", "", "the manifest to apply, or - for standard input")
	wait := fs.Bool("w", false, "wait for the sandbox to be Running")
	valueEnv := fs.String("value-from-env", "", "read a Secret's value from this environment variable")
	valueFile := fs.String("value-file", "", "read a Secret's value from this file")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long -w waits")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("apply takes no argument; the manifest is -f")
	}
	if *file == "" {
		return usagef("apply needs -f <file>, or -f - to read standard input")
	}
	body, err := c.read(*file)
	if err != nil {
		return err
	}
	var header document
	if err = json.Unmarshal(body, &header); err != nil {
		return usagef("the manifest is not one JSON object: %v", err)
	}
	if header.APIVersion == "" || header.Kind == "" {
		return usagef("the manifest names no apiVersion and kind")
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	switch header.Kind {
	case v1.KindSecret:
		if header.Metadata.Name == "" {
			return usagef("a Secret is applied by name; the manifest names none")
		}
		if body, err = withValue(body, c, *valueEnv, *valueFile); err != nil {
			return err
		}
		obj, raw, err := client.ApplySecret(ctx, header.Metadata.Name, body)
		if err != nil {
			return err
		}
		return c.applied(cellaclient.KindSecret, obj.Metadata.Name, raw)
	case "Sandbox":
		if *valueEnv != "" || *valueFile != "" {
			return usagef("a value belongs to a Secret, and this manifest is a Sandbox")
		}
		obj, raw, err := client.CreateSandbox(ctx, body)
		if err != nil {
			return err
		}
		if *wait {
			if obj, raw, err = waitForRunning(ctx, client, obj.Status.ID, *timeout); err != nil {
				return err
			}
		}
		return c.applied(cellaclient.KindSandbox, obj.Metadata.Name, raw)
	default:
		return usagef("this server serves no kind %q", header.Kind)
	}
}

// applied reports one apply: the API's own body under --json, and the
// object's name otherwise.
func (c *invocation) applied(kind cellaclient.Kind, name string, raw []byte) error {
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	_, err := fmt.Fprintf(c.Stdout, "%s/%s applied\n", kind, name)
	return err
}

// read reads a manifest from a file or from standard input.
func (c *invocation) read(path string) ([]byte, error) {
	var source io.Reader
	if path == "-" {
		source = c.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, usageError{err}
		}
		defer func() { _ = f.Close() }()
		source = f
	}
	body, err := io.ReadAll(io.LimitReader(source, maxDocumentBytes))
	if err != nil {
		return nil, usageError{err}
	}
	if len(body) == 0 {
		return nil, usagef("the manifest is empty")
	}
	return body, nil
}

// withValue puts a Secret's value into the document it is applied with. It
// is the one place the command writes into a manifest, and the value reaches
// no output: it is read, placed, and sent.
func withValue(body []byte, c *invocation, fromEnv, fromFile string) ([]byte, error) {
	switch {
	case fromEnv != "" && fromFile != "":
		return nil, usagef("--value-from-env and --value-file name two sources for one value")
	case fromEnv == "" && fromFile == "":
		return body, nil
	}
	value := c.Getenv(fromEnv)
	if fromFile != "" {
		data, err := os.ReadFile(fromFile)
		if err != nil {
			return nil, usageError{err}
		}
		value = strings.TrimRight(string(data), "\r\n")
	} else if value == "" {
		return nil, usagef("%s holds no value", fromEnv)
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, usagef("the manifest is not one JSON object: %v", err)
	}
	spec, _ := object["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["value"] = value
	object["spec"] = spec
	out, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// waitForRunning is -w: the object is read until its phase is one a caller
// can use, or until the wait runs out.
func waitForRunning(ctx context.Context, client *cellaclient.Client, id string, timeout time.Duration) (v1.Sandbox, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		obj, raw, err := client.GetSandbox(ctx, id)
		if err != nil {
			return obj, raw, err
		}
		switch obj.Status.Phase {
		case "Running":
			return obj, raw, nil
		case "Failed":
			return obj, raw, fmt.Errorf("the sandbox is Failed: %s", obj.Status.Reason)
		}
		if time.Now().After(deadline) {
			return obj, raw, fmt.Errorf("the sandbox is %s after %s", obj.Status.Phase, timeout)
		}
		select {
		case <-ctx.Done():
			return obj, raw, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stringList is a flag that may be given more than once, which is what a
// label selector is.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// get reads one object or lists a kind.
func get(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella get <kind> [<ref>] [-o json|wide|name] [-l k=v]... [--phase p] [--owner o] [--environment e] [--limit n]")
	var labels stringList
	output := fs.String("o", outputColumns, "the output form: "+strings.Join(outputs, ", "))
	fs.Var(&labels, "l", "a label selector, repeatable")
	phase := fs.String("phase", "", "only objects in this phase")
	owner := fs.String("owner", "", "only objects of this owner")
	environment := fs.String("environment", "", "only objects in this environment")
	limit := fs.Int("limit", 0, "stop after this many objects")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if c.json {
		*output = outputJSON
	}
	if *output != outputColumns && !slices.Contains(outputs, *output) {
		return usagef("-o takes %s", strings.Join(outputs, ", "))
	}
	if len(rest) == 0 {
		return usagef("get needs a kind: %s", kindList())
	}
	kind, ok := cellaclient.ParseKind(rest[0])
	if !ok {
		return usagef("this server serves no kind %q; it serves %s", rest[0], kindList())
	}
	if len(rest) > 2 {
		return usagef("get takes one kind and at most one reference")
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	if len(rest) == 2 {
		return c.one(ctx, client, kind, rest[1], *output)
	}
	return c.many(ctx, client, kind, *output, cellaclient.ListOptions{
		Labels: labels, Phase: *phase, Owner: *owner, Environment: *environment, Limit: *limit,
	})
}

// one reads a single object.
func (c *invocation) one(ctx context.Context, client *cellaclient.Client, kind cellaclient.Kind, ref, output string) error {
	now := time.Now()
	switch kind {
	case cellaclient.KindSecret:
		obj, raw, err := client.GetSecret(ctx, ref)
		if err != nil {
			return err
		}
		switch output {
		case outputJSON:
			return writeRaw(c.Stdout, raw)
		case outputName:
			return writeNames(c.Stdout, kind, []string{obj.Metadata.Name})
		default:
			return writeSecrets(c.Stdout, []v1.Secret{obj}, output == outputWide, now)
		}
	default:
		obj, raw, err := client.GetSandbox(ctx, ref)
		if err != nil {
			return err
		}
		switch output {
		case outputJSON:
			return writeRaw(c.Stdout, raw)
		case outputName:
			return writeNames(c.Stdout, kind, []string{obj.Metadata.Name})
		default:
			return writeSandboxes(c.Stdout, []v1.Sandbox{obj}, output == outputWide, now)
		}
	}
}

// many lists a kind, following the cursor to the end.
func (c *invocation) many(ctx context.Context, client *cellaclient.Client, kind cellaclient.Kind, output string, o cellaclient.ListOptions) error {
	now := time.Now()
	if kind == cellaclient.KindSecret {
		items, raws, err := client.ListSecrets(ctx, o)
		if err != nil {
			return err
		}
		switch output {
		case outputJSON:
			return writeItems(c.Stdout, raws)
		case outputName:
			return writeNames(c.Stdout, kind, names(items, func(o v1.Secret) string { return o.Metadata.Name }))
		default:
			return writeSecrets(c.Stdout, items, output == outputWide, now)
		}
	}
	items, raws, err := client.ListSandboxes(ctx, o)
	if err != nil {
		return err
	}
	switch output {
	case outputJSON:
		return writeItems(c.Stdout, raws)
	case outputName:
		return writeNames(c.Stdout, kind, names(items, func(o v1.Sandbox) string { return o.Metadata.Name }))
	default:
		return writeSandboxes(c.Stdout, items, output == outputWide, now)
	}
}

// remove deletes one object. Design 008 accepts a delete in every phase, so
// 202 and 200 are both success.
func remove(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella delete <kind> <ref>")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		return usagef("delete needs a kind and a reference")
	}
	kind, ok := cellaclient.ParseKind(rest[0])
	if !ok {
		return usagef("this server serves no kind %q; it serves %s", rest[0], kindList())
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	raw, err := client.Delete(ctx, kind, rest[1])
	if err != nil {
		return err
	}
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	_, err = fmt.Fprintf(c.Stdout, "%s/%s deleted\n", kind, rest[1])
	return err
}

func start(ctx context.Context, c *invocation, args []string) error {
	return act(ctx, c, args, "start", "started")
}

func stop(ctx context.Context, c *invocation, args []string) error {
	return act(ctx, c, args, "stop", "stopped")
}

// act runs one verb of a sandbox.
func act(ctx context.Context, c *invocation, args []string, verb, done string) error {
	fs := c.flags("cella " + verb + " <ref>")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("%s needs one sandbox", verb)
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	obj, raw, err := client.Act(ctx, rest[0], verb)
	if err != nil {
		return err
	}
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	_, err = fmt.Fprintf(c.Stdout, "sandbox/%s %s\n", dash(obj.Metadata.Name), done)
	return err
}

// egressRecords reads what the gateway reported for one sandbox.
func egressRecords(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella egress <ref> [--limit n]")
	limit := fs.Int("limit", 0, "at most this many records")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("egress needs one sandbox")
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	records, raw, err := client.EgressRecords(ctx, rest[0], *limit)
	if err != nil {
		return err
	}
	if c.json {
		return writeRaw(c.Stdout, raw)
	}
	rows := make([][]string, 0, len(records))
	for _, r := range records {
		rows = append(rows, []string{
			r.At.UTC().Format(time.RFC3339), r.Door, fmt.Sprintf("%s:%d", r.Host, r.Port),
			r.Decision, dash(r.Reason), dash(strings.Join(r.Substituted, ",")),
		})
	}
	return table(c.Stdout, []string{"AT", "DOOR", "HOST", "DECISION", "REASON", "SECRETS"}, rows)
}

// version prints the client's identity, and the server's where the address
// answers. A server that does not answer is not a failure: the client's own
// identity is what the command was asked for.
func version(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella version")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	identity := map[string]any{"client": c.Identity}
	server := ""
	if client, err := c.client(); err == nil {
		if build, err := client.ServerVersion(ctx); err == nil {
			server = fmt.Sprintf("%s (%s, %s)", build.Version, build.Commit, build.BuildTime)
			identity["server"] = build
		}
	}
	if c.json {
		return writeValue(c.Stdout, identity)
	}
	if _, err := fmt.Fprintln(c.Stdout, c.Identity); err != nil {
		return err
	}
	if server == "" {
		return nil
	}
	_, err := fmt.Fprintf(c.Stdout, "server %s\n", server)
	return err
}

// names renders one field of every item.
func names[T any](items []T, of func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, of(item))
	}
	return out
}

// kindList names the kinds this server serves, for a usage line.
func kindList() string {
	out := make([]string, 0, len(cellaclient.Kinds))
	for _, k := range cellaclient.Kinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}
