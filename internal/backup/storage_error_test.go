package backup

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestDiskErrorDoesNotClassifyTargetTimeoutAsSharedFailure(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			err := diskError(fmt.Errorf("hash interrupted: %w", cause))
			var shared *storageError
			if errors.As(err, &shared) {
				t.Fatal("a target timeout must not prevent remaining targets from backing up")
			}
			if !errors.Is(err, cause) {
				t.Fatal("cancellation cause lost")
			}
		})
	}
	var shared *storageError
	if !errors.As(diskError(syscall.ENOSPC), &shared) {
		t.Fatal("an actual disk failure must still stop remaining targets")
	}
}
