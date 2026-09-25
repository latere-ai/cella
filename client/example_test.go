// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"latere.ai/x/cella/client"
)

// A sandbox provider applies a manifest under a name, holding the answer
// until the sandbox runs, runs a command in it and waits for it, and deletes
// the sandbox. The address and the token are the caller's own.
func ExampleNew() {
	ctx := context.Background()
	c, err := client.New(client.Config{
		URL:       "https://cella.example.com",
		Token:     client.StaticToken(os.Getenv("PROVIDER_TOKEN")),
		UserAgent: "provider/1.0",
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	manifest := client.YAML([]byte(`apiVersion: cella.latere.ai/v1beta1
kind: Sandbox
spec:
  image: ghcr.io/example/agent:1
`))
	// A create answers once the sandbox is recorded, before it runs; the
	// hold makes the answer the running sandbox, so the command below has a
	// workload to run in.
	sandbox, _, err := c.ApplySandbox(ctx, "agent-7", manifest, client.Wait(2*time.Minute))
	if err != nil {
		var refusal *client.Error
		if errors.As(err, &refusal) {
			fmt.Printf("%s (%s, request %s)\n", refusal.Message, refusal.Code, refusal.RequestID)
			return
		}
		fmt.Println(err)
		return
	}
	result, _, err := c.Exec(ctx, sandbox.Status.ID, client.ExecRequest{Command: []string{"uname", "-a"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Print(result.Stdout)
	if _, err = c.Delete(ctx, client.KindSandbox, sandbox.Status.ID); err != nil {
		fmt.Println(err)
	}
}

// Code running inside a sandbox reaches the control plane that runs it with
// nothing configured: the address and the projected token are in the
// environment the driver gave it.
func ExampleEnvironment() {
	c, err := client.New(client.Environment(os.Getenv))
	if err != nil {
		fmt.Println(err)
		return
	}
	sandboxes, _, err := c.ListSandboxes(context.Background(), client.ListOptions{Labels: []string{"team=core"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, s := range sandboxes {
		fmt.Println(s.Metadata.Name, s.Status.Phase)
	}
}

// Waiting for a sandbox to start reads the newest record of its history and
// follows from there, so a start that happens between the read and the
// follow is in the feed.
func ExampleClient_FollowEvents() {
	ctx := context.Background()
	c, err := client.New(client.Config{URL: "https://cella.example.com", Token: client.TokenFile("/var/run/provider/token")})
	if err != nil {
		fmt.Println(err)
		return
	}
	page, _, err := c.Events(ctx, "agent-7", client.EventOptions{Limit: 1})
	if err != nil {
		fmt.Println(err)
		return
	}
	var cursor string
	if len(page.Items) > 0 {
		cursor = client.After(page.Items[0].Seq)
	}
	feed, err := c.FollowEvents(ctx, client.FollowOptions{Object: "agent-7", Cursor: cursor})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = feed.Close() }()
	for {
		event, err := feed.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			fmt.Println(err)
			return
		}
		if event.Type == "sandbox.started" || event.Type == "sandbox.failed" {
			fmt.Println(event.Type, event.Reason)
			return
		}
	}
}
