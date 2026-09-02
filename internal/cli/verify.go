// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/hostid"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/verify"
	"github.com/tagwright/core/runtime"
)

// newVerifyCmd builds "ballast verify <service>". version stamps the verify
// record's ballast_version.
func newVerifyCmd(version string) *cobra.Command {
	var (
		snapshot    string
		jsonOut     bool
		timeout     time.Duration
		requestedBy string
	)

	cmd := &cobra.Command{
		Use:   "verify <service>",
		Short: "Prove a snapshot restores",
		Long: `verify restores one snapshot of <service> to a throwaway location, runs the
service's probe against the restored copy, and records the outcome as a
ballast.verify.v1 document, the machine-readable proof a backup is restorable.

The mechanism is chosen by the service's verify.mode label:

  files           restore to a scratch directory and diff the restored tree
                  against the backup-time manifest
  container       restore volume data into fresh scratch volumes, boot a
                  throwaway copy of the image on an isolated network, probe it
  stream-restore  restore a dump to scratch, boot a throwaway container on an
                  isolated network, pipe the dump into it, probe it

The live service and its real volumes are never touched, and any throwaway
container is always placed on an isolated (internal) network. The scratch is
destroyed on every exit path, including failure and timeout.

<service>'s container must be running now and discoverable via its ballast.*
(or tagwright.backup.*) labels: verify reads the service's image and volume
layout from it. The verify result is reflected in the exit status (0 for pass,
non-zero for fail or inconclusive). With --json the ballast.verify.v1 record is
written to stdout (and, as always, to the state directory).

--timeout overrides the verify.timeout label for this one invocation, bounding
the same whole-operation wall clock. --requested-by records the verify as a
remote request (trigger "remote") by the named identity, the path a controller
like Billet drives; left unset, the record is exactly the local manual-path
record it is today.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()

			// A stable host identity is required: a verify record keys its
			// evidence on host_id, so verify refuses rather than emit a record
			// with an invalid host.
			hostID, herr := hostid.LoadOrCreate(d.Config.StateDir)
			if herr != nil {
				return fmt.Errorf("verify needs a stable host identity: %w", herr)
			}

			ctx := cmd.Context()
			containers, err := d.Runtime.List(ctx)
			if err != nil {
				return fmt.Errorf("list containers: %w", err)
			}

			var spec *discovery.BackupSpec
			var container runtime.Container
			for _, c := range containers {
				s, _, derr := discovery.Discover(c, d.Config)
				if s == nil || s.Service != service {
					continue
				}
				if derr != nil {
					return fmt.Errorf("service %q was discovered but is not valid: %w", service, derr)
				}
				spec = s
				container = c
				break
			}
			if spec == nil {
				return fmt.Errorf("service %q not found: no running container currently discoverable with ballast.enable=true and this service name", service)
			}

			repo, err := orchestrator.BuildRepo(spec, d.Config, d.Resolver, d.Master)
			if err != nil {
				return fmt.Errorf("build repo for %q: %w", service, err)
			}

			deps := verify.Deps{
				Runtime:     d.Runtime,
				Engine:      d.Engine,
				Repo:        repo,
				Logger:      d.Logger,
				StateDir:    d.Config.StateDir,
				HostID:      hostID,
				Version:     version,
				RuntimeName: firstNonEmptyCLI(d.Config.Runtime, "docker"),
				Trigger:     "manual",
				JSON:        jsonOut,
				Stdout:      cmd.OutOrStdout(),
			}

			// BALLAST_VERIFY_NAME_PREFIX namespaces every throwaway object a
			// verify creates (the scratch container, its isolated network, any
			// scratch volumes). Unset, it stays the "ballast-verify" default. A
			// controller like Billet sets it so the throwaways it drives are
			// scoped to its own naming (Billet Product Spec section 10 names
			// them billet-verify-*), and an operator can tell controller-driven
			// throwaways apart from a local ballast verify on a busy host.
			if p := os.Getenv("BALLAST_VERIFY_NAME_PREFIX"); p != "" {
				deps.NamePrefix = p
			}

			// --requested-by marks this invocation as a remote request (the
			// path Billet drives): the record's trigger becomes "remote" and
			// requested_by carries the caller's identity. Unset, the record is
			// exactly the manual-path record it is today.
			if cmd.Flags().Changed("requested-by") {
				deps.Trigger = "remote"
				deps.RequestedBy = &requestedBy
			}
			// --timeout overrides the verify.timeout label for this one run,
			// bounding the same whole-operation wall clock. Unset, the label
			// value (or its 10m default) stands unchanged.
			if cmd.Flags().Changed("timeout") {
				deps.TimeoutOverride = &timeout
			}

			v, err := verify.Run(ctx, spec, container, snapshot, deps)
			if err != nil {
				return fmt.Errorf("verify %q: %w", service, err)
			}

			// In --json mode stdout is reserved for the record; the human
			// summary is suppressed rather than appended after the JSON.
			if !jsonOut {
				reason := ""
				if v.Reason != nil {
					reason = ": " + *v.Reason
				}
				fmt.Fprintf(cmd.OutOrStdout(), "verify %s: %s%s\n", service, v.Result, reason)
			}

			if v.Result != "pass" {
				// A non-pass exits non-zero without a duplicate cobra usage dump.
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return fmt.Errorf("verify %s did not pass: %s", service, v.Result)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&snapshot, "snapshot", "latest", `snapshot ID to verify, or "latest"`)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the ballast.verify.v1 record for the verify on stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "override the verify.timeout label for this run (Go duration); unset uses the label value or the 10m default")
	cmd.Flags().StringVar(&requestedBy, "requested-by", "", `record this verify as remotely requested by the given identity (sets trigger "remote"); unset keeps the manual trigger`)
	return cmd
}

// firstNonEmptyCLI returns a if non-empty, else b.
func firstNonEmptyCLI(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
