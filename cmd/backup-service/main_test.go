package main

import (
	"os"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"backup-service", "--help"}, 0},
		{[]string{"backup-service", "backup"}, 1},
		{[]string{"backup-service", "validate", "--config", "/nonexistent"}, 1},
	} {
		os.Args = tc.args
		if got := run(); got != tc.want {
			t.Fatalf("%v: exit = %d, want %d", tc.args, got, tc.want)
		}
	}
}
