// Package backup orchestrates complete per-target PostgreSQL logical backups.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/quoctan/backup-service/internal/config"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type Engine struct {
	runner IRunner
	log    *zap.Logger
}

var (
	PGDump    string = "pg_dump"
	PGDumpAll string = "pg_dumpall"
	PGRestore string = "pg_restore"
)

func New(runner IRunner, log *zap.Logger) *Engine {
	if runner == nil {
		runner = ExecRunner{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Engine{runner: runner, log: log}
}

func (e *Engine) Backup(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	storage, err := openStorage(cfg.Storage)
	if err != nil {
		return err
	}
	defer storage.Close()

	var failures []error
	for _, target := range cfg.Targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := storage.checkMarker(cfg.Storage.ExpectedDiskID); err != nil {
			return diskError(err)
		}

		err := e.backupTarget(ctx, storage, target, cfg.Storage.KeepSuccessful)
		if err == nil {
			e.log.Info("backup complete", zap.String("target", target.Name))
			continue
		}

		e.log.Error("backup failed", zap.String("target", target.Name), zap.Error(err))
		failures = append(failures, fmt.Errorf("target %s: %w", target.Name, err))
		var shared *storageError
		if errors.As(err, &shared) {
			break
		}
	}

	return errors.Join(failures...)
}

func (e *Engine) backupTarget(parent context.Context, s *storage, t config.Target, keep int) (result error) {
	ctx, cancel := context.WithTimeout(parent, t.Timeout)
	defer cancel()

	pass, cleanup, err := pgpass(t)
	if err != nil {
		return err
	}
	defer cleanup()

	env := commandEnv(t, pass)
	if err := e.checkVersion(ctx, t, env); err != nil {
		return err
	}

	dir, err := s.makeDirTarget(t.Name)
	if err != nil {
		return err
	}
	defer dir.Close()

	id, err := newID()
	if err != nil {
		return err
	}
	partial := ".partial-" + id
	if err := unix.Mkdirat(int(dir.Fd()), partial, 0700); err != nil {
		return diskError(err)
	}
	fd, err := unix.Openat(int(dir.Fd()), partial, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return diskError(err)
	}

	work := os.NewFile(uintptr(fd), partial)
	defer work.Close()
	published := false
	defer func() {
		if !published {
			// Go's RemoveAll does not follow symlinks. The /proc fd anchor pins
			// the parent, so replacing a target pathname cannot redirect cleanup.
			if err := os.RemoveAll(filepath.Join(fdPath(dir), partial)); err != nil {
				result = errors.Join(result, diskError(err))
			}
			if err := dir.Sync(); err != nil {
				result = errors.Join(result, diskError(err))
			}
		}
	}()

	manifest := Manifest{
		SchemaVersion: 1,
		ID:            "backup-" + id,
		Target:        t.Name,
		MajorVersion:  t.MajorVersion,
		StartedAt:     time.Now().UTC(),
	}

	// MARK: databases

	for i, db := range t.Databases {
		// create the backup
		name := fmt.Sprintf("db-%03d.dump", i)
		args := []string{"--no-password", "--format=custom", "--dbname=" + db}
		if err := e.dump(ctx, t, env, work, PGDump, args, name); err != nil {
			return err
		}

		// verify that pg_restore able to read the archive after dump
		// work.Fd() were closed by CLOEXEC, pg_restore run as child process so need
		// to use the parent process id for accessing FDs
		archive := fmt.Sprintf("/proc/%d/fd/%d/%s", os.Getpid(), work.Fd(), name)
		if err := e.run(ctx, t, env, PGRestore, []string{"--list", archive}, io.Discard); err != nil {
			return err
		}

		entry, err := hashFile(ctx, work, name, db)
		if err != nil {
			return diskError(err)
		}

		manifest.Files = append(manifest.Files, entry)
	}

	// MARK: cluster info

	if err := e.dump(ctx, t, env, work, PGDumpAll, []string{"--no-password", "--globals-only", "--no-role-passwords", "--database=" + t.Databases[0]}, "globals.sql"); err != nil {
		return err
	}
	globals, err := hashFile(ctx, work, "globals.sql", "")
	if err != nil {
		return diskError(err)
	}
	manifest.Files = append(manifest.Files, globals)
	manifest.CompletedAt = time.Now().UTC()

	// MARK: complete

	if err := writeManifest(work, manifest); err != nil {
		return diskError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// rename is atomic only within one filesystem. First fsync all files and
	// the staging directory, rename the whole set, then fsync its parent so
	// the published name survives power loss (rename alone is not durability).
	if err := work.Sync(); err != nil {
		return diskError(err)
	}
	if err := unix.Renameat2(int(dir.Fd()), partial, int(dir.Fd()), manifest.ID, unix.RENAME_NOREPLACE); err != nil {
		return diskError(err)
	}
	published = true
	if err := dir.Sync(); err != nil {
		return diskError(err)
	}

	return retain(ctx, dir, t.Name, keep, manifest.ID)
}

func (e *Engine) dump(ctx context.Context, t config.Target, env []string, dir *os.File, tool string, args []string, name string) error {
	f, err := createFile(dir, name)
	if err != nil {
		return err
	}

	runErr := e.run(ctx, t, env, tool, args, f)
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return diskError(syncErr)
	}
	if closeErr != nil {
		return diskError(closeErr)
	}
	if runErr != nil {
		// exec's write errors may hide ENOSPC behind a utility failure; a
		// failed fsync above catches most shared-medium failures, not all.
		return runErr
	}

	return nil
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("cannot generate backup identifier")
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:]), nil
}
