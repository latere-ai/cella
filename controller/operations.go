// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"

	"latere.ai/x/cella/runtime"
)

func (c *Controller) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	d, err := c.driverOf(id)
	if err != nil {
		return err
	}
	return d.ExportTar(ctx, id, paths, dst)
}
func (c *Controller) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	d, err := c.driverOf(id)
	if err != nil {
		return err
	}
	return d.ImportTar(ctx, id, dest, src)
}
func (c *Controller) Logs(ctx context.Context, id string, req runtime.LogsRequest) (io.ReadCloser, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	return d.Logs(ctx, id, req)
}

// Attach opens one terminal in a sandbox. A driver that declares no Attach has
// no Attacher, which the API reports as the capability the environment lacks.
func (c *Controller) Attach(ctx context.Context, id string, req runtime.AttachRequest) (runtime.Session, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	a, ok := d.(runtime.Attacher)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return a.Attach(ctx, id, req)
}

// Files returns the driver's per-file half. A driver that declares no Files
// has no FileStore, which the API reports as the capability the environment
// lacks.
func (c *Controller) Files(id string) (runtime.FileStore, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	store, ok := d.(runtime.FileStore)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return store, nil
}
