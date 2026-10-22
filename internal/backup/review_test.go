package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPgpassRejectsFIFOWithoutBlocking(t *testing.T) {
	cfg, _, _ := fixture(t)
	target := cfg.Targets[0]
	target.PasswordFile = filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(target.PasswordFile, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, cleanup, err := pgpass(target)
		if cleanup != nil {
			cleanup()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO credentials")
		}
	case <-time.After(time.Second):
		// Unblock the old implementation so a regression cannot leak a goroutine.
		fd, err := unix.Open(target.PasswordFile, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("FIFO credential reader did not unblock during cleanup")
		}
		t.Fatal("opening FIFO credentials blocked")
	}
}

func TestPgpassFollowsSecretSymlink(t *testing.T) {
	cfg, _, _ := fixture(t)
	target := cfg.Targets[0]
	link := filepath.Join(t.TempDir(), "secret")
	if err := os.Symlink(target.PasswordFile, link); err != nil {
		t.Fatal(err)
	}
	target.PasswordFile = link
	_, cleanup, err := pgpass(target)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestRetentionCorruptSameSizeDoesNotDisplaceHealthyHistory(t *testing.T) {
	cfg, _, engine := fixture(t)
	cfg.Storage.KeepSuccessful = 2
	parent := filepath.Join(cfg.Storage.Root, "primary")
	for i := 0; i < 2; i++ {
		if err := engine.Backup(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	healthy := entries[0].Name()
	corrupt := entries[1].Name()
	path := filepath.Join(parent, corrupt, "db-000.dump")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, healthy)); err != nil {
		t.Fatalf("corrupt backup displaced healthy history: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("retention deleted unverified corrupt set", err)
	}
}

func TestHashFileHonorsCancellation(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.WriteFile(filepath.Join(fdPath(dir), "archive"), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashFile(ctx, dir, "archive", "db"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

// Cancel deterministically during hashing, not before the retention scan.
type cancelOnCheck struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelOnCheck) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRetentionReturnsCancellationDuringLastSetScan(t *testing.T) {
	cfg, _, engine := fixture(t)
	if err := engine.Backup(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(filepath.Join(cfg.Storage.Root, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanCtx := &cancelOnCheck{Context: ctx, cancel: cancel, checks: 4}
	if err := retain(scanCtx, dir, "primary", 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("retention swallowed cancellation: %v", err)
	}
}

func TestRetentionCancelledEmptyDirectory(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retain(ctx, dir, "primary", 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("empty scan swallowed cancellation: %v", err)
	}
}

func TestContextReaderBoundsReadsAndChecksBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := contextReader{ctx: ctx, reader: strings.NewReader(strings.Repeat("x", 128*1024))}
	p := make([]byte, 128*1024)
	if n, err := r.Read(p); n != 64*1024 || err != nil {
		t.Fatalf("unbounded read: %d, %v", n, err)
	}
	cancel()
	if n, err := r.Read(p); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("read continued after cancellation: %d, %v", n, err)
	}
}

type waitingRunner struct {
	started, cancelled, release chan struct{}
}

func (r waitingRunner) Run(ctx context.Context, cmd Command) error {
	if filepath.Base(cmd.Path) == "psql" {
		_, err := io.WriteString(cmd.Stdout, "170003\n")
		return err
	}
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	// Model a child that has been cancelled but whose Wait/cleanup has not
	// returned yet. The storage lock and partial set must stay pinned.
	<-r.release
	return ctx.Err()
}

func TestLockHeldUntilCancelledChildWaitAndCleanupReturn(t *testing.T) {
	cfg, _, engine := fixture(t)
	runner := waitingRunner{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	engine.runner = runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- engine.Backup(ctx, cfg) }()
	released := false
	defer func() {
		if !released {
			close(runner.release)
		}
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("child did not observe cancellation")
	}
	if s, err := openStorage(cfg.Storage); err == nil {
		s.Close()
		t.Fatal("lock released before child wait returned")
	}
	close(runner.release)
	released = true
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backup did not finish cleanup")
	}
	entries, err := os.ReadDir(filepath.Join(cfg.Storage.Root, "primary"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial set survived cleanup: %v, %v", entries, err)
	}
	s, err := openStorage(cfg.Storage)
	if err != nil {
		t.Fatal("lock not released after cleanup", err)
	}
	s.Close()
}
