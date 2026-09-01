// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/daemon"
)

// newDaemonCmd builds "ballast daemon", the long-running service. It is
// the container's default command: daemon.Run does all of its own wiring
// (config, secrets, notifier, runtime, engine, scheduler), so this command
// only needs to build the logger, install a signal-driven context, and
// call it. version is threaded through so the daemon can stamp run records
// with ballast_version.
func newDaemonCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the Ballast backup daemon",
		Long: `daemon runs Ballast's long-running service: it loads config, discovers
containers opted in via their ballast.* (or tagwright.backup.*) labels,
watches the container runtime for lifecycle changes, and drives each
service's backup schedule until it receives SIGINT or SIGTERM.

This is the container's default command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newLogger()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return daemon.Run(ctx, cfgFile, version, logger)
		},
	}
}
