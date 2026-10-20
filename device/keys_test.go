// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func testKey(env string) Key {
	now := time.Now().UTC().Truncate(time.Second)
	return Key{Environment: env, Token: "secret-" + env, JTI: "jti-" + env, MintedAt: now, ExpiresAt: now.Add(KeyTTL)}
}

func TestKeysInTheKeychain(t *testing.T) {
	keyring.MockInit()
	keys := OpenKeys(filepath.Join(t.TempDir(), "device-keys.json"))
	if _, ok, err := keys.Get("dev-a"); ok || err != nil {
		t.Fatalf("an empty store answers %v, %v", ok, err)
	}
	want := testKey("dev-a")
	if err := keys.Put(want); err != nil {
		t.Fatal(err)
	}
	if keys.InFile() || keys.Name() != "the system keychain" {
		t.Errorf("a key written to a working keychain reads as kept in %s", keys.Name())
	}
	if _, err := os.Stat(keys.File.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key in the keychain also left the file: %v", err)
	}
	secret, err := keyring.Get("latere-cli device key", "dev-a")
	if err != nil || !strings.Contains(secret, want.Token) {
		t.Errorf("the keychain entry under the device service is %q, %v", secret, err)
	}
	got, ok, err := keys.Get("dev-a")
	if err != nil || !ok || got != want {
		t.Errorf("the key read back as %+v, %v, %v", got, ok, err)
	}
	if err = keys.Delete("dev-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := keys.Get("dev-a"); ok {
		t.Error("a deleted key is still kept")
	}
	if err = keys.Delete("dev-a"); err != nil {
		t.Errorf("deleting what is not there failed: %v", err)
	}
}

func TestKeysFallBackToAFile(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	t.Cleanup(keyring.MockInit)
	path := filepath.Join(t.TempDir(), "conf", "device-keys.json")
	keys := OpenKeys(path)
	if _, _, err := keys.Get("dev-a"); err == nil || !strings.Contains(err.Error(), "no secret service") {
		t.Errorf("an empty store with no keychain answers %v", err)
	}
	want := testKey("dev-a")
	if err := keys.Put(want); err != nil {
		t.Fatal(err)
	}
	if !keys.InFile() || keys.Name() != path {
		t.Errorf("a key with no keychain reads as kept in %s", keys.Name())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file is %v, %v; want mode 0600", info, err)
	}
	if err = keys.Put(testKey("dev-b")); err != nil {
		t.Fatal(err)
	}
	fresh := OpenKeys(path)
	got, ok, err := fresh.Get("dev-a")
	if err != nil || !ok || got != want || !fresh.InFile() {
		t.Errorf("the file's key reads back as %+v, %v, %v", got, ok, err)
	}
	if err = fresh.Delete("dev-a"); err != nil {
		t.Fatal(err)
	}
	if err = fresh.Delete("dev-b"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file outlived its last key: %v", err)
	}

	// A keychain that appears later takes the key, and the file's copy goes.
	if err = keys.Put(want); err != nil {
		t.Fatal(err)
	}
	keyring.MockInit()
	if err = keys.Put(want); err != nil {
		t.Fatal(err)
	}
	if keys.InFile() {
		t.Error("a working keychain did not take the key")
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file kept a copy of a key the keychain holds: %v", err)
	}
}

func TestKeyFileRefusesWhatItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-keys.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := KeyFile{Path: path}
	if _, _, err := f.Get("dev-a"); err == nil {
		t.Error("a malformed key file reads")
	}
	if err := f.Put(testKey("dev-a")); err == nil {
		t.Error("a malformed key file is overwritten")
	}
	if err := f.Delete("dev-a"); err == nil {
		t.Error("a malformed key file is deleted from")
	}
	keyring.MockInit()
	if err := keyring.Set("latere-cli device key", "dev-bad", "not json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Keychain{}).Get("dev-bad"); err == nil {
		t.Error("a malformed keychain entry reads")
	}
}

func TestKeysFileAlone(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	path := filepath.Join(dir, "elsewhere", "keys.json")
	t.Setenv("HOME", dir)
	t.Setenv(EnvKeysFile, path)
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.KeyFile() != path {
		t.Errorf("the key file is %s, want %s", paths.KeyFile(), path)
	}
	keys := paths.Keys()
	if _, ok, err := keys.Get("dev-a"); ok || err != nil {
		t.Errorf("an empty file reads %v, %v", ok, err)
	}
	if err = keys.Put(testKey("dev-a")); err != nil {
		t.Fatal(err)
	}
	if !keys.InFile() {
		t.Error("a key kept by LATERE_DEVICE_KEYS_FILE reads as in the keychain")
	}
	if _, err := keyring.Get(keychainService, "dev-a"); err == nil {
		t.Error("the keychain was written although the key is kept in a file")
	}
	if got, ok, err := keys.Get("dev-a"); err != nil || !ok || got.Token != "secret-dev-a" {
		t.Errorf("the key reads back as %+v, %v, %v", got, ok, err)
	}
	if err = keys.Delete("dev-a"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range paths.Deny("linux") {
		found = found || d == path
	}
	if !found {
		t.Errorf("the key file outside the configuration directory is not denied: %v", paths.Deny("linux"))
	}
}
