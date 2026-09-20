// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// listCases prove the list envelope, the cursor, every selector and the
// ceiling on the page size.
func listCases() []Case {
	return []Case{
		{"list", "case008ListEnvelope", case008ListEnvelope},
		{"list", "case008ListSelectors", case008ListSelectors},
		{"list", "case008ListLimitCeiling", case008ListLimitCeiling},
	}
}

// listPage reads one page.
func listPage(ctx context.Context, c *client, query string) (listEnvelope, *exchange, error) {
	var page listEnvelope
	x, err := c.get(ctx, "/v1/sandboxes"+query)
	if err != nil {
		return page, nil, err
	}
	if err := x.status(http.StatusOK); err != nil {
		return page, x, err
	}
	if err := json.Unmarshal(x.Body, &page); err != nil {
		return page, x, x.disagree("a list envelope with items and next", "a body that does not decode: "+err.Error())
	}
	return page, x, nil
}

// case008ListEnvelope: a list answers items and next, a page holds at most
// the limit, and following next reaches every object exactly once.
func case008ListEnvelope(ctx context.Context, e *Env) error {
	var made []string
	for range 3 {
		obj, err := e.create(ctx, e.caller, e.manifest(e.name()))
		if err != nil {
			return err
		}
		made = append(made, obj.Status.ID)
	}
	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		query := "?limit=1"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page, x, err := listPage(ctx, e.caller, query)
		if err != nil {
			return err
		}
		if len(page.Items) > 1 {
			return x.disagree("at most one item under limit=1", fmt.Sprintf("%d items", len(page.Items)))
		}
		for _, item := range page.Items {
			seen[item.Status.ID]++
		}
		pages++
		if page.Next == "" || pages > 200 {
			break
		}
		cursor = page.Next
	}
	for _, id := range made {
		switch seen[id] {
		case 1:
		case 0:
			return fmt.Errorf("paging the list did not reach %s, which this run created", id)
		default:
			return fmt.Errorf("paging the list reached %s %d times", id, seen[id])
		}
	}
	return nil
}

// case008ListSelectors: every selector narrows the list, repeated labels are
// a conjunction, and a selector that matches nothing is an empty list and
// never a refusal.
func case008ListSelectors(ctx context.Context, e *Env) error {
	run := e.run
	first, err := e.create(ctx, e.caller, e.manifest(e.name(), labels(map[string]string{"suite": run, "group": "one"})))
	if err != nil {
		return err
	}
	if _, err = e.create(ctx, e.caller, e.manifest(e.name(), labels(map[string]string{"suite": run, "group": "two"}))); err != nil {
		return err
	}
	page, x, err := listPage(ctx, e.caller, "?limit=200&label=suite="+run+"&label=group=one")
	if err != nil {
		return err
	}
	ids := idsOf(page)
	if !slices.Equal(ids, []string{first.Status.ID}) {
		return x.disagree("the one object both labels match", fmt.Sprintf("%v", ids))
	}
	page, x, err = listPage(ctx, e.caller, "?limit=200&label=suite="+run+"&label=group=none")
	if err != nil {
		return err
	}
	if len(page.Items) != 0 {
		return x.disagree("an empty list for a selector nothing matches", fmt.Sprintf("%d items", len(page.Items)))
	}
	if _, err = e.await(ctx, e.caller, first.Status.ID, "Running"); err != nil {
		return err
	}
	page, x, err = listPage(ctx, e.caller, "?limit=200&label=suite="+run+"&phase=Running")
	if err != nil {
		return err
	}
	if !slices.Contains(idsOf(page), first.Status.ID) {
		return x.disagree("the running object under phase=Running", fmt.Sprintf("%v", idsOf(page)))
	}
	page, x, err = listPage(ctx, e.caller, "?limit=200&label=suite="+run+"&environment="+first.Status.Environment)
	if err != nil {
		return err
	}
	if !slices.Contains(idsOf(page), first.Status.ID) {
		return x.disagree("the object under its own environment", fmt.Sprintf("%v", idsOf(page)))
	}
	return nil
}

// case008ListLimitCeiling: a page size above the ceiling is a refusal at the
// field and never a silently smaller page.
func case008ListLimitCeiling(ctx context.Context, e *Env) error {
	x, err := e.caller.get(ctx, "/v1/sandboxes?limit=300")
	if err != nil {
		return err
	}
	return x.refusal("invalid_field")
}

// labels sets the metadata labels of a manifest.
func labels(set map[string]string) func(map[string]any) {
	return func(body map[string]any) {
		metadata, _ := body["metadata"].(map[string]any)
		metadata["labels"] = set
	}
}

// idsOf is the ids of one page, in the order the page holds them.
func idsOf(page listEnvelope) []string {
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.Status.ID)
	}
	return out
}
