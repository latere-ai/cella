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

// Kind is a kind this API serves. The command takes it singular or plural
// and the routes are the plural.
type Kind string

const (
	KindSandbox Kind = "sandbox"
	KindSecret  Kind = "secret"
)

// Kinds is every kind, in the order a listing shows them.
var Kinds = []Kind{KindSandbox, KindSecret}

// plurals is each kind's collection, which is the segment of its routes.
var plurals = map[Kind]string{KindSandbox: "sandboxes", KindSecret: "secrets"}

// ParseKind reads a kind written singular or plural. The second result says
// whether it is one.
func ParseKind(s string) (Kind, bool) {
	for _, k := range Kinds {
		if strings.EqualFold(s, string(k)) || strings.EqualFold(s, plurals[k]) {
			return k, true
		}
	}
	return "", false
}

// Plural is the collection name of a kind.
func (k Kind) Plural() string { return plurals[k] }

// Path is the collection route of a kind.
func (k Kind) Path() string { return "/v1/" + plurals[k] }

// ManifestKind is the kind a manifest of this kind declares.
func (k Kind) ManifestKind() string {
	return strings.ToUpper(string(k)[:1]) + string(k)[1:]
}

// Page is one page of a list as the API answers it: the items' own bytes and
// the cursor to the next page. The items stay raw so an output that promises
// the API's bytes can keep them.
type Page struct {
	Items []json.RawMessage `json:"items"`
	Next  string            `json:"next"`
}

// ListOptions are the selectors of design 008. Limit is the total a caller
// wants across pages, not the page size; zero means every object.
type ListOptions struct {
	Labels      []string
	Phase       string
	Owner       string
	Environment string
	Limit       int
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

// CreateSandbox applies a Sandbox manifest. The body is the caller's own
// bytes: the command reads the kind from the document and sends the rest
// unchanged, so what the server refuses is what the caller wrote.
func (c *Client) CreateSandbox(ctx context.Context, body []byte) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.Path(), nil, body, "application/json")
	return decodeInto[v1.Sandbox](raw, err)
}

// ApplySecret applies a Secret manifest by name: a create when the name is
// free and an update when the caller holds it.
func (c *Client) ApplySecret(ctx context.Context, name string, body []byte) (v1.Secret, []byte, error) {
	raw, err := c.send(ctx, http.MethodPut, KindSecret.Path()+"/"+url.PathEscape(name), nil, body, "application/json")
	return decodeInto[v1.Secret](raw, err)
}

// GetSandbox reads one Sandbox by id or by name.
func (c *Client) GetSandbox(ctx context.Context, ref string) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindSandbox.Path()+"/"+url.PathEscape(ref), nil, nil, "")
	return decodeInto[v1.Sandbox](raw, err)
}

// GetObjectAs reads one object in the syntax the caller names: the answer's
// own bytes, undecoded. Design 008 renders one object as YAML where the
// request names one of the three YAML types, so the syntax is the server's to
// produce and this client's to pass through.
func (c *Client) GetObjectAs(ctx context.Context, kind Kind, ref, accept string) ([]byte, error) {
	return c.accepting(ctx, kind.Path()+"/"+url.PathEscape(ref), nil, accept)
}

// ListAs reads one page of a kind in the syntax the caller names, with the
// selectors of design 008. It is one page and not the whole list: the pages
// are concatenated by re-encoding an envelope, and this command holds no
// encoder for a syntax the server rendered.
func (c *Client) ListAs(ctx context.Context, kind Kind, o ListOptions, accept string) ([]byte, error) {
	return c.accepting(ctx, kind.Path(), o.query("", o.Limit), accept)
}

// GetSecret reads one Secret. No response carries its value.
func (c *Client) GetSecret(ctx context.Context, ref string) (v1.Secret, []byte, error) {
	raw, err := c.send(ctx, http.MethodGet, KindSecret.Path()+"/"+url.PathEscape(ref), nil, nil, "")
	return decodeInto[v1.Secret](raw, err)
}

// Delete removes one object of a kind. The body is the object as the API
// answered, which a delete carries in every phase.
func (c *Client) Delete(ctx context.Context, kind Kind, ref string) ([]byte, error) {
	return c.send(ctx, http.MethodDelete, kind.Path()+"/"+url.PathEscape(ref), nil, nil, "")
}

// Act runs one verb of a sandbox: start or stop.
func (c *Client) Act(ctx context.Context, ref, verb string) (v1.Sandbox, []byte, error) {
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.Path()+"/"+url.PathEscape(ref)+"/"+verb, nil, nil, "")
	return decodeInto[v1.Sandbox](raw, err)
}

// ListSandboxes follows the cursor to the end, or to Limit objects. It
// returns the decoded objects and the same objects' own bytes, in order, so
// a caller that prints columns and a caller that prints the API's JSON read
// one call.
func (c *Client) ListSandboxes(ctx context.Context, o ListOptions) ([]v1.Sandbox, []json.RawMessage, error) {
	return listAll[v1.Sandbox](ctx, c, KindSandbox, o)
}

// ListSecrets is the same over the Secret kind.
func (c *Client) ListSecrets(ctx context.Context, o ListOptions) ([]v1.Secret, []json.RawMessage, error) {
	return listAll[v1.Secret](ctx, c, KindSecret, o)
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
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
}

// ExecResult is the synchronous route's answer: design 008 caps each output
// at 1 MiB, keeps the head, and reports 124 for its own timeout.
type ExecResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"durationMs"`
}

// Exec runs a command and waits for it. This is the ?wait=1 answer of
// design 008; a command with input or a terminal is ExecSession.
func (c *Client) Exec(ctx context.Context, ref string, req ExecRequest) (ExecResult, []byte, error) {
	body, err := json.Marshal(execBody{Command: req.Command, Env: req.Env, Workdir: req.Workdir, Timeout: req.Timeout})
	if err != nil {
		return ExecResult{}, nil, err
	}
	q := url.Values{"wait": []string{"1"}}
	raw, err := c.send(ctx, http.MethodPost, KindSandbox.Path()+"/"+url.PathEscape(ref)+"/exec", q, body, "application/json")
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
	Follow bool
	Since  time.Time
	Tail   int
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
	body, _, err := c.stream(ctx, http.MethodGet, KindSandbox.Path()+"/"+url.PathEscape(ref)+"/logs", q, nil, "")
	return body, err
}

// EgressRecord is one connection the gateway reported, as the route
// answers it (design 018). The boundary package's own type is not imported
// here: the build list of this command is the standard library, this
// module's contract types and the error envelope, and a record is a row to
// read rather than a boundary to compile.
type EgressRecord struct {
	Principal string    `json:"principal"`
	At        time.Time `json:"at"`
	Door      string    `json:"door"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	// Substituted names the secrets whose values replaced a placeholder on
	// this connection. Names, never values.
	Substituted []string `json:"substituted,omitempty"`
	Method      string   `json:"method,omitempty"`
	Path        string   `json:"path,omitempty"`
	Status      int      `json:"status,omitempty"`
	BytesOut    int64    `json:"bytesOut,omitempty"`
	BytesIn     int64    `json:"bytesIn,omitempty"`
	DurationMS  int64    `json:"durationMs,omitempty"`
}

// EgressRecords reads what the gateway reported for one sandbox, newest
// first.
func (c *Client) EgressRecords(ctx context.Context, ref string, limit int) ([]EgressRecord, []byte, error) {
	q := url.Values{"limit": []string{limitValue(limit)}}
	raw, err := c.send(ctx, http.MethodGet, KindSandbox.Path()+"/"+url.PathEscape(ref)+"/egress", q, nil, "")
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
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

// ServerVersion asks the control plane who it is. It carries no bearer,
// because the route carries none.
func (c *Client) ServerVersion(ctx context.Context) (Build, error) {
	target := *c.base
	target.Path = c.base.Path + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Build{}, err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("X-Request-Id", RequestID())
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
