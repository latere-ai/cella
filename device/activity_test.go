// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActivityLogRotates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dev-x")
	a, err := OpenActivity(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if ActivityMax != 10<<20 || ActivityKeep != 5 {
		t.Fatalf("the log rotates at %d bytes keeping %d files; spec 016 says 10 MB and five", ActivityMax, ActivityKeep)
	}
	// A small limit stands in for 10 MB: each line is about 100 bytes, so
	// the log rotates every few lines.
	a.max = 300
	start := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	const lines = 40
	for i := range lines {
		if err = a.Write(Entry{Time: start.Add(time.Duration(i) * time.Second), Type: "Exec", Command: []string{"echo", fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != ActivityKeep+1 {
		var got []string
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Errorf("the log holds %v; want the current file and %d older ones", got, ActivityKeep)
	}
	for _, n := range names {
		info, err := n.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > a.max {
			t.Errorf("%s holds %d bytes, past the limit of %d", n.Name(), info.Size(), a.max)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v, want 0600", n.Name(), info.Mode().Perm())
		}
	}
	entries, err := ReadActivity(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || len(entries) >= lines {
		t.Fatalf("read %d entries; the oldest were not dropped, or nothing was kept", len(entries))
	}
	last := entries[len(entries)-1]
	if last.Command[1] != fmt.Sprint(lines-1) {
		t.Errorf("the newest entry is %v", last.Command)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatalf("entries read out of order at %d", i)
		}
	}
	since, err := ReadActivity(dir, start.Add((lines-3)*time.Second))
	if err != nil || len(since) != 3 {
		t.Errorf("since the last three seconds: %d entries, %v", len(since), err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = a.Write(Entry{}); err == nil {
		t.Error("a closed log was written")
	}
	if err = a.Close(); err != nil {
		t.Errorf("closing twice failed: %v", err)
	}
}

func TestReadActivitySkipsBrokenLines(t *testing.T) {
	dir := t.TempDir()
	content := `{"time":"2026-10-18T09:00:00Z","type":"Exec"}` + "\n\nnot json\n" + `{"time":"2026-10-18T09:00:01Z","type":"Wri`
	if err := os.WriteFile(filepath.Join(dir, "activity.ndjson"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadActivity(dir, time.Time{})
	if err != nil || len(entries) != 1 || entries[0].Type != "Exec" {
		t.Errorf("read %+v, %v", entries, err)
	}
	if entries, err := ReadActivity(filepath.Join(dir, "none"), time.Time{}); err != nil || len(entries) != 0 {
		t.Errorf("a missing log reads %v, %v", entries, err)
	}
}

func TestFollowActivity(t *testing.T) {
	previous := followPoll
	followPoll = 10 * time.Millisecond
	t.Cleanup(func() { followPoll = previous })
	dir := t.TempDir()
	a, err := OpenActivity(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.max = 200
	if err = a.Write(Entry{Time: time.Now(), Type: "Exec", Path: "before"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- FollowActivity(ctx, dir, time.Time{}, func(e Entry) error {
			mu.Lock()
			got = append(got, e.Path)
			mu.Unlock()
			return nil
		})
	}()
	seen := func(n int) func() bool {
		return func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= n }
	}
	waitUntil(t, "the existing line", seen(1))
	// Enough lines to rotate the file under the follower more than once.
	// Each line waits until the follower has the one before it: two
	// rotations between two polls lose the lines in the file between them,
	// which a 200-byte log reaches in milliseconds and the 10 MB one never.
	for i := range 12 {
		if err = a.Write(Entry{Time: time.Now(), Type: "Exec", Path: fmt.Sprintf("after-%d", i)}); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, fmt.Sprintf("line after-%d", i), seen(i+2))
	}
	waitUntil(t, "every line written after", seen(13))
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[0] != "before" || got[12] != "after-11" || len(got) != 13 {
		t.Errorf("followed %v", got)
	}

	stop := errors.New("stop")
	if err := FollowActivity(context.Background(), dir, time.Time{}, func(Entry) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("a failing callback ends the follow with %v", err)
	}
}

// waitUntil polls cond for up to ten seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEntryLine(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 3, 22, 0, time.Local)
	one := 1
	for _, tc := range []struct {
		entry Entry
		want  string
	}{
		{Entry{Time: at, Type: "Exec", Session: "ses_1", Command: []string{"git", "commit", "-m", "a b"}}, `09:03:22  ses_1  exec  git commit -m "a b"`},
		{Entry{Time: at, Type: "Write", Sandbox: "sbx_1", Path: "/h/site/a.go"}, "09:03:22  sbx_1  write  ~/site/a.go"},
		{Entry{Time: at, Type: "Move", Session: "s", Path: "/h/a", To: "/h/b"}, "09:03:22  s  move  ~/a -> ~/b"},
		{Entry{Time: at, Type: "Exec", Session: "s", Command: []string{"false"}, ExitCode: &one}, "09:03:22  s  exec  false  (exit 1)"},
		{Entry{Time: at, Type: "Create", Session: "s", Outcome: "refused", Reason: "no"}, "09:03:22  s  create  (refused: no)"},
		{Entry{Time: at, Type: TypeApproval, Session: "s", Answer: AnswerAllow, By: "this terminal"}, "09:03:22  s  approval  allowed in this terminal"},
		{Entry{Time: at, Type: TypeApproval, Session: "s", Answer: answerRemembered}, "09:03:22  s  approval  allowed without asking: a remembered trigger"},
		{Entry{Time: at, Type: TypeApproval, Answer: answerNoOne}, "09:03:22  -  approval  refused: no one was here to approve"},
		{Entry{Time: at, Type: TypeApproval, Session: "s", Answer: "unanswered"}, "09:03:22  s  approval  unanswered"},
	} {
		if got := tc.entry.Line("/h"); got != tc.want {
			t.Errorf("Line = %q, want %q", got, tc.want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if got, err := ParseSince("", now); err != nil || !got.IsZero() {
		t.Errorf("empty = %v, %v", got, err)
	}
	if got, err := ParseSince("90m", now); err != nil || !got.Equal(now.Add(-90*time.Minute)) {
		t.Errorf("90m = %v, %v", got, err)
	}
	if got, err := ParseSince("2026-10-18T09:00:00Z", now); err != nil || got.Hour() != 9 {
		t.Errorf("a time = %v, %v", got, err)
	}
	for _, bad := range []string{"-1h", "yesterday"} {
		if _, err := ParseSince(bad, now); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q = %v", bad, err)
		}
	}
}
