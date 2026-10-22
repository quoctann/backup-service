package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecRunner(t *testing.T) {
	runner := ExecRunner{}
	var out bytes.Buffer
	if err := runner.Run(context.Background(), Command{Path: "/bin/echo", Args: []string{"safe; not shell"}, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "safe; not shell\n" {
		t.Fatal(out.String())
	}
	if err := runner.Run(context.Background(), Command{Path: "/bin/sh", Args: []string{"-c", "echo sensitive >&2; exit 1"}}); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runner.Run(ctx, Command{Path: "/bin/sleep", Args: []string{"5"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestCredentialsAndEnvironment(t *testing.T) {
	cfg, _, _ := fixture(t)
	target := cfg.Targets[0]
	t.Setenv("PGPASSWORD", "must-not-inherit")
	t.Setenv("PGSERVICE", "must-not-inherit")
	t.Setenv("PGSSLROOTCERT", "/ssl/root.crt")
	for _, env := range commandEnv(target, "/private/pgpass") {
		if strings.Contains(env, "must-not-inherit") {
			t.Fatal("unsafe environment inherited")
		}
	}
	for _, data := range []string{"", "multi\nline", "nul\x00value", strings.Repeat("x", 65537)} {
		if err := os.WriteFile(target.PasswordFile, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := pgpass(target); err == nil {
			t.Fatal("accepted malformed credential")
		}
	}
	if err := os.WriteFile(target.PasswordFile, []byte("space password \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := pgpass(target)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(data), "space password \n") {
		t.Fatalf("whitespace changed: %q", data)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("credential not removed")
	}
}

type versionRunner string

func (v versionRunner) Run(_ context.Context, c Command) error {
	_, err := c.Stdout.Write([]byte(v))
	return err
}

func TestServerVersionMustMatch(t *testing.T) {
	cfg, _, engine := fixture(t)
	for _, version := range []string{"160001", "not a number", strings.Repeat("1", 200)} {
		engine.runner = versionRunner(version)
		if err := engine.Backup(context.Background(), cfg); err == nil {
			t.Fatal("accepted wrong version", version)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.Root, "primary")); !os.IsNotExist(err) {
		t.Fatal("created set before checking version")
	}
}

func TestManifestHashesMatchActualFiles(t *testing.T) {
	cfg, _, engine := fixture(t)
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(cfg.Storage.Root, "primary"))
	base := filepath.Join(cfg.Storage.Root, "primary", entries[0].Name())
	data, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 3 || manifest.Target != "primary" || manifest.MajorVersion != 17 {
		t.Fatalf("wrong manifest: %+v", manifest)
	}
	for _, entry := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(base, entry.Name))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if entry.SHA256 != hex.EncodeToString(hash[:]) || entry.Size != int64(len(data)) {
			t.Fatalf("wrong hash entry: %+v", entry)
		}
	}
}

func TestRetentionIgnoresUnownedAndIncompleteDirectories(t *testing.T) {
	cfg, _, engine := fixture(t)
	cfg.Storage.KeepSuccessful = 1
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(cfg.Storage.Root, "primary")
	for _, name := range []string{"notes", ".partial-old", "backup-20000101T000000.000000000Z-0000000000000000"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	link := "backup-20000101T000000.000000000Z-0000000000000001"
	if err := os.Symlink(outside, filepath.Join(parent, link)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := engine.Backup(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 5 {
		t.Fatalf("unexpected retained entries %v", entries)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatal("retention followed symlink")
	}
}

func TestStorageFailuresAbortOtherTargets(t *testing.T) {
	cfg, runner, engine := fixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(cfg.Storage.Root, "primary")); err != nil {
		t.Fatal(err)
	}
	second := cfg.Targets[0]
	second.Name = "second"
	cfg.Targets = append(cfg.Targets, second)
	err := engine.Backup(context.Background(), cfg)
	var shared *storageError
	if !errors.As(err, &shared) || !errors.Is(err, shared.err) {
		t.Fatalf("expected wrapped storage failure: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("continued after storage failure: %d commands", len(runner.calls))
	}
}

func TestSymlinkRootAndLockRejected(t *testing.T) {
	cfg, _, engine := fixture(t)
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(cfg.Storage.Root, link); err != nil {
		t.Fatal(err)
	}
	original := cfg.Storage.Root
	cfg.Storage.Root = link
	if err := engine.Backup(context.Background(), cfg); err == nil {
		t.Fatal("followed root symlink")
	}
	cfg.Storage.Root = original
	if err := os.Symlink(cfg.Targets[0].PasswordFile, filepath.Join(original, ".backup.lock")); err != nil {
		t.Fatal(err)
	}
	if err := engine.Backup(context.Background(), cfg); err == nil {
		t.Fatal("followed lock symlink")
	}
}

func TestInvalidConfigRejectedBeforeStorage(t *testing.T) {
	cfg, _, engine := fixture(t)
	cfg.Targets[0].Name = "../bad"
	if err := engine.Backup(context.Background(), cfg); err == nil {
		t.Fatal("accepted invalid configuration")
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.Root, ".backup.lock")); !os.IsNotExist(err) {
		t.Fatal("wrote before config validation")
	}
	if New(nil, nil) == nil {
		t.Fatal("nil default engine")
	}
}

func TestMalformedManifestRejected(t *testing.T) {
	cfg, _, engine := fixture(t)
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Storage.Root, "primary")
	parent, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	entries, _ := os.ReadDir(path)
	name := entries[0].Name()
	manifest := filepath.Join(path, name, "manifest.json")
	original, _ := os.ReadFile(manifest)
	for _, data := range []string{`{`, string(original) + `{}`, strings.Replace(string(original), `"schemaVersion":1`, `"schemaVersion":2`, 1), strings.Replace(string(original), `"majorVersion":17`, `"majorVersion":15`, 1), strings.Replace(string(original), `"name":"globals.sql"`, `"name":"../outside"`, 1)} {
		if err := os.WriteFile(manifest, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSuccessful(context.Background(), parent, name, "primary"); err == nil {
			t.Fatalf("accepted malformed manifest: %s", data)
		}
	}
	if _, err := readSuccessful(context.Background(), parent, "notes", "primary"); err == nil {
		t.Fatal("accepted unrelated directory")
	}
	if err := os.WriteFile(manifest, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, name, "globals.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSuccessful(context.Background(), parent, name, "primary"); err == nil {
		t.Fatal("accepted missing artifact")
	}
}

func TestRetentionAlwaysProtectsJustPublishedSet(t *testing.T) {
	cfg, _, engine := fixture(t)
	cfg.Storage.KeepSuccessful = 1
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(cfg.Storage.Root, "primary")
	entries, _ := os.ReadDir(parent)
	old := entries[0].Name()
	// Simulate an old run having a clock far in the future. A subsequent
	// clock correction must never cause retention to delete the new backup.
	future := strings.Replace(old, old[7:11], "2099", 1)
	data, _ := os.ReadFile(filepath.Join(parent, old, "manifest.json"))
	if err := os.WriteFile(filepath.Join(parent, old, "manifest.json"), []byte(strings.Replace(string(data), old, future, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(parent, old), filepath.Join(parent, future)); err != nil {
		t.Fatal(err)
	}
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() == future {
		t.Fatal("retention deleted just-published backup after clock correction")
	}
}
