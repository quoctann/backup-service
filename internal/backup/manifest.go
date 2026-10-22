package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

var setName = regexp.MustCompile(`^backup-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{16}$`)
var dumpName = regexp.MustCompile(`^db-[0-9]{3,}\.dump$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Manifest struct {
	SchemaVersion int         `json:"schemaVersion"`
	ID            string      `json:"id"`
	Target        string      `json:"target"`
	MajorVersion  int         `json:"majorVersion"`
	StartedAt     time.Time   `json:"startedAt"`
	CompletedAt   time.Time   `json:"completedAt"`
	Files         []FileEntry `json:"files"`
}

type FileEntry struct {
	Name     string `json:"name"`
	Database string `json:"database,omitempty"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

func regularFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("not a regular file")
	}

	return f, nil
}

func hashFile(ctx context.Context, dir *os.File, name, database string) (FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return FileEntry{}, err
	}

	f, err := regularFile(dir, name)
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()

	hash := sha256.New()
	// repeating partial copy from src to dst until EOF, 64KBs is the buffer for
	// this copy process, hash.Hash will update during this repeat, in the final
	// call the hash.Sum will return the entire file checksum
	n, err := io.CopyBuffer(hash, contextReader{ctx: ctx, reader: f}, make([]byte, 64*1024))
	if err != nil {
		return FileEntry{}, err
	}
	if n == 0 {
		return FileEntry{}, errors.New("empty backup output")
	}

	return FileEntry{
		Name:     name,
		Database: database,
		Size:     n,
		SHA256:   hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// contextReader checks cancellation between bounded reads. It deliberately
// exposes no WriterTo fast path that could bypass those checks for large files.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	return r.reader.Read(p)
}

func writeManifest(dir *os.File, m Manifest) error {
	f, err := createFile(dir, "manifest.json")
	if err != nil {
		return err
	}

	err = json.NewEncoder(f).Encode(m)
	if err == nil {
		err = f.Sync()
	}

	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

// readSuccessful recognizes only our complete-set format. Unknown directories,
// partial sets and symlinks are never counted or removed by retention.
func readSuccessful(ctx context.Context, parent *os.File, name, target string) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if !setName.MatchString(name) {
		return Manifest{}, errors.New("not a backup set")
	}

	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Manifest{}, err
	}

	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()

	f, err := regularFile(dir, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()

	var m Manifest
	dec := json.NewDecoder(io.LimitReader(f, 1024*1024)) // 1MB
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, err
	}

	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return m, errors.New("invalid manifest ending")
	}

	if m.SchemaVersion != 1 || m.ID != name || m.Target != target || m.CompletedAt.IsZero() || m.StartedAt.IsZero() || m.CompletedAt.Before(m.StartedAt) || len(m.Files) < 2 {
		return m, errors.New("invalid complete-set manifest")
	}
	if m.MajorVersion != 14 && m.MajorVersion != 16 && m.MajorVersion != 17 {
		return m, errors.New("invalid manifest version")
	}

	seen := make(map[string]bool)
	for _, entry := range m.Files {
		if (entry.Name != "globals.sql" && !dumpName.MatchString(entry.Name)) || seen[entry.Name] || entry.Size <= 0 || !hashPattern.MatchString(entry.SHA256) {
			return m, errors.New("invalid manifest entry")
		}
		seen[entry.Name] = true
		// Rehash even same-size archives before counting or pruning a set.
		// Reading roughly 14 retained daily sets is an intentional safety cost:
		// silent corruption must never displace healthy recovery history.
		actual, err := hashFile(ctx, dir, entry.Name, entry.Database)
		if err != nil {
			return m, err
		}
		if actual.Size != entry.Size || actual.SHA256 != entry.SHA256 {
			return m, errors.New("backup file integrity mismatch")
		}
	}
	if !seen["globals.sql"] {
		return m, errors.New("missing globals")
	}

	entries, err := os.ReadDir(fdPath(dir))
	if err != nil || len(entries) != len(m.Files)+1 {
		return m, errors.New("unexpected backup directory contents")
	}

	return m, nil
}

func retain(ctx context.Context, parent *os.File, target string, keep int, recentlyPublished string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := os.ReadDir(fdPath(parent))
	if err != nil {
		return diskError(err)
	}

	var successful []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}

		if _, err := readSuccessful(ctx, parent, entry.Name(), target); err == nil {
			successful = append(successful, entry.Name())
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	sort.Strings(successful)
	// Wall-clock corrections can put an older backup lexically after the new
	// set. Always keep the set this run just published, independent of clocks.
	ordered := make([]string, 0, len(successful))
	publishedFound := false
	for _, name := range successful {
		if name == recentlyPublished {
			// pop the recently backup and append to the end of list, ensure the
			// retention not delete the latest
			publishedFound = true
			continue
		}
		ordered = append(ordered, name)
	}
	if publishedFound {
		ordered = append(ordered, recentlyPublished)
	}
	successful = ordered

	for len(successful) > keep {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := successful[0]
		successful = successful[1:]

		if _, err := readSuccessful(ctx, parent, name, target); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return diskError(fmt.Errorf("retention candidate changed: %w", err))
		}

		if err := os.RemoveAll(filepath.Join(fdPath(parent), name)); err != nil {
			return diskError(err)
		}

		if err := parent.Sync(); err != nil {
			return diskError(err)
		}
	}

	return ctx.Err()
}
