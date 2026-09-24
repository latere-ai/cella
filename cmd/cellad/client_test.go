// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestTheExportedClientDrivesARunningNode is the exported client against a
// running node, the way a program outside this module uses it: it applies a
// YAML manifest under a name, finds the sandbox by label, runs a command and
// a session, writes and reads a file, follows the sandbox's records from the
// newest one it read, deletes the sandbox and reads the feed to its end, and
// mints and revokes an environment key.
func TestTheExportedClientDrivesARunningNode(t *testing.T) {
	issuer := issuertest.New(t)
	base, _, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":   issuer.URL(),
		"CELLA_ADMIN_SUBJECTS": issuer.URL() + "|alice",
	})
	defer stop()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"cella"}})
	c, err := client.New(client.Config{URL: base, Token: client.StaticToken(alice), UserAgent: "client-e2e/1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	manifest := "apiVersion: " + v1.APIVersion + "\nkind: Sandbox\nmetadata:\n  labels:\n    team: core\nspec: {}\n"
	obj, _, err := c.ApplySandbox(ctx, "driven", client.YAML([]byte(manifest)))
	if err != nil {
		t.Fatalf("applying the YAML manifest: %v", err)
	}
	if obj.Metadata.Name != "driven" || obj.Status.Phase != "Running" || obj.Metadata.Labels["team"] != "core" {
		t.Fatalf("the apply answered %s in %s with %v", obj.Metadata.Name, obj.Status.Phase, obj.Metadata.Labels)
	}
	listed, _, err := c.ListSandboxes(ctx, client.ListOptions{Labels: []string{"team=core"}})
	if err != nil || len(listed) != 1 || listed[0].Status.ID != obj.Status.ID {
		t.Fatalf("the label selected %d sandboxes, %v", len(listed), err)
	}

	page, _, err := c.Events(ctx, obj.Status.ID, client.EventOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("the history holds %d records, %v", len(page.Items), err)
	}
	feed, err := c.FollowEvents(ctx, client.FollowOptions{Object: obj.Status.ID, Cursor: client.After(page.Items[0].Seq)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = feed.Close() }()

	result, _, err := c.Exec(ctx, "driven", client.ExecRequest{Command: []string{"sh", "-c", "echo from inside"}})
	if err != nil || result.ExitCode != 0 || result.Stdout != "from inside\n" {
		t.Fatalf("the exec answered %+v, %v", result, err)
	}
	session, err := c.ExecSession(ctx, obj.Status.ID, client.ExecRequest{Command: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := session.Wait(); err != nil || code != 3 {
		t.Fatalf("the session ended %d, %v", code, err)
	}
	_ = session.Close()
	if err = c.FilePut(ctx, obj.Status.ID, "/workspace/notes.txt", "", strings.NewReader("written by the client")); err != nil {
		t.Fatal(err)
	}
	body, err := c.FileGet(ctx, obj.Status.ID, "/workspace/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(read) != "written by the client" {
		t.Fatalf("the file read back as %q, %v", read, err)
	}

	if _, err = c.Delete(ctx, client.KindSandbox, "driven"); err != nil {
		t.Fatal(err)
	}
	var types []string
	for {
		event, err := feed.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the feed failed after %v: %v", types, err)
		}
		if event.Object.ID != obj.Status.ID || event.Seq <= page.Items[0].Seq {
			t.Fatalf("the feed of %s from %d carried %s", obj.Status.ID, page.Items[0].Seq, event.Raw)
		}
		types = append(types, event.Type)
	}
	if !slices.Contains(types, "sandbox.exec") || types[len(types)-1] != "sandbox.deleted" {
		t.Fatalf("the feed carried %v, want the exec and then the delete last", types)
	}
	if _, _, err = c.GetSandbox(ctx, obj.Status.ID); client.CodeOf(err) != "not_found" {
		t.Fatalf("the deleted sandbox read as %v", err)
	}

	environment, _, err := c.GetEnvironment(ctx, "default")
	if err != nil || environment.Status.Phase != v1.EnvironmentReady {
		t.Fatalf("the default environment read as %s, %v", environment.Status.Phase, err)
	}
	key, _, err := c.MintEnvironmentKey(ctx, "default")
	if err != nil || key.Token == "" || key.JTI == "" {
		t.Fatalf("the mint answered %+v, %v", key, err)
	}
	if err = c.RevokeEnvironmentKey(ctx, "default", key.JTI); err != nil {
		t.Fatalf("revoking the key: %v", err)
	}
}
