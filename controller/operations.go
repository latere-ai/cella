// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"

	"latere.ai/x/cella/runtime"
)

func (c *Controller) Capabilities() runtime.Capabilities { return c.driver.Capabilities() }
func (c *Controller) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	return c.driver.ExportTar(ctx, id, paths, dst)
}
func (c *Controller) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	return c.driver.ImportTar(ctx, id, dest, src)
}
func (c *Controller) Logs(ctx context.Context, id string, req runtime.LogsRequest) (io.ReadCloser, error) {
	return c.driver.Logs(ctx, id, req)
}
