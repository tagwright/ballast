// SPDX-License-Identifier: GPL-3.0-or-later

// Package cli builds Ballast's Cobra command tree and is the CLI's only
// entry point: cmd/ballast/main.go calls Execute and does nothing else.
//
// Cobra is used specifically for two properties: it generates shell
// completion for every command and flag it knows about (the "completion"
// subcommand, added automatically), and it derives --help text straight
// from the command and flag definitions below, so help can never drift out
// of sync with the actual options the way a hand-rolled usage string can.
//
// The command tree:
//
//	ballast daemon              run the long-running service (the
//	                             container's default command)
//	ballast version              print the build version
//	ballast key <service>        print a service's repository password
//	                             (disaster recovery)
//	ballast snapshots [service]  list snapshots
//	ballast restore <service>    restore a snapshot
//	ballast backup <service>     force a backup now
//
// Every subcommand except "daemon" and "key" shares a small set of
// collaborators (config, secret resolver, Docker runtime, restic engine)
// built once by buildCommonDeps in deps.go, rather than each command
// re-deriving its own wiring.
package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// cfgFile and logLevel back the root command's persistent flags. Cobra
// commands are typically small, long-lived singletons, so package-level
// vars bound by pflag are the idiomatic way to thread persistent flags
// through to every subcommand's RunE without passing them explicitly.
var (
	cfgFile  string
	logLevel string
)

// Execute builds the command tree and runs it against os.Args. version is
// the build-time version string (see cmd/ballast/main.go), reported by
// both "ballast version" and the auto-generated "ballast --version" flag.
func Execute(version string) error {
	return newRootCmd(version).Execute()
}

// newRootCmd builds the root "ballast" command and attaches every
// subcommand. Cobra adds its own "completion" and "help" subcommands, and
// (because Version is set below) a "--version" flag, automatically: none
// of those are hand-rolled here.
func newRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "ballast",
		Short: "Ballast is a label-driven backup daemon for Docker Compose services.",
		Long: `Ballast watches a container runtime for services opted in via ballast.*
(or tagwright.backup.*) labels, and backs each one up on its own schedule
through a pluggable engine (restic first). The daemon is the normal way to
run it; the other subcommands cover disaster recovery and one-off runs.`,
		Version:      version,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("ballast {{.Version}}\n")

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "path to ballast.yml (default: none, env-only operation)")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newKeyCmd())
	root.AddCommand(newSnapshotsCmd())
	root.AddCommand(newRestoreCmd())
	root.AddCommand(newBackupCmd())

	return root
}

// newVersionCmd prints the same "ballast <version>" line the pre-Cobra CLI
// printed for "version", "--version", and "-v". The auto-generated
// "ballast --version" flag (see Version above) is templated to match it.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ballast version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "ballast %s\n", version)
			return nil
		},
	}
}

// newLogger builds a slog.Logger from the --log-level persistent flag,
// writing to stderr so stdout stays free for command output that's meant
// to be captured or piped (snapshot listings, restore confirmations, and
// especially "ballast key", whose whole contract is a clean stdout).
func newLogger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", logLevel, err)
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}
