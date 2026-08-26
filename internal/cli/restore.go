// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/engine"
)

// newRestoreCmd builds "ballast restore <service>".
func newRestoreCmd() *cobra.Command {
	var (
		snapshotID  string
		target      string
		include     []string
		destination string
		repoPath    string
	)

	cmd := &cobra.Command{
		Use:   "restore <service>",
		Short: "Restore a snapshot",
		Long: `restore restores one snapshot of <service>'s repository into --target.

The repository is resolved the same way "ballast snapshots" resolves it: if
the service's container is currently discoverable via its ballast.* labels,
its own spec builds the repository, exactly as a scheduled backup would.
Otherwise --destination (and --repo-path, if it differs from the service
name) build the repository directly, so this also works for disaster
recovery once the container, and its labels, are gone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()

			ctx := cmd.Context()
			repo, err := resolveRepo(ctx, d, service, destination, repoPath)
			if err != nil {
				return err
			}

			req := engine.RestoreRequest{
				Repo:       repo,
				SnapshotID: snapshotID,
				Target:     target,
				Include:    include,
			}
			if err := d.Engine.Restore(ctx, req); err != nil {
				return fmt.Errorf("restore %q: %w", service, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "restored %s (snapshot %s) to %s\n", service, snapshotID, target)
			return nil
		},
	}

	cmd.Flags().StringVar(&snapshotID, "snapshot", "latest", `snapshot ID to restore, or "latest"`)
	cmd.Flags().StringVar(&target, "target", "", "destination directory to restore into (required)")
	cmd.Flags().StringArrayVar(&include, "include", nil, "restore only paths matching this pattern (repeatable)")
	cmd.Flags().StringVar(&destination, "destination", "", "named destination (disaster recovery: used when the service's container can't be discovered)")
	cmd.Flags().StringVar(&repoPath, "repo-path", "", "repository sub-path (disaster recovery; defaults to the service name)")
	_ = cmd.MarkFlagRequired("target")

	return cmd
}
