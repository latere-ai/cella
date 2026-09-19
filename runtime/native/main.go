// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	driver "latere.ai/x/cella/runtime"
)

type mainProcess struct {
	exec     *execution
	done     chan struct{}
	stopping bool
	err      error
}
type logChunk struct {
	At   time.Time
	Data []byte
}
type logWriter struct {
	mu   sync.Mutex
	file *os.File
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := json.NewEncoder(w.file).Encode(logChunk{At: time.Now().UTC(), Data: p})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
func (d *Driver) startMainLocked(id string, r *record) error {
	log, err := os.OpenFile(filepath.Join(d.dir(id), "logs.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	r.State.Phase = driver.Running
	r.State.Reason = ""
	r.State.ExitCode = nil
	r.State.StartedAt = time.Now().UTC()
	r.State.LastActivityAt = r.State.StartedAt
	r.State.StoppedAt = time.Time{}
	if err = d.save(id, *r); err != nil {
		_ = log.Close()
		return err
	}
	argv := append(append([]string{}, r.Command...), r.Args...)
	e, err := d.execLocked(context.Background(), id, driver.ExecRequest{Command: argv})
	if err != nil {
		_ = log.Close()
		r.State.Phase = "Failed"
		r.State.Reason = "DriverFailed"
		return errors.Join(err, d.save(id, *r))
	}
	r.PID = e.pid
	if err = d.save(id, *r); err != nil {
		_ = e.Close()
		<-e.done
		_ = log.Close()
		return err
	}
	main := &mainProcess{exec: e, done: make(chan struct{})}
	d.mains[id] = main
	go d.supervise(id, main, log)
	return nil
}
func (d *Driver) supervise(id string, main *mainProcess, log *os.File) {
	writer := &logWriter{file: log}
	var wg sync.WaitGroup
	var writeErrors [2]error
	for i, src := range []io.Reader{main.exec.Stdout(), main.exec.Stderr()} {
		wg.Go(func() {
			_, writeErrors[i] = io.Copy(writer, src)
			if writeErrors[i] != nil {
				_ = main.exec.Close()
			}
		})
	}
	code, runErr := main.exec.Wait(context.Background())
	wg.Wait()
	logErr := errors.Join(writeErrors[0], writeErrors[1], log.Close())
	d.mu.Lock()
	defer d.mu.Unlock()
	defer close(main.done)
	if d.mains[id] != main {
		return
	}
	delete(d.mains, id)
	r, err := d.load(id)
	if err != nil {
		if !errors.Is(err, driver.ErrNotFound) {
			main.err = err
		}
		return
	}
	r.PID = 0
	if r.State.Phase == driver.Running {
		r.State.Phase = driver.Stopped
		r.State.Reason = "Exited"
		r.State.ExitCode = &code
		r.State.StoppedAt = time.Now().UTC()
		if code != 0 || runErr != nil {
			r.State.Phase = "Failed"
		}
		if logErr != nil {
			r.State.Phase = "Failed"
			r.State.Reason = "LogWriteFailed"
		}
	}
	if main.stopping {
		r.State.Phase = driver.Stopped
		r.State.Reason = ""
		r.State.ExitCode = nil
		r.State.StoppedAt = time.Now().UTC()
	}
	main.err = d.save(id, r)
}
