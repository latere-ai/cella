// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command cella-stubs serves the operator endpoints a control plane dials:
// the issuer of spec 006, the authorizer of spec 006, the admission
// endpoint of spec 007 and the event sink of spec 009. It is what makes a
// clean clone runnable with `make run` and what the kind stack of spec 012
// puts beside `cellad`.
//
// It is a test binary. Nothing under deploy/ names its image, no
// installation runs it, and every answer it gives is one a flag told it to
// give.
//
//	cella-stubs [-issuer addr] [-authorizer addr] [-admission addr] [-sink addr] ...
//
// One line per started role is written to standard output, "<role> <url>",
// so a script reads the addresses it was given rather than the ones it
// asked for. Every request is logged to standard error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"latere.ai/x/cella/internal/stubs"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the flags, starts every role that has an address, prints
// them, and serves until the context is done.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usage(stderr, err)
	}
	o.Log = stderr
	s, err := stubs.Start(*o)
	if err != nil {
		fmt.Fprintf(stderr, "cella-stubs: %v\n", err)
		return 1
	}
	for _, role := range stubs.RoleOrder {
		if url := s.URL(role); url != "" {
			fmt.Fprintf(stdout, "%s %s\n", role, url)
		}
	}
	<-ctx.Done()
	// The signal is what asked for the stop, so the shutdown runs on a
	// context of its own and not on the cancelled one.
	if err := s.Close(context.WithoutCancel(ctx)); err != nil {
		fmt.Fprintf(stderr, "cella-stubs: %v\n", err)
		return 1
	}
	return 0
}

// usage reports a bad flag as the exit code a shell reads for one.
func usage(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "cella-stubs: %v\n", err)
	return 2
}

// parse reads the flag table of spec 049 into the package's options.
func parse(args []string, stderr io.Writer) (*stubs.Options, error) {
	fs := flag.NewFlagSet("cella-stubs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o stubs.Options
	var deny, warn, secrets list
	defaults, rewrites := pairs{}, pairs{}

	fs.StringVar(&o.Issuer.Addr, "issuer", stubs.DefaultAddrs[stubs.RoleIssuer], "the issuer's listen address, empty to turn the role off")
	fs.StringVar(&o.Issuer.URL, "issuer-url", "", "the iss every minted token carries; the listen address by default")
	fs.StringVar(&o.Issuer.Audience, "issuer-audience", stubs.DefaultAudience, "the aud a minted token carries when the request names none")
	fs.StringVar(&o.Issuer.Algorithm, "issuer-alg", stubs.AlgRS256, "the key set's algorithm, rs256 or es256")

	fs.StringVar(&o.Authorizer.Addr, "authorizer", stubs.DefaultAddrs[stubs.RoleAuthorizer], "the authorizer's listen address, empty to turn the role off")
	fs.StringVar(&o.Authorizer.Token, "authorizer-token", "", "the bearer the authorizer requires; the shared stub's default when empty")
	fs.Var(&deny, "authorizer-deny", "an action denied for every subject, repeatable")
	fs.StringVar(&o.Authorizer.Limits, "authorizer-limits", "", "the limits object returned on every allow, as JSON")
	fs.StringVar(&o.Authorizer.Filter, "authorizer-filter", "", "the filter returned on every allow, as JSON")
	fs.IntVar(&o.Authorizer.TTL, "authorizer-ttl", 0, "how long an allow may be cached, in seconds")
	fs.StringVar(&o.Authorizer.Fail, "authorizer-fail", "", "timeout, malformed, no-allow or status:<code>")

	fs.StringVar(&o.Admission.Addr, "admission", stubs.DefaultAddrs[stubs.RoleAdmission], "the admission endpoint's listen address, empty to turn the role off")
	fs.StringVar(&o.Admission.Token, "admission-token", "", "the bearer the admission endpoint requires")
	fs.Var(&defaults, "admission-default", "<field>=<value> applied where the manifest's spec leaves it empty, repeatable")
	fs.Var(&rewrites, "admission-rewrite", "<field>=<value> applied whatever the manifest says, repeatable")
	fs.Var(&warn, "admission-warn", "a warning carried on every allow, repeatable")
	fs.StringVar(&o.Admission.Refuse, "admission-refuse", "", "the reason every apply is refused with")
	fs.StringVar(&o.Admission.RefuseImage, "admission-refuse-image", "", "refuse an apply naming this image")
	fs.StringVar(&o.Admission.Fail, "admission-fail", "", "timeout, malformed, no-allow or status:<code>")

	fs.StringVar(&o.Sink.Addr, "sink", stubs.DefaultAddrs[stubs.RoleSink], "the sink's listen address, empty to turn the role off")
	fs.Var(&secrets, "sink-secret", "a secret every delivery's signature is verified against, repeatable for a rotation")
	fs.IntVar(&o.Sink.FailFirst, "sink-fail-first", 0, "answer the first n verified deliveries with the failure status")
	fs.IntVar(&o.Sink.FailStatus, "sink-fail-status", stubs.DefaultFailStatus, "the status -sink-fail-first answers")
	fs.IntVar(&o.Sink.Status, "sink-status", 0, "answer the next delivery with this status and then return to normal")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%q is not an argument; every stub is configured by flag", fs.Arg(0))
	}
	o.Authorizer.Deny, o.Admission.Warnings, o.Sink.Secrets = deny, warn, secrets
	o.Admission.Defaults, o.Admission.Rewrites = defaults, rewrites
	if len(o.Sink.Secrets) == 0 {
		o.Sink.Secrets = []string{stubs.DefaultSinkSecret}
	}
	return &o, nil
}

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string { return strings.Join(*l, ",") }

func (l *list) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("the value is empty")
	}
	*l = append(*l, v)
	return nil
}

// pairs is a repeatable <key>=<value> flag.
type pairs map[string]string

func (p pairs) String() string {
	out := make([]string, 0, len(p))
	for k, v := range p {
		out = append(out, k+"="+v)
	}
	return strings.Join(out, ",")
}

func (p pairs) Set(v string) error {
	key, value, ok := strings.Cut(v, "=")
	if key = strings.TrimSpace(key); !ok || key == "" {
		return fmt.Errorf("%q is not <field>=<value>", v)
	}
	p[key] = value
	return nil
}
