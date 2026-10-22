package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/quoctan/backup-service/internal/config"
	"golang.org/x/sys/unix"
)

func pgpass(t config.Target) (string, func(), error) {
	// Follow Kubernetes Secret projection symlinks, but never block opening a
	// FIFO. Validate the opened descriptor rather than racing a pathname stat.
	fd, err := unix.Open(t.PasswordFile, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, errors.New("cannot read password file")
	}

	f := os.NewFile(uintptr(fd), t.PasswordFile)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return "", nil, errors.New("password file is not a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(f, 65537))
	_ = f.Close()
	if err != nil || len(data) > 65536 {
		return "", nil, errors.New("cannot read password file")
	}

	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if password == "" || strings.ContainsAny(password, "\x00\r\n") {
		return "", nil, errors.New("password file must contain one nonempty line")
	}

	dir, err := os.MkdirTemp("", "pg-backup-pass-")
	if err != nil {
		return "", nil, errors.New("cannot create temporary credential directory")
	}

	cleanupFn := func() { _ = os.RemoveAll(dir) }
	escape := strings.NewReplacer(`\`, `\\`, ":", `\:`)
	line := escape.Replace(t.Host) + ":" + strconv.Itoa(t.Port) + ":*:" + escape.Replace(t.Username) + ":" + escape.Replace(password) + "\n"
	path := filepath.Join(dir, "pgpass")

	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		cleanupFn()
		return "", nil, errors.New("cannot write temporary credentials")
	}

	return path, cleanupFn, nil
}

func commandEnv(t config.Target, pass string) []string {
	// Do not inherit PGPASSWORD, PGSERVICE, PGOPTIONS, connection settings or
	// loader variables from the pod. Only explicit TLS file settings carry over.
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8",
		"PGPASSFILE=" + pass,
		"PGHOST=" + t.Host,
		"PGPORT=" + strconv.Itoa(t.Port),
		"PGUSER=" + t.Username,
		"PGDATABASE=" + t.Databases[0],
		"PGSSLMODE=" + t.SSLMode,
		"PGCONNECT_TIMEOUT=15",
		"PGAPPNAME=backup-service",
	}
	for _, key := range []string{"PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLCRL", "PGSSLCRLDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (e *Engine) run(ctx context.Context, t config.Target, env []string, tool string, args []string, out io.Writer) error {
	path := fmt.Sprintf("/usr/lib/postgresql/%d/bin/%s", t.MajorVersion, tool)
	if err := e.runner.Run(ctx, Command{Path: path, Args: args, Env: env, Stdout: out}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed", tool)
	}
	return nil
}

func (e *Engine) checkVersion(ctx context.Context, t config.Target, env []string) error {
	out := &limitedBuffer{limit: 128}
	if err := e.run(ctx, t, env, "psql", []string{"--no-password", "--no-psqlrc", "--no-align", "--tuples-only", "--set=ON_ERROR_STOP=1", "--command=SHOW server_version_num"}, out); err != nil {
		return err
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(out.data)))
	if err != nil || n/10000 != t.MajorVersion {
		return errors.New("server major version does not match configured majorVersion")
	}

	return nil
}
