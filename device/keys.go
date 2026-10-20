// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
	"latere.ai/x/pkg/atomicfile"
)

// KeyTTL is the lifetime of every environment key a device mints. The
// platform refuses a longer one (platform spec 220).
const KeyTTL = 30 * 24 * time.Hour

// RotateAfter is the age at which connect replaces the key.
const RotateAfter = 7 * 24 * time.Hour

// Key is one environment key as the computer keeps it: the token the worker
// presents, the jti that revokes it, and when it was minted and expires.
type Key struct {
	Environment string    `json:"environment"`
	Token       string    `json:"token"`
	JTI         string    `json:"jti"`
	MintedAt    time.Time `json:"minted_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// KeyStore keeps the environment key by environment id.
type KeyStore interface {
	// Get answers the key of environment, and false when none is kept.
	Get(environment string) (Key, bool, error)
	Put(k Key) error
	// Delete forgets the key; forgetting what is not there is no error.
	Delete(environment string) error
	// InFile reports whether the last key read or written lives in the
	// file fallback rather than the keychain, which the engine says at
	// every start.
	InFile() bool
	// Name says where keys are kept.
	Name() string
}

// keychainService is the keychain entry's service; the account is the
// environment id.
const keychainService = "latere-cli device key"

// Keychain keeps keys in the operating system's keychain.
type Keychain struct{}

// Get reads the environment's entry.
func (Keychain) Get(environment string) (Key, bool, error) {
	secret, err := keyring.Get(keychainService, environment)
	if errors.Is(err, keyring.ErrNotFound) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("keychain: %w", err)
	}
	var k Key
	if err = json.Unmarshal([]byte(secret), &k); err != nil {
		return Key{}, false, fmt.Errorf("keychain entry for %s: %w", environment, err)
	}
	return k, true, nil
}

// Put writes the environment's entry.
func (Keychain) Put(k Key) error {
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	if err = keyring.Set(keychainService, k.Environment, string(b)); err != nil {
		return fmt.Errorf("keychain: %w", err)
	}
	return nil
}

// Delete removes the environment's entry.
func (Keychain) Delete(environment string) error {
	if err := keyring.Delete(keychainService, environment); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("keychain: %w", err)
	}
	return nil
}

// KeyFile keeps keys in a 0600 JSON file beside the login token.
type KeyFile struct{ Path string }

func (f KeyFile) read() (map[string]Key, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Key{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Key{}
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.Path, err)
	}
	return m, nil
}

func (f KeyFile) write(m map[string]Key) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	if len(m) == 0 {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteSync(f.Path, b, 0o600)
}

// Get reads the environment's key.
func (f KeyFile) Get(environment string) (Key, bool, error) {
	m, err := f.read()
	if err != nil {
		return Key{}, false, err
	}
	k, ok := m[environment]
	return k, ok, nil
}

// Put writes the environment's key.
func (f KeyFile) Put(k Key) error {
	m, err := f.read()
	if err != nil {
		return err
	}
	m[k.Environment] = k
	return f.write(m)
}

// Delete forgets the environment's key, and the file with its last key.
func (f KeyFile) Delete(environment string) error {
	m, err := f.read()
	if err != nil {
		return err
	}
	if _, ok := m[environment]; !ok {
		return nil
	}
	delete(m, environment)
	return f.write(m)
}

// Keys keeps keys in the keychain, and in the file when the keychain cannot,
// as on a headless Linux host with no secret service. Reads look in both, so
// a key the file kept is found after a keychain appears.
type Keys struct {
	Keychain interface {
		Get(string) (Key, bool, error)
		Put(Key) error
		Delete(string) error
	}
	File   KeyFile
	inFile bool
}

// errNoKeychain is the keychain declining, when the key is kept in a file
// by the person's choice.
var errNoKeychain = errors.New("the keychain is not used: " + EnvKeysFile + " names the file the key is kept in")

// noKeychain is a keychain that declines everything.
type noKeychain struct{}

func (noKeychain) Get(string) (Key, bool, error) { return Key{}, false, errNoKeychain }
func (noKeychain) Put(Key) error                 { return errNoKeychain }
func (noKeychain) Delete(string) error           { return errNoKeychain }

// OpenKeys is the store the engine uses: the keychain, falling back to the
// file at path.
func OpenKeys(path string) *Keys { return &Keys{Keychain: Keychain{}, File: KeyFile{Path: path}} }

// Name is where the last key was read or written.
func (k *Keys) Name() string {
	if k.inFile {
		return k.File.Path
	}
	return "the system keychain"
}

// InFile reports whether the last key lives in the file.
func (k *Keys) InFile() bool { return k.inFile }

// Get looks in the keychain, then the file.
func (k *Keys) Get(environment string) (Key, bool, error) {
	key, ok, kerr := k.Keychain.Get(environment)
	if kerr == nil && ok {
		k.inFile = false
		return key, true, nil
	}
	key, ok, err := k.File.Get(environment)
	if err != nil {
		return Key{}, false, err
	}
	if ok {
		k.inFile = true
		return key, true, nil
	}
	if kerr != nil && !errors.Is(kerr, errNoKeychain) {
		// No keychain answered and the file holds nothing: the keychain's
		// failure is what the person needs to read.
		return Key{}, false, kerr
	}
	return Key{}, false, nil
}

// Put writes to the keychain, or to the file when the keychain cannot, and
// removes a copy the other one held.
func (k *Keys) Put(key Key) error {
	if err := k.Keychain.Put(key); err == nil {
		k.inFile = false
		return k.File.Delete(key.Environment)
	}
	if err := k.File.Put(key); err != nil {
		return err
	}
	k.inFile = true
	return nil
}

// Delete forgets the key in both. A keychain that cannot be reached holds
// nothing this CLI wrote there, so its error counts only when the entry is
// still readable afterwards.
func (k *Keys) Delete(environment string) error {
	if err := k.Keychain.Delete(environment); err != nil {
		if _, still, gerr := k.Keychain.Get(environment); gerr == nil && still {
			return err
		}
	}
	return k.File.Delete(environment)
}
