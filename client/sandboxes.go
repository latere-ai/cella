// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// Kind is a kind this API serves, which names its collection route.
type Kind string

// The kinds of /v1.
const (
	KindSandbox     Kind = "sandbox"
	KindSecret      Kind = "secret"
	KindEnvironment Kind = "environment"
)

// plurals is each kind's collection, which is the segment of its routes.
var plurals = map[Kind]string{KindSandbox: "sandboxes", KindSecret: "secrets", KindEnvironment: "environments"}

// Plural is the collection name of a kind.
func (k Kind) Plural() string { return plurals[k] }

// Path is the collection route of a kind.
func (k Kind) Path() string { return "/v1/" + plurals[k] }

// ManifestKind is the kind a manifest of this kind declares.
func (k Kind) ManifestKind() string {
	if k == "" {
		return ""
	}
	return strings.ToUpper(string(k)[:1]) + string(k)[1:]
}

// item is the route of one object of a kind.
func (k Kind) item(ref string) string { return k.Path() + "/" + url.PathEscape(ref) }

// Page is one page of a list as the API answers it: the items' own bytes and
// the cursor to the next page. The items stay raw so an output that promises
// the API's bytes can keep them.
type Page struct {
	// Items are the page's objects, each its own bytes.
	Items []json.RawMessage `json:"items"`
	// Next is the cursor of the following page, empty on the last.
	Next string `json:"next"`
}

// ListOptions are the selectors of design 008. Limit is the total a caller
// wants across pages, not the page size; zero means every object.
type ListOptions struct {
	// Labels are selectors of the form key=value; an object matches when
	// it carries every one.
	Labels []string
	// Phase keeps the objects in that phase.
	Phase string
	// Owner keeps the objects of that rendered subject.
	Owner string
	// Environment keeps the sandboxes placed on that environment.
	Environment string
	// Limit is the most objects the call returns across every page; zero
	// is all of them.
	Limit int
}

// query renders the selectors, with the page size the API accepts.
func (o ListOptions) query(cursor string, page int) url.Values {
	q := url.Values{}
	for _, l := range o.Labels {
		q.Add("label", l)
	}
	for key, value := range map[string]string{
		"phase": o.Phase, "owner": o.Owner, "environment": o.Environment, "cursor": cursor,
	} {
		if value != "" {
			q.Set(key, value)
		}
	}
	q.Set("limit", limitValue(page))
	return q
}

// CreateSandbox creates a Sandbox from a manifest, which names it or leaves
// the server to. The body is the caller's own bytes in the syntax the
// manifest carries, so what the server refuses is what the caller wrote.
func (c *Client) CreateSandbox(ctx context.Context, m Manifest) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.Path(), nil, m.Body, m.mediaType())
	return decodeInto[v1.Sandbox](raw, err)
}

// ApplySandbox applies a Sandbox manifest under a name: a create when the
// name is free and an update when the caller holds it. A manifest that names
// no sandbox takes the name; one that names another is refused.
func (c *Client) ApplySandbox(ctx context.Context, name string, m Manifest) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodPut, KindSandbox.item(name), nil, m.Body, m.mediaType())
	return decodeInto[v1.Sandbox](raw, err)
}

// GetSandbox reads one Sandbox by id or by name.
func (c *Client) GetSandbox(ctx context.Context, ref string) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindSandbox.item(ref), nil, nil, "")
	return decodeInto[v1.Sandbox](raw, err)
}

// GetAs reads one object in the syntax the caller names: the answer's own
// bytes, undecoded. Design 008 renders one object as YAML where the request
// names one of the three YAML types, so the syntax is the server's to produce
// and this client's to pass through.
func (c *Client) GetAs(ctx context.Context, kind Kind, ref, accept string) ([]byte, error) {
	return c.accepting(ctx, kind.item(ref), nil, accept)
}

// ListAs reads one page of a kind in the syntax the caller names, with the
// selectors of design 008. It is one page and not the whole list: pages are
// joined by re-encoding an envelope, and this client holds no encoder for a
// syntax the server rendered.
func (c *Client) ListAs(ctx context.Context, kind Kind, o ListOptions, accept string) ([]byte, error) {
	return c.accepting(ctx, kind.Path(), o.query("", o.Limit), accept)
}

// Delete removes one object of a kind. The body is the object as the API
// answered, which a sandbox's delete carries in every phase.
func (c *Client) Delete(ctx context.Context, kind Kind, ref string) ([]byte, error) {
	return c.send(ctx, http.MethodDelete, kind.item(ref), nil, nil, "")
}

// StartSandbox starts a stopped sandbox and answers it as it stands after.
func (c *Client) StartSandbox(ctx context.Context, ref string) (v1.Sandbox, []byte, error) {
	return c.act(ctx, ref, "start")
}

// StopSandbox stops a running sandbox and answers it as it stands after.
func (c *Client) StopSandbox(ctx context.Context, ref string) (v1.Sandbox, []byte, error) {
	return c.act(ctx, ref, "stop")
}

// act runs one verb of a sandbox.
func (c *Client) act(ctx context.Context, ref, verb string) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.item(ref)+"/"+verb, nil, nil, "")
	return decodeInto[v1.Sandbox](raw, err)
}

// ListSandboxes follows the cursor to the end, or to Limit objects. It
// returns the decoded objects and the same objects' own bytes, in order, so
// a caller that prints columns and a caller that prints the API's JSON read
// one call.
func (c *Client) ListSandboxes(ctx context.Context, o ListOptions) ([]v1.Sandbox, []json.RawMessage, error) {
	return listAll[v1.Sandbox](ctx, c, KindSandbox, o)
}

// listAll pages a collection. A page is asked for at the API's ceiling or
// at what is left of the caller's limit, whichever is smaller, so a limit
// above the ceiling pages instead of being refused.
func listAll[T any](ctx context.Context, c *Client, kind Kind, o ListOptions) ([]T, []json.RawMessage, error) {
	var items []T
	var raws []json.RawMessage
	cursor := ""
	for {
		page := maxPage
		if o.Limit > 0 {
			page = min(o.Limit-len(items), maxPage)
		}
		body, err := c.send(ctx, http.MethodGet, kind.Path(), o.query(cursor, page), nil, "")
		if err != nil {
			return nil, nil, err
		}
		var answer Page
		if err = json.Unmarshal(body, &answer); err != nil {
			return nil, nil, fmt.Errorf("the list answer is no page: %w", err)
		}
		for _, raw := range answer.Items {
			var item T
			if err = json.Unmarshal(raw, &item); err != nil {
				return nil, nil, fmt.Errorf("the list answer holds no %s: %w", kind, err)
			}
			items = append(items, item)
			raws = append(raws, raw)
			if o.Limit > 0 && len(items) == o.Limit {
				return items, raws, nil
			}
		}
		if answer.Next == "" || len(answer.Items) == 0 {
			return items, raws, nil
		}
		cursor = answer.Next
	}
}

// ExecRequest is one command to run. Cols and Rows are set for a terminal
// and Stdin for a command whose input the caller writes; either opens the
// socket rather than the synchronous route.
type ExecRequest struct {
	// Command is the program and its arguments. An attach with none runs
	// the sandbox's shell.
	Command []string `json:"command,omitempty"`
	// Env is added to the sandbox's environment for this command.
	Env map[string]string `json:"env,omitempty"`
	// Workdir is where the command starts, the sandbox's workdir when
	// empty.
	Workdir string `json:"workdir,omitempty"`
	// Timeout is a duration such as 30s after which the server ends the
	// command and reports exit 124; empty is the server's bound.
	Timeout string `json:"timeout,omitempty"`
	// Cols and Rows are the terminal's window; set on a session, they ask
	// for a terminal.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

// ExecResult is the synchronous route's answer: design 008 caps each output
// at 1 MiB, keeps the head, and reports 124 for its own timeout.
type ExecResult struct {
	// ExitCode is the command's, or 124 where the server's timeout ended it.
	ExitCode int `json:"exitCode"`
	// Stdout and Stderr are the command's two outputs, each at most 1 MiB.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Truncated says an output was longer than its cap and lost its tail.
	Truncated bool `json:"truncated"`
	// DurationMS is how long the command ran, in milliseconds.
	DurationMS int64 `json:"durationMs"`
}

// Exec runs a command and waits for it. This is the ?wait=1 answer of
// design 008; a command with input or a terminal is ExecSession.
func (c *Client) Exec(ctx context.Context, ref string, req ExecRequest) (ExecResult, []byte, error) {
	body, err := json.Marshal(execBody{Command: req.Command, Env: req.Env, Workdir: req.Workdir, Timeout: req.Timeout})
	if err != nil {
		return ExecResult{}, nil, err
	}
	q := url.Values{"wait": []string{"1"}}
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.item(ref)+"/exec", q, body, "application/json")
	return decodeInto[ExecResult](raw, err)
}

// execBody is the synchronous route's body, which takes no window: the
// route refuses a field it does not know.
type execBody struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// LogOptions are the selectors of the log route.
type LogOptions struct {
	// Follow keeps the stream open and writes new output as it arrives.
	Follow bool
	// Since leaves out what was written before it; zero is everything.
	Since time.Time
	// Tail starts that many lines from the end; zero is from the start.
	Tail int
}

// Logs opens the sandbox's main process output. The caller closes it.
func (c *Client) Logs(ctx context.Context, ref string, o LogOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if o.Follow {
		q.Set("follow", "1")
	}
	if !o.Since.IsZero() {
		q.Set("since", o.Since.UTC().Format(time.RFC3339))
	}
	if o.Tail > 0 {
		q.Set("tail", strconv.Itoa(o.Tail))
	}
	body, _, err := c.stream(ctx, http.MethodGet, KindSandbox.item(ref)+"/logs", q, nil, "")
	return body, err
}

// EgressRecord is one connection the gateway reported, as the route
// answers it (design 018). The boundary package's own type is not imported
// here: the build list of this command is the standard library, this
// module's contract types and the error envelope, and a record is a row to
// read rather than a boundary to compile.
type EgressRecord struct {
	// Principal is the sandbox the connection was made for.
	Principal string `json:"principal"`
	// At is when the gateway decided.
	At time.Time `json:"at"`
	// Door is the gateway door the connection came through.
	Door string `json:"door"`
	// Host and Port are where the connection was going.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Decision is what the boundary decided, and Reason which rule decided
	// a connection that was not allowed.
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	// Substituted names the secrets whose values replaced a placeholder on
	// this connection. Names, never values.
	Substituted []string `json:"substituted,omitempty"`
	// Method, Path and Status are present only where the gateway saw the
	// request; the path carries no query string.
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Status int    `json:"status,omitempty"`
	// BytesOut and BytesIn are what the connection carried each way, and
	// DurationMS how long it was open, in milliseconds.
	BytesOut   int64 `json:"bytesOut,omitempty"`
	BytesIn    int64 `json:"bytesIn,omitempty"`
	DurationMS int64 `json:"durationMs,omitempty"`
}

// EgressRecords reads what the gateway reported for one sandbox, newest
// first.
func (c *Client) EgressRecords(ctx context.Context, ref string, limit int) ([]EgressRecord, []byte, error) {
	q := url.Values{"limit": []string{limitValue(limit)}}
	raw, err := c.send(ctx, http.MethodGet, KindSandbox.item(ref)+"/egress", q, nil, "")
	if err != nil {
		return nil, nil, err
	}
	var answer struct {
		Items []EgressRecord `json:"items"`
	}
	if err = json.Unmarshal(raw, &answer); err != nil {
		return nil, nil, fmt.Errorf("the egress answer is no record list: %w", err)
	}
	return answer.Items, raw, nil
}

// Build is the identity the server reports at /version, which needs no
// bearer.
type Build struct {
	// Version is the release, such as v1.2.3.
	Version string `json:"version"`
	// Commit is the source revision the server was built from.
	Commit string `json:"commit"`
	// BuildTime is when it was built.
	BuildTime string `json:"buildTime"`
}

// ServerVersion asks the control plane who it is. It carries no bearer,
// because the route carries none.
func (c *Client) ServerVersion(ctx context.Context) (Build, error) {
	target := *c.base
	target.Path = Route(c.base.Path, "/version")
	target.RawPath = Route(c.base.EscapedPath(), "/version")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Build{}, err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("X-Request-Id", requestID())
	resp, err := c.do(req)
	if err != nil {
		return Build{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var build Build
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if err != nil {
		return build, err
	}
	if err = json.Unmarshal(body, &build); err != nil {
		return build, fmt.Errorf("the version answer is no build identity: %w", err)
	}
	return build, nil
}

// decodeInto is the common tail of every call whose answer is one object:
// the decoded object and the response's own bytes, so an output that
// promises the API's bytes has them.
func decodeInto[T any](raw []byte, err error) (T, []byte, error) {
	var out T
	if err != nil {
		return out, nil, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, raw, fmt.Errorf("the answer is not the object this route returns: %w", err)
	}
	return out, raw, nil
}
