// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/hostid"
	"github.com/tagwright/ballast/internal/orchestrator"
)

// newBackupCmd builds "ballast backup <service>". version stamps the run
// record's ballast_version.
func newBackupCmd(version string) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "backup <service>",
		Short: "Force a backup now",
		Long: `backup runs <service>'s full backup lifecycle immediately, exactly as the
scheduler would run it: pre-hook, optional container stop, filesystem and
stream backups, container restart, retention, post-hook, and an outcome
report through the same notification channels a scheduled run uses.

<service>'s container must be running right now and discoverable via its
ballast.* (or tagwright.backup.*) labels, with ballast.enable=true: this
command has no disaster-recovery fallback, unlike "snapshots" and
"restore", because there is nothing to back up once the container is gone.

With --json the ballast.run.v1 record for the run is written to stdout (and,
as always, to the state directory).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()
			if err := d.withNotifier(); err != nil {
				return err
			}

			// The stable host identity gates run-record writing: without it a
			// record would carry no valid host_id, so recording is left off
			// rather than writing an invalid record. The backup itself runs
			// regardless.
			hostID, herr := hostid.LoadOrCreate(d.Config.StateDir)
			recordStateDir := d.Config.StateDir
			if herr != nil {
				d.Logger.Warn("host identity unavailable; run record disabled", "error", herr)
				recordStateDir = ""
			}

			ctx := cmd.Context()
			containers, err := d.Runtime.List(ctx)
			if err != nil {
				return fmt.Errorf("list containers: %w", err)
			}

			var spec *discovery.BackupSpec
			for _, c := range containers {
				s, _, derr := discovery.Discover(c, d.Config)
				if s == nil || s.Service != service {
					continue
				}
				if derr != nil {
					return fmt.Errorf("service %q was discovered but is not valid for backup: %w", service, derr)
				}
				spec = s
				break
			}
			if spec == nil {
				return fmt.Errorf("service %q not found: no running container currently discoverable with ballast.enable=true and this service name", service)
			}

			deps := orchestrator.Deps{
				Runtime:  d.Runtime,
				Engine:   d.Engine,
				Config:   d.Config,
				Resolver: d.Resolver,
				Master:   d.Master,
				Notifier: d.Notifier,
				Logger:   d.Logger,
				StateDir: recordStateDir,
				HostID:   hostID,
				Version:  version,
				Trigger:  "manual",
				JSON:     jsonOut,
				Stdout:   cmd.OutOrStdout(),
			}

			if err := orchestrator.RunBackup(ctx, spec, deps); err != nil {
				return fmt.Errorf("backup %q: %w", service, err)
			}

			// In --json mode stdout is reserved for the run record, so the
			// human completion line is suppressed rather than appended after
			// the JSON document.
			if !jsonOut {
				fmt.Fprintf(cmd.OutOrStdout(), "backup complete: %s\n", service)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the ballast.run.v1 record for the run on stdout")
	return cmd
}
