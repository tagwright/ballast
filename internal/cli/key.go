// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/secret"
)

// newKeyCmd builds "ballast key <service>". Deliberately independent of
// buildCommonDeps: it must keep working with only the master secret
// available, with no Docker socket and no running container at all, since
// that is exactly the situation a disaster-recovery run is in.
func newKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key <service>",
		Short: "Print a service's repository password (disaster recovery)",
		Long: `key derives and prints <service>'s restic repository password to stdout,
and nothing else.

This is a DISASTER RECOVERY command. It needs only the master secret
(repo-master-key, resolved the same way the daemon resolves it via
--config/BALLAST_SECRETS_DIR) and works with no Docker socket and no
running container: exactly the situation you're in once the compose stack
that used to run "ballast daemon" for this service is gone.

The printed value is a restic repository password: treat it with the same
care you would any other credential. It is what "restic -r <repo>
snapshots" (or any other restic command run by hand) needs as
RESTIC_PASSWORD or RESTIC_PASSWORD_FILE to open the service's repository
directly. Avoid leaving it in shell history or an unencrypted log; prefer
piping straight into RESTIC_PASSWORD_FILE or a password manager over
pasting it somewhere it will linger.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolver := secret.FileEnvResolver(cfg.SecretsDir)

			master, err := secret.LoadMaster(resolver)
			if err != nil {
				return fmt.Errorf("load master secret: %w", err)
			}

			password, err := secret.DeriveRepoPassword(master, service)
			if err != nil {
				return fmt.Errorf("derive repo password: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), password)
			return nil
		},
	}
}
