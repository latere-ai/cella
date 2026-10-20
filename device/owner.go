// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"latere.ai/x/pkg/atomicfile"
)

// owner is the record of whose device this computer serves: the device and
// the person (the sub of their tokens) the platform confirmed as its owner,
// at a connect or by minting its key or reading it as that person. An
// engine that takes its tokens from a client refuses another person's once
// the owner is recorded; until then, a removal the platform answers to
// tokens of an unconfirmed person never makes the computer forget the
// device, since the tokens may be someone else's.
type owner struct {
	Device  string `json:"device"`
	Subject string `json:"subject"`
}

// readOwner is the person recorded for device, "" when none is.
func readOwner(path, device string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var o owner
	if err = json.Unmarshal(raw, &o); err != nil {
		return "", err
	}
	if o.Device != device {
		return "", nil
	}
	return o.Subject, nil
}

// writeOwner records subject as the owner of device, mode 0600.
func writeOwner(path, device, subject string) error {
	raw, err := json.Marshal(owner{Device: device, Subject: subject})
	if err != nil {
		return err
	}
	if err = ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicfile.WriteSync(path, raw, 0o600)
}

// RecordOwner records subject, the person the command is signed in as, as
// the owner of device: `latere devices connect` calls it once its connect
// went through.
func RecordOwner(paths Paths, device, subject string) error {
	if subject == "" {
		return nil
	}
	return writeOwner(paths.OwnerFile(), device, subject)
}

// removeOwner deletes the record when it names device, which is no longer
// served.
func removeOwner(path, device string) error {
	subject, err := readOwner(path, device)
	if err != nil || subject == "" {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
