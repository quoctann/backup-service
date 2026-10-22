package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quoctan/backup-service/internal/config"
)

type fakeBackup struct{ called bool }

func (b *fakeBackup) Backup(context.Context, config.Config) error { b.called = true; return nil }

func TestCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "storage:\n  root: /backups\n  expectedDiskID: disk-1\ntargets:\n  - name: primary\n    host: db\n    majorVersion: 17\n    username: backup\n    databases: [app]\n    passwordFile: /secrets/password\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"validate", "backup"} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackup{}
			cmd := New(b)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{name, "--config", path})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if b.called != (name == "backup") {
				t.Fatal("wrong handler")
			}
			if name == "validate" && !strings.Contains(out.String(), "valid") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestMissingAndInvalidConfig(t *testing.T) {
	for _, args := range [][]string{{"backup"}, {"validate", "--config", "/nonexistent"}, {"backup", "extra"}, {"validate", "extra"}} {
		b := &fakeBackup{}
		cmd := New(b)
		cmd.SetArgs(args)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected error for %v", args)
		}
		if b.called {
			t.Fatal("ran backup for invalid input")
		}
	}
}
