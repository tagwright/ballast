// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/orchestrator"
)

// newSnapshotsCmd builds "ballast snapshots [service]".
func newSnapshotsCmd() *cobra.Command {
	var destination, repoPath string

	cmd := &cobra.Command{
		Use:   "snapshots [service]",
		Short: "List snapshots",
		Long: `snapshots lists restic snapshots for one service, or for every currently
discoverable enabled service if no service is named.

The repository is resolved the same way "ballast restore" resolves it: if
the named service's container is currently discoverable via its ballast.*
labels, its own spec builds the repository, exactly as a scheduled backup
would. Otherwise --destination (and --repo-path, if it differs from the
service name) build the repository directly, so this also works for
disaster recovery once the container, and its labels, are gone. With no
service argument, only currently discoverable services are listed: there is
no label-free way to enumerate services that no longer exist.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()

			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				service := args[0]
				repo, err := resolveRepo(ctx, d, service, destination, repoPath)
				if err != nil {
					return err
				}
				snaps, err := d.Engine.Snapshots(ctx, repo)
				if err != nil {
					return fmt.Errorf("list snapshots for %q: %w", service, err)
				}
				return printSnapshots(out, map[string][]engine.Snapshot{service: snaps})
			}

			specs, err := discoverAllServices(ctx, d.Runtime, d.Config)
			if err != nil {
				return err
			}
			if len(specs) == 0 {
				fmt.Fprintln(out, "no enabled services discovered")
				return nil
			}

			byService := make(map[string][]engine.Snapshot, len(specs))
			for _, spec := range specs {
				repo, err := orchestrator.BuildRepo(spec, d.Config, d.Resolver, d.Master)
				if err != nil {
					return fmt.Errorf("build repo for %q: %w", spec.Service, err)
				}
				snaps, err := d.Engine.Snapshots(ctx, repo)
				if err != nil {
					return fmt.Errorf("list snapshots for %q: %w", spec.Service, err)
				}
				byService[spec.Service] = snaps
			}
			return printSnapshots(out, byService)
		},
	}

	cmd.Flags().StringVar(&destination, "destination", "", "named destination (disaster recovery: used when the service's container can't be discovered)")
	cmd.Flags().StringVar(&repoPath, "repo-path", "", "repository sub-path (disaster recovery; defaults to the service name)")

	return cmd
}

// printSnapshots renders a readable id/time/host/tags/paths table across
// every service in byService, sorted by service name and then by snapshot
// time.
func printSnapshots(w io.Writer, byService map[string][]engine.Snapshot) error {
	services := make([]string, 0, len(byService))
	for service := range byService {
		services = append(services, service)
	}
	sort.Strings(services)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tID\tTIME\tHOST\tTAGS\tPATHS")

	for _, service := range services {
		snaps := byService[service]
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })

		for _, s := range snaps {
			id := s.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				service, id, s.Time.Format(time.RFC3339), s.Host,
				strings.Join(s.Tags, ","), strings.Join(s.Paths, ","))
		}
	}

	return tw.Flush()
}
