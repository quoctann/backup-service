package cli

import (
	"context"
	"fmt"

	"github.com/quoctan/backup-service/internal/config"
	"github.com/spf13/cobra"
)

type IBackup interface {
	Backup(context.Context, config.Config) error
}

func New(engine IBackup) *cobra.Command {
	root := &cobra.Command{
		Use:           "backup-service",
		Short:         "Complete-set PostgreSQL logical backups",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	var path string
	root.PersistentFlags().StringVar(&path, "config", "", "Path to strict YAML configuration (required)")
	_ = root.MarkPersistentFlagRequired("config")

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration without connecting or writing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(path); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "configuration valid")
			return err
		},
	}

	backup := &cobra.Command{
		Use:   "backup",
		Short: "Back up all configured targets sequentially",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			return engine.Backup(cmd.Context(), cfg)
		},
	}

	root.AddCommand(validate, backup)
	return root
}
