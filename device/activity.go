// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The activity log: activity.ndjson in the environment's state directory,
// one line per operation the worker answered, and per approval. It carries
// no file content and no output. At ActivityMax bytes it rotates, keeping
// ActivityKeep older files beside the current one.
const (
	activityName = "activity.ndjson"
	ActivityMax  = 10 << 20
	ActivityKeep = 5
)

// followPoll is how often a follower looks for new lines.
var followPoll = 250 * time.Millisecond

// SessionLabels are the keys under which the plane that starts sessions on
// this computer names them on a sandbox: the session, its agent and the
// agent's version, the trigger that started it and what the trigger fires
// on, and the annotation that carries the agent's display name. The engine
// shows them in an approval and records them in the activity log, and
// remembers a scheduled trigger by them. The keys are the plane's, so the
// embedder names them; an empty key reads nothing.
type SessionLabels struct {
	Session, Agent, AgentVersion, Trigger, TriggerFiresOn string
	// AgentName is an annotation key, not a label.
	AgentName string
}

// labelOf is the value of key in m, "" for an empty key.
func labelOf(m map[string]string, key string) string {
	if key == "" {
		return ""
	}
	return m[key]
}

// The types of an activity line that records something other than an
// operation: an approval; a connection a command was refused, with its host
// and port; and a host the person allowed, for one session or saved to the
// host policy for every session.
const (
	TypeApproval       = "approval"
	TypeNetworkRefused = "network_refused"
	TypeHostAllowed    = "host_allowed"
	TypeHostSaved      = "host_saved"
)

// Entry is one line of the activity log.
type Entry struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Sandbox string    `json:"sandbox,omitempty"`
	Session string    `json:"session,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Trigger string    `json:"trigger,omitempty"`
	// Command is an exec's command, Path a file operation's path and To a
	// move's destination.
	Command []string `json:"command,omitempty"`
	Path    string   `json:"path,omitempty"`
	To      string   `json:"to,omitempty"`
	// Host and Port are a refused connection's, or an allowed host's.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Outcome is ok, refused or failed, and Reason why.
	Outcome  string `json:"outcome,omitempty"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// Answer and By are an approval's: allow, deny or remember and the
	// client that gave it, or, with no one asked, remembered,
	// session_start_allow or no_one_here.
	Answer string `json:"answer,omitempty"`
	By     string `json:"by,omitempty"`
}

// Line renders an entry as the terminal shows it: the time, the session,
// and the command or path, with its outcome when it was not ok.
func (e Entry) Line(home string) string {
	who := e.Session
	if who == "" {
		who = e.Sandbox
	}
	if who == "" {
		who = "-"
	}
	parts := []string{e.Time.Local().Format("15:04:05"), who, strings.ToLower(e.Type)}
	switch {
	case e.Type == TypeApproval:
		parts = append(parts, approvalWords(e.Answer, e.By))
	case len(e.Command) > 0:
		parts = append(parts, quoteCommand(e.Command))
	case e.Path != "" && e.To != "":
		parts = append(parts, Shorten(home, e.Path)+" -> "+Shorten(home, e.To))
	case e.Path != "":
		parts = append(parts, Shorten(home, e.Path))
	case e.Host != "":
		parts = append(parts, net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
	}
	line := strings.Join(parts, "  ")
	switch {
	case e.Outcome == "refused" || e.Outcome == "failed":
		line += "  (" + e.Outcome + ": " + e.Reason + ")"
	case e.ExitCode != nil && *e.ExitCode != 0:
		line += "  (exit " + strconv.Itoa(*e.ExitCode) + ")"
	}
	return line
}

// approvalWords is an approval's answer as the terminal shows it.
func approvalWords(answer, by string) string {
	words := map[string]string{
		AnswerAllow:        "allowed",
		AnswerDeny:         "refused",
		AnswerRemember:     "allowed and remembered for 30 days",
		answerRemembered:   "allowed without asking: a remembered trigger",
		answerSessionStart: "allowed without asking: session_start is allow",
		answerNoOne:        "refused: no one was here to approve",
	}[answer]
	if words == "" {
		words = answer
	}
	if by != "" && answer != answerRemembered && answer != answerSessionStart && answer != answerNoOne {
		words += " in " + by
	}
	return words
}

// quoteCommand joins a command's words, quoting a word a shell would split.
func quoteCommand(command []string) string {
	out := make([]string, len(command))
	for i, w := range command {
		if w == "" || strings.ContainsAny(w, " \t\n\"'\\$`|&;<>()*?[]#~") {
			w = strconv.Quote(w)
		}
		out[i] = w
	}
	return strings.Join(out, " ")
}

// Activity writes one environment's activity log.
type Activity struct {
	dir  string
	max  int64
	keep int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenActivity opens the log in dir, mode 0600 in a directory of mode 0700,
// appending to what is there.
func OpenActivity(dir string) (*Activity, error) {
	a := &Activity{dir: dir, max: ActivityMax, keep: ActivityKeep}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := a.open(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Activity) open() error {
	f, err := os.OpenFile(filepath.Join(a.dir, activityName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	a.f, a.size = f, info.Size()
	return nil
}

// Write appends one entry, rotating first when the line would carry the file
// past the limit.
func (a *Activity) Write(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return errors.New("the activity log is closed")
	}
	if a.size > 0 && a.size+int64(len(line)) > a.max {
		if err = a.rotate(); err != nil {
			return err
		}
	}
	n, err := a.f.Write(line)
	a.size += int64(n)
	return err
}

// rotate moves activity.ndjson to activity.1.ndjson, each older file one
// place along, and drops the oldest past keep.
func (a *Activity) rotate() error {
	if err := a.f.Close(); err != nil {
		return err
	}
	a.f = nil
	if err := os.Remove(rotated(a.dir, a.keep)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := a.keep - 1; i >= 1; i-- {
		if err := os.Rename(rotated(a.dir, i), rotated(a.dir, i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(filepath.Join(a.dir, activityName), rotated(a.dir, 1)); err != nil {
		return err
	}
	return a.open()
}

// Close closes the log.
func (a *Activity) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// rotated is the path of the i-th older file.
func rotated(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("activity.%d.ndjson", i))
}

// activityFiles are the log's files, oldest first.
func activityFiles(dir string) []string {
	var out []string
	for i := ActivityKeep; i >= 1; i-- {
		out = append(out, rotated(dir, i))
	}
	return append(out, filepath.Join(dir, activityName))
}

// ReadActivity reads every kept file of the log in dir, oldest first, and
// answers the entries at or after since. A line that does not decode, such
// as one cut short by a crash, is skipped.
func ReadActivity(dir string, since time.Time) ([]Entry, error) {
	var out []Entry
	for _, path := range activityFiles(dir) {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for line := range bytes.Lines(raw) {
			if e, ok := decodeEntry(line); ok && !e.Time.Before(since) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

func decodeEntry(line []byte) (Entry, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Entry{}, false
	}
	var e Entry
	if err := json.Unmarshal(line, &e); err != nil {
		return Entry{}, false
	}
	return e, true
}

// FollowActivity calls fn with every entry at or after since, then with each
// one written after, until ctx ends or fn fails. It follows the current file
// across a rotation.
func FollowActivity(ctx context.Context, dir string, since time.Time, fn func(Entry) error) error {
	entries, err := ReadActivity(dir, since)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err = fn(e); err != nil {
			return err
		}
	}
	path := filepath.Join(dir, activityName)
	var (
		f       *os.File
		reader  *bufio.Reader
		partial []byte
	)
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	// The lines already read are skipped by starting at the file's end.
	reopen := func(atEnd bool) error {
		if f != nil {
			_ = f.Close()
			f = nil
		}
		next, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if atEnd {
			if _, err = next.Seek(0, io.SeekEnd); err != nil {
				_ = next.Close()
				return err
			}
		}
		f, reader, partial = next, bufio.NewReader(next), nil
		return nil
	}
	if err = reopen(true); err != nil {
		return err
	}
	// drain hands fn every whole line the open file holds past the reader.
	drain := func() error {
		for {
			chunk, rerr := reader.ReadBytes('\n')
			partial = append(partial, chunk...)
			if rerr != nil {
				if !errors.Is(rerr, io.EOF) {
					return rerr
				}
				return nil
			}
			if e, ok := decodeEntry(partial); ok {
				if err := fn(e); err != nil {
					return err
				}
			}
			partial = nil
		}
	}
	ticker := time.NewTicker(followPoll)
	defer ticker.Stop()
	for {
		if f != nil {
			if err = drain(); err != nil {
				return err
			}
			// A rotation moved the file away and the name holds a new one.
			// The old file is read to its end once more, since the writer
			// may have added its last lines after the read above.
			renamed, err := moved(f, path)
			if err != nil {
				return err
			}
			if renamed {
				if err = drain(); err != nil {
					return err
				}
				if err = reopen(false); err != nil {
					return err
				}
				continue
			}
		} else if err = reopen(false); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// moved reports whether the path names another file than the open one.
func moved(f *os.File, path string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	now, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !os.SameFile(held, now), nil
}

// ParseSince reads a --since or since= value: empty is the beginning, a
// duration such as 1h is that long before now, and anything else is a time
// in RFC 3339.
func ParseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("since %q is a negative duration", value)
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("since %q is neither a duration such as 1h nor a time in RFC 3339", value)
	}
	return t, nil
}
