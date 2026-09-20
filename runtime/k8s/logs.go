// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
)

// Logs streams what the sandbox's main process wrote. The Pod holds the
// output, so a sandbox that has stopped has none: the log of the previous leg
// left with the Pod that produced it.
func (d *Driver) Logs(ctx context.Context, id string, req driver.LogsRequest) (io.ReadCloser, error) {
	if _, err := d.getClaim(ctx, id); err != nil {
		return nil, err
	}
	pod, err := d.getPod(ctx, id)
	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, driver.ErrNotRunning
	}
	opts := &corev1.PodLogOptions{Container: Container, Follow: req.Follow}
	if !req.Since.IsZero() {
		since := metav1.NewTime(req.Since)
		opts.SinceTime = &since
	}
	if req.TailLines > 0 {
		lines := int64(req.TailLines)
		opts.TailLines = &lines
	}
	stream, err := d.cs.CoreV1().Pods(d.opts.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("pod logs: %w", err)
	}
	return stream, nil
}
