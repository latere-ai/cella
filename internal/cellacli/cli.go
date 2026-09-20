// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package cellacli is the cella command: the flags, the commands, the exit
// codes of design 011 and the two outputs every command has, the columns a
// person reads and the API's own JSON.
package cellacli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/internal/cellaclient"
)

// Env is everything the command reads and writes. It is a parameter so the
// command is driven by a test exactly as a shell drives it.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads the environment. Nil is os.Getenv.
	Getenv func(string) string
	// Terminal is the caller's own terminal, or nil where the input is not
	// one. It is what a session sets raw and resizes.
	Terminal Terminal
	// Version is the build's version, which the user agent of every request
	// carries.
	Version string
	// Identity is the line `cella version` prints for the client: the
	// version, the commit and the build date.
	Identity string
	// Now is the clock the AGE column is measured against. Nil is time.Now.
	Now func() time.Time
}

// Terminal is the caller's terminal as the streaming commands use it: the
// window, raw mode with the restore that undoes it, and the signal that says
// the window changed.
type Terminal interface {
	// Size is the window in columns and rows.
	Size() (cols, rows int, err error)
	// MakeRaw puts the terminal in raw mode and returns what restores it.
	MakeRaw() (restore func() error, err error)
	// Resized delivers one value per window change. The second result ends
	// the watch.
	Resized() (<-chan struct{}, func())
}

// usageError is a flag, a reference or a document the command could not
// read. Design 011 gives it exit 2, outside a session and under one.
type usageError struct{ error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// exitError carries an exit code a command decided on directly, which is
// how the code of a command that ran inside a sandbox leaves this process.
// It is not a failure and nothing prints it.
type exitError struct{ code int }

func (e exitError) Error() string { return "the command inside exited " + strconv.Itoa(e.code) }

// Run is the command. It returns the process exit code and writes every
// byte to the streams it was handed.
func Run(ctx context.Context, e Env) int {
	if e.Getenv == nil {
		e.Getenv = func(string) string { return "" }
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	name, rest := subcommand(e.Args)
	switch name {
	case "":
		_, _ = fmt.Fprint(e.Stderr, usage)
		return 2
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(e.Stdout, usage)
		return 0
	}
	run, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(e.Stderr, "cella: unknown command %q; `cella help` lists them\n", name)
		return 2
	}
	c := &invocation{Env: e, name: name}
	err := run(ctx, c, rest)
	return c.report(err)
}

// subcommand is the rule of design 002, which both binaries share: the
// first argument that does not start with a dash names the command, and the
// arguments around it are the command's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return "help", nil
	}
	return "", args
}

// invocation is one command's run: its name, the streams, and the flags every
// command shares.
type invocation struct {
	Env
	name string
	// The flags of design 011 every command carries.
	url       string
	token     string
	tokenFile string
	ca        string
	json      bool
	verbose   bool
	// session says whether the failure being reported happened under exec,
	// where design 011's second column applies.
	session bool
	// socket says whether the failure happened on the exec or attach socket,
	// where design 011 separates a command that could not start from one
	// that failed part way.
	socket bool
	// started says whether the session had written a byte when it failed.
	started bool
}

// flags builds the command's flag set with the shared flags on it, so
// `cella get --json` and `cella --json get` are the same call.
func (c *invocation) flags(usageLine string) *flag.FlagSet {
	fs := flag.NewFlagSet("cella "+c.name, flag.ContinueOnError)
	fs.SetOutput(c.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(c.Stderr, "usage: %s\n", usageLine)
		fs.PrintDefaults()
	}
	fs.StringVar(&c.url, "url", "", "the control plane's address, else "+cellaclient.URLEnv)
	fs.StringVar(&c.token, "token", "", "the bearer, else "+cellaclient.TokenEnv)
	fs.StringVar(&c.tokenFile, "token-file", "", "a file holding the bearer, else "+cellaclient.TokenFileEnv+", else "+cellaclient.DefaultTokenPath)
	fs.StringVar(&c.ca, "ca", "", "a certificate authority to trust beside the system roots")
	fs.BoolVar(&c.json, "json", false, "write the API's own JSON instead of columns")
	fs.BoolVar(&c.verbose, "v", false, "print the code, the paths and the request id of a refusal")
	return fs
}

// client builds the client from the flags and the environment.
func (c *invocation) client() (*cellaclient.Client, error) {
	client, err := cellaclient.New(cellaclient.Config{
		URL: c.url, Token: c.token, TokenFile: c.tokenFile, CAFile: c.ca,
		UserAgent: "cella/" + c.Version, Getenv: c.Getenv,
	})
	if err != nil {
		return nil, usageError{err}
	}
	return client, nil
}

// report writes the failure and returns the exit code of design 011.
func (c *invocation) report(err error) int {
	if err == nil {
		return 0
	}
	var exit exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	c.write(err)
	return exitFor(err, c.session, c.socket && !c.started)
}

// write prints one refusal: the API's sentence as one line, and under -v the
// code, the paths and the request id on their own. No token and no value is
// ever among them, because the client never puts one in an error.
func (c *invocation) write(err error) {
	var refusal *cellaclient.Error
	if !errors.As(err, &refusal) {
		_, _ = fmt.Fprintf(c.Stderr, "cella: %v\n", err)
		return
	}
	line := refusal.Message
	// Design 011 prints Retry-After in the line of a 429 and nowhere else.
	if refusal.Status == http.StatusTooManyRequests && refusal.RetryAfter != "" {
		line += " Retry after " + refusal.RetryAfter + " seconds."
	}
	_, _ = fmt.Fprintln(c.Stderr, line)
	if !c.verbose {
		return
	}
	_, _ = fmt.Fprintf(c.Stderr, "code: %s\n", refusal.Code)
	if len(refusal.Paths) > 0 {
		_, _ = fmt.Fprintf(c.Stderr, "paths: %s\n", strings.Join(refusal.Paths, ", "))
	}
	if refusal.Detail != "" {
		_, _ = fmt.Fprintf(c.Stderr, "detail: %s\n", refusal.Detail)
	}
	if refusal.RequestID != "" {
		_, _ = fmt.Fprintf(c.Stderr, "request: %s\n", refusal.RequestID)
	}
}

// command is one verb.
type command func(context.Context, *invocation, []string) error

// commands is the table of design 011, less every row whose route this API
// does not serve.
var commands = map[string]command{
	"apply":   apply,
	"get":     get,
	"delete":  remove,
	"start":   start,
	"stop":    stop,
	"exec":    exec,
	"attach":  attach,
	"logs":    logs,
	"cp":      copyFiles,
	"files":   files,
	"egress":  egressRecords,
	"version": version,
}

// splitArgs cuts the arguments at the first `--`, which separates the
// command's own flags from the command line it carries. The flag package
// consumes the marker, so the two halves are separated before it parses.
func splitArgs(args []string) (before, after []string, separated bool) {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i], args[i+1:], true
	}
	return args, nil, false
}

// parse reads the flags wherever they stand among the positional
// arguments, which the flag package alone does not do: it stops at the
// first argument that is not a flag.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err}
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
