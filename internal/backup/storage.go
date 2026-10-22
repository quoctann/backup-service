package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/quoctan/backup-service/internal/config"
	"golang.org/x/sys/unix"
)

// storageError means the shared backup medium is no longer safe to write.
// Unlike an individual database failure it aborts the remaining targets.
type storageError struct{ err error }

func (e *storageError) Error() string {
	return "backup storage operation failed"
}

func (e *storageError) Unwrap() error {
	return e.err
}

func diskError(err error) error {
	// Cancellation during file hashing is not evidence of a failed disk. A
	// target-local deadline must leave other independent targets eligible.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &storageError{err: err}
}

type storage struct{ root, lock *os.File }

// openDirectory walks every component with O_NOFOLLOW, rather than checking
// with Lstat and opening later (a symlink replacement would race that check),
// e.g. open /var/app/data will open each folder components "var", "app", "data"
// unfollows the Linux symlink
func openDirectory(path string) (*os.File, error) {
	// open root path / with readonly permission, only open directory, auto close
	// fd after exec successfully, after this, fd become a file descriptor
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, e
		}

		fd = next
	}

	return os.NewFile(uintptr(fd), path), nil
}

func openStorage(c config.Storage) (*storage, error) {
	root, err := openDirectory(c.Root)
	if err != nil {
		return nil, diskError(err)
	}

	storage := &storage{root: root}
	if err := storage.checkMarker(c.ExpectedDiskID); err != nil {
		storage.Close()
		return nil, diskError(err)
	}

	fd, err := unix.Openat(int(root.Fd()), ".backup.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		storage.Close()
		return nil, diskError(err)
	}

	storage.lock = os.NewFile(uintptr(fd), ".backup.lock")
	info, err := storage.lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		storage.Close()
		return nil, diskError(errors.New("invalid lock file"))
	}

	// Mutex at OS level, asking Linux lock this file, flock locks the inode, not
	// its pathname. Never unlink this persistent file: a second process could
	// otherwise lock a new inode simultaneously.
	//
	// LOCK_EX: exclusive lock, only 1 process can keep the lock at that time
	// LOCK_NB: non-blocking, once acquire lock failed, escape with error instead
	// of blocking the process
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		storage.Close()
		return nil, errors.New("backup lock unavailable")
	}

	return storage, nil
}

// The marker file must be existed on disk to prevent writing on /mnt/ folder
// instead of actual mounted disk
func (s *storage) checkMarker(expected string) error {
	fd, err := unix.Openat(int(s.root.Fd()), ".backup-disk-id", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}

	f := os.NewFile(uintptr(fd), ".backup-disk-id")
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("disk marker is not a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return err
	}

	if len(data) > 1024 || strings.TrimSpace(string(data)) != expected {
		return errors.New("disk marker mismatch")
	}

	return nil
}

func (s *storage) Close() {
	if s.lock != nil {
		_ = s.lock.Close()
	}
	if s.root != nil {
		_ = s.root.Close()
	}
}

/*
Get the Linux File Descriptor path

/proc/self/fd contains symlink presenting the opened FD of current process

e.g:

/proc/self/fd/

	0 -> /dev/stdin
	1 -> /dev/stdout
	2 -> /dev/stderr
	3 -> /backups/daily

There fore this allow access the folder or file by FD path "/proc/self/fd/3"
which is the same with open file from "/backups/daily"
*/
func fdPath(f *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", f.Fd())
}

func (s *storage) makeDirTarget(name string) (*os.File, error) {
	if err := unix.Mkdirat(int(s.root.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, diskError(err)
	}

	fd, err := unix.Openat(int(s.root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, diskError(err)
	}

	f := os.NewFile(uintptr(fd), name)
	if err := s.root.Sync(); err != nil {
		_ = f.Close()
		return nil, diskError(err)
	}

	return f, nil
}

func createFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, diskError(err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(fdPath(dir), name)), nil
}
