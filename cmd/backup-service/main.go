package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/quoctan/backup-service/internal/backup"
	"github.com/quoctan/backup-service/internal/cli"
	"go.uber.org/zap"
)

func main() {
	os.Exit(run())
}

func run() int {
	log, err := zap.NewProduction()
	if err != nil {
		return 1
	}
	defer func() { _ = log.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.New(backup.New(backup.ExecRunner{}, log)).ExecuteContext(ctx); err != nil {
		log.Error("backup-service failed", zap.Error(err))
		return 1
	}

	return 0
}
