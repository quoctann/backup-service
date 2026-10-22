package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quoctan/backup-service/internal/config"
	"go.uber.org/zap"
)

type fakeRunner struct {
	mu       sync.Mutex
	calls    []Command
	fail     string
	password string
}

func (f *fakeRunner) Run(ctx context.Context, cmd Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if filepath.Base(cmd.Path) == f.fail {
		return errors.New("sensitive stderr must not escape")
	}
	var passPath string
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "PGPASSFILE=") {
			passPath = strings.TrimPrefix(env, "PGPASSFILE=")
		}
	}
	if passPath != "" {
		info, err := os.Stat(passPath)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0600 {
			return errors.New("unsafe pgpass mode")
		}
		data, err := os.ReadFile(passPath)
		if err != nil {
			return err
		}
		f.password = string(data)
	}
	if cmd.Stdout != nil {
		if filepath.Base(cmd.Path) == "psql" {
			_, _ = io.WriteString(cmd.Stdout, "170003\n")
		} else {
			_, _ = io.WriteString(cmd.Stdout, "backup content\n")
		}
	}
	return nil
}

func fixture(t *testing.T) (config.Config, *fakeRunner, *Engine) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".backup-disk-id"), []byte("disk-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pass := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pass, []byte("secret:with\\escape\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Storage: config.Storage{Root: root, ExpectedDiskID: "disk-1", KeepSuccessful: 2}, Targets: []config.Target{{Name: "primary", Host: "db", Port: 5432, MajorVersion: 17, Username: "backup", Databases: []string{"app", "数据库"}, PasswordFile: pass, SSLMode: "require", Timeout: time.Minute}}}
	runner := &fakeRunner{}
	return cfg, runner, New(runner, zap.NewNop())
}

func TestMarkerRequiredBeforeWrites(t *testing.T) {
	for _, variant := range []string{"missing", "wrong", "symlink"} {
		t.Run(variant, func(t *testing.T) {
			cfg, runner, engine := fixture(t)
			marker := filepath.Join(cfg.Storage.Root, ".backup-disk-id")
			_ = os.Remove(marker)
			if variant == "wrong" {
				_ = os.WriteFile(marker, []byte("wrong"), 0600)
			}
			if variant == "symlink" {
				_ = os.Symlink(cfg.Targets[0].PasswordFile, marker)
			}
			if err := engine.Backup(context.Background(), cfg); err == nil {
				t.Fatal("expected disk error")
			}
			if len(runner.calls) != 0 {
				t.Fatal("ran command on invalid disk")
			}
			if _, err := os.Stat(filepath.Join(cfg.Storage.Root, ".backup.lock")); !os.IsNotExist(err) {
				t.Fatal("wrote lock before disk verification")
			}
		})
	}
}

func TestCompleteSetAndRetention(t *testing.T) {
	cfg, runner, engine := fixture(t)
	for i := 0; i < 4; i++ {
		if err := engine.Backup(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.Storage.Root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want two successful sets, got %d", len(entries))
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "backup-") {
			t.Fatal(entry.Name())
		}
		base := filepath.Join(cfg.Storage.Root, "primary", entry.Name())
		for _, name := range []string{"manifest.json", "globals.sql", "db-000.dump", "db-001.dump"} {
			if _, err := os.Stat(filepath.Join(base, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !strings.Contains(runner.password, `secret\:with\\escape`) {
		t.Fatalf("incorrect pgpass escaping: %q", runner.password)
	}
	for _, call := range runner.calls {
		if !strings.HasPrefix(call.Path, "/usr/lib/postgresql/17/bin/") {
			t.Fatal(call.Path)
		}
		if strings.Contains(strings.Join(call.Args, " "), "secret") {
			t.Fatal("password in arguments")
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.Root, ".backup.lock")); err != nil {
		t.Fatal("persistent lock missing")
	}
}

func TestFailedBackupDoesNotPruneOrPublish(t *testing.T) {
	cfg, runner, engine := fixture(t)
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	runner.fail = "pg_restore"
	if err := engine.Backup(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe failure: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(cfg.Storage.Root, "primary"))
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "backup-") {
		t.Fatalf("failed set published or leaked: %v", entries)
	}
}

func TestTargetFailureContinues(t *testing.T) {
	cfg, runner, engine := fixture(t)
	cfg.Targets = append([]config.Target{{Name: "broken", Host: "db", Port: 5432, MajorVersion: 17, Username: "backup", Databases: []string{"app"}, PasswordFile: "/nonexistent", SSLMode: "require", Timeout: time.Minute}}, cfg.Targets...)
	if err := engine.Backup(context.Background(), cfg); err == nil {
		t.Fatal("expected combined failure")
	}
	if len(runner.calls) == 0 {
		t.Fatal("second target not attempted")
	}
}

func TestLockAndSymlinkSafety(t *testing.T) {
	cfg, _, engine := fixture(t)
	store, err := openStorage(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := engine.Backup(context.Background(), cfg); err == nil {
		t.Fatal("second lock acquired")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cfg.Storage.Root, "primary")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.makeDirTarget("primary"); err == nil {
		t.Fatal("followed target symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("outside changed")
	}
}

func TestCanceledBackupWritesNoSet(t *testing.T) {
	cfg, runner, engine := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.Backup(ctx, cfg); err == nil {
		t.Fatal("expected cancellation")
	}
	if len(runner.calls) != 0 {
		t.Fatal("ran command after cancellation")
	}
}
