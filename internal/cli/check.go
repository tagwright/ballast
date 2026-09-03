// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/check"
	"github.com/tagwright/ballast/internal/hostid"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/record"
)

// newCheckCmd builds "ballast check <service>". version stamps the check
// record's ballast_version.
func newCheckCmd(version string) *cobra.Command {
	var (
		readData    bool
		jsonOut     bool
		requestedBy string
	)

	cmd := &cobra.Command{
		Use:   "check <service>",
		Short: "Check a repository's integrity",
		Long: `check runs an integrity check on <service>'s repository and records the
outcome as a ballast.check.v1 document, the machine-readable evidence that the
repository is internally consistent.

Two methods, a materially different claim:

  metadata   (default) restic check: walk the repository's structure and
             index and confirm every referenced pack and blob is present and
             internally consistent. It does NOT read the pack data, so it
             proves nothing about whether the stored bytes are intact.
  read-data  (--read-data) restic check --read-data: additionally read every
             pack and re-hash its data, catching bit rot and silent backend
             corruption the metadata pass cannot. It reads the whole
             repository and is the stronger, slower claim.

An integrity check is NOT a restore test: use "ballast verify" for the separate
evidence that a snapshot actually restores. The method is recorded on the
record so a metadata check is never mistaken for a data read downstream.

<service>'s container must be running now and discoverable via its ballast.*
(or tagwright.backup.*) labels. The check result is reflected in the exit
status (0 for pass, non-zero for fail or inconclusive). With --json the
ballast.check.v1 record is written to stdout (and, as always, to the state
directory).

--requested-by records the check as a remote request (trigger "remote") by the
named identity, the path a controller like Billet drives; left unset, the
record is the local manual-path record it is today.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()

			// A stable host identity is required: a check record keys its
			// evidence on host_id, so check refuses rather than emit a record
			// with an invalid host, exactly as verify does.
			hostID, herr := hostid.LoadOrCreate(d.Config.StateDir)
			if herr != nil {
				return fmt.Errorf("check needs a stable host identity: %w", herr)
			}

			ctx := cmd.Context()
			spec, err := discoverService(ctx, d.Runtime, d.Config, service)
			if err != nil {
				return fmt.Errorf("discover service %q: %w", service, err)
			}
			if spec == nil {
				return fmt.Errorf("service %q not found: no running container currently discoverable with ballast.enable=true and this service name", service)
			}

			repo, err := orchestrator.BuildRepo(spec, d.Config, d.Resolver, d.Master)
			if err != nil {
				return fmt.Errorf("build repo for %q: %w", service, err)
			}

			params := check.Params{
				Spec:        spec,
				HostID:      hostID,
				RuntimeName: firstNonEmptyCLI(d.Config.Runtime, "docker"),
				Version:     version,
				Trigger:     "manual",
				ReadData:    readData,
			}
			// --requested-by marks this invocation as a remote request (the
			// path Billet drives): the record's trigger becomes "remote" and
			// requested_by carries the caller's identity. Unset, the record is
			// exactly the manual-path record it is today.
			if cmd.Flags().Changed("requested-by") {
				params.Trigger = "remote"
				params.RequestedBy = &requestedBy
			}

			c := check.Run(ctx, d.Engine, repo, params)

			if _, err := record.WriteCheck(d.Config.StateDir, c); err != nil {
				// The record is evidence about the check; failing to write it is
				// worth surfacing, but the check itself already ran and its
				// verdict still governs the exit status below.
				d.Logger.Warn("check: write record failed", "service", service, "check_id", c.CheckID, "error", err)
			}

			if jsonOut {
				// In --json mode stdout is reserved for the record.
				data, err := record.MarshalCheck(c)
				if err != nil {
					return fmt.Errorf("marshal check record: %w", err)
				}
				if _, err := cmd.OutOrStdout().Write(data); err != nil {
					return err
				}
			} else {
				reason := ""
				if c.Reason != nil {
					reason = ": " + *c.Reason
				}
				fmt.Fprintf(cmd.OutOrStdout(), "check %s (%s): %s in %dms%s\n",
					service, c.Method, c.Result, c.DurationMs, reason)
			}

			if c.Result != "pass" {
				// A non-pass exits non-zero without a duplicate cobra usage dump.
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return fmt.Errorf("check %s did not pass: %s", service, c.Result)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&readData, "read-data", false, "read and re-hash every pack's data (restic check --read-data); default checks structure and metadata only")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the ballast.check.v1 record for the check on stdout")
	cmd.Flags().StringVar(&requestedBy, "requested-by", "", `record this check as remotely requested by the given identity (sets trigger "remote"); unset keeps the manual trigger`)
	return cmd
}
