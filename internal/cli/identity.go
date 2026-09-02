// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/hostid"
)

// newIdentityCmd builds "ballast identity", which prints the daemon's stable
// host identity, generating and persisting it on first run. The whole
// contract of this command is a clean stdout carrying exactly the host_id
// and a trailing newline, so it can be captured by a script or an operator
// enrolling the host into the fleet plane.
func newIdentityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "identity",
		Short: "Print this host's stable Ballast identity (host_id)",
		Long: `identity prints the stable host identity Ballast keys its records on. It
is generated once from a CSPRNG on first run, persisted in the state
directory (state_dir, default /var/lib/ballast), and returned unchanged on
every subsequent run so it survives container recreation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			id, err := hostid.LoadOrCreate(cfg.StateDir)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
}
