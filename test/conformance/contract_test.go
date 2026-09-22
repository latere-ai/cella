// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The external form of the suite: one command, against a server somebody
// else is running. It is behind the e2e tag because a run of it needs a
// server, and the bar's untagged suite has none.
//
//	go test -tags=e2e -count=1 -timeout 30m -v -run '^TestContract$' ./test/conformance -args \
//	  -url $CELLA_TEST_URL -issuer $CELLA_TEST_ISSUER -capabilities files,attach
//
// With no -url it skips whole and says which flag turns it on.

package conformance

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// The inputs of design 015, as flags. Each has an environment default, so a
// tier that exports CELLA_TEST_URL and CELLA_TEST_TOKEN runs the command
// with no flag at all.
var (
	flagURL          = flag.String("url", os.Getenv("CELLA_TEST_URL"), "the server under test")
	flagIssuer       = flag.String("issuer", os.Getenv("CELLA_TEST_ISSUER"), "an issuer with a mint route, to take every subject's token from")
	flagToken        = flag.String("token", os.Getenv("CELLA_TEST_TOKEN"), "the caller's bearer, where there is no issuer to mint one")
	flagAdmin        = flag.String("admin", os.Getenv("CELLA_TEST_ADMIN"), "a bearer the server treats as an administrator")
	flagImage        = flag.String("image", os.Getenv("CELLA_TEST_IMAGE"), "the image every case creates from; empty creates from a command")
	flagCapabilities = flag.String("capabilities", os.Getenv("CELLA_TEST_CAPABILITIES"), "what the environment declares, comma separated")
	flagAuthorizer   = flag.String("authorizer-control", os.Getenv("CELLA_TEST_AUTHORIZER_CONTROL"), "the authorizer stub's control URL")
	flagAdmission    = flag.String("admission-control", os.Getenv("CELLA_TEST_ADMISSION_CONTROL"), "the admission stub's control URL")
	flagSink         = flag.String("sink", os.Getenv("CELLA_TEST_SINK"), "the event sink's URL, whose /events route is read back")
	flagCella        = flag.String("cella", os.Getenv("CELLA_TEST_CELLA"), "the built agent command, for the agent case")
	flagDisplay      = flag.String("display-image", os.Getenv("CELLA_TEST_DISPLAY_IMAGE"), "an image with a desktop, for the computer-use case")
	flagUpstream     = flag.String("upstream", os.Getenv("CELLA_TEST_UPSTREAM"), "the host a sandbox reaches through the gateway")
	flagQueued       = flag.String("queued-environment", os.Getenv("CELLA_TEST_QUEUED_ENVIRONMENT"), "an environment that queues, for the sets case")
	flagWorker       = flag.String("worker-environment", os.Getenv("CELLA_TEST_WORKER_ENVIRONMENT"), "an environment a worker serves")
	flagKnown        = flag.String("known", "", "a file declaring the cases this server fails and why")
	flagSkip         = flag.String("skip", "", "group or case names to skip, comma separated")
)

// TestContract runs the suite against the server the flags name and fails on
// every case the server does not answer and has not declared.
func TestContract(t *testing.T) {
	if strings.TrimSpace(*flagURL) == "" {
		t.Skip("no server: run with -args -url <the server under test>")
	}
	known, err := LoadDeclaration(*flagKnown)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		URL:               *flagURL,
		Caller:            *flagToken,
		Admin:             *flagAdmin,
		Image:             *flagImage,
		DisplayImage:      *flagDisplay,
		Upstream:          *flagUpstream,
		QueuedEnvironment: *flagQueued,
		WorkerEnvironment: *flagWorker,
		AuthorizerControl: *flagAuthorizer,
		AdmissionControl:  *flagAdmission,
		SinkControl:       *flagSink,
		Cella:             *flagCella,
		Capabilities:      split(*flagCapabilities),
		Skip:              split(*flagSkip),
		Known:             known,
	}
	if issuer := strings.TrimSpace(*flagIssuer); issuer != "" {
		cfg.Token = Minter(issuer)
	}
	report := Run(t, cfg)
	if len(report.Passed) == 0 {
		t.Fatal("no case passed, which is a suite that did not run")
	}
}

// split reads a comma separated flag, dropping what is empty.
func split(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
