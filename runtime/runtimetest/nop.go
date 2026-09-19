// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"io"
	"strings"

	"latere.ai/x/cella/runtime"
)

// Nop implements every runtime.Driver method as a no-op returning zero values.
// Embed it and override the methods a test drives, so adding a method to
// runtime.Driver is implemented once here and not in every fake. Two methods
// return an empty value instead of a nil interface, so a caller of an
// embedder does not dereference nil: Exec returns NopExec and Logs an empty
// reader. Nop does not pass Run; it is a base for fakes, not a driver.
type Nop struct{}

var _ runtime.Driver = Nop{}

func (Nop) Name() string                       { return "nop" }
func (Nop) Isolation() string                  { return runtime.IsolationNone }
func (Nop) Capabilities() runtime.Capabilities { return runtime.Capabilities{} }
func (Nop) Preflight(context.Context) error    { return nil }
func (Nop) Ready(context.Context) error        { return nil }
func (Nop) Create(context.Context, runtime.CreateSpec) (runtime.Ref, error) {
	return runtime.Ref{}, nil
}
func (Nop) Start(context.Context, string) error                    { return nil }
func (Nop) Stop(context.Context, string) error                     { return nil }
func (Nop) Delete(context.Context, string) error                   { return nil }
func (Nop) Update(context.Context, string, runtime.Change) error   { return nil }
func (Nop) Inspect(context.Context, string) (runtime.State, error) { return runtime.State{}, nil }
func (Nop) List(context.Context, runtime.Filter) ([]runtime.State, error) {
	return nil, nil
}
func (Nop) Exec(context.Context, string, runtime.ExecRequest) (runtime.Exec, error) {
	return NopExec{}, nil
}
func (Nop) Logs(context.Context, string, runtime.LogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (Nop) ExportTar(context.Context, string, []string, io.Writer) error { return nil }
func (Nop) ImportTar(context.Context, string, string, io.Reader) error   { return nil }
func (Nop) Touch(context.Context, string) error                          { return nil }

// NopExec is the exec Nop returns: both streams empty, exit code 0.
type NopExec struct{}

var _ runtime.Exec = NopExec{}

func (NopExec) Stdout() io.Reader                 { return strings.NewReader("") }
func (NopExec) Stderr() io.Reader                 { return strings.NewReader("") }
func (NopExec) Wait(context.Context) (int, error) { return 0, nil }
func (NopExec) Close() error                      { return nil }
