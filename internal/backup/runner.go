package backup

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

type Command struct {
	Path   string
	Args   []string
	Env    []string
	Stdout io.Writer
}

type IRunner interface {
	Run(context.Context, Command) error
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, c Command) error {
	// No shell is involved: argv boundaries cannot become shell syntax. We
	// intentionally drop stderr: server errors can contain connection details,
	// SQL or secrets. Return only the utility name at the orchestration layer.
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Stdout = c.Stdout
	cmd.Stderr = io.Discard
	// CommandContext kills the child on cancellation; WaitDelay also bounds
	// waits on inherited pipes if an unexpected descendant keeps them open.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("PostgreSQL utility failed")
	}

	return nil
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, errors.New("utility output exceeded limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
