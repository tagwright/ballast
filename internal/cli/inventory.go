// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/hostid"
)

// inventoryRecordType is the discriminator for the inventory document, mirrored
// by the ballast.inventory.v1 prose in docs/RECORDS.md.
const inventoryRecordType = "ballast.inventory.v1"

// inventoryRecord is the ballast.inventory.v1 document: the discovered service
// inventory a controller (the Billet agent) reads over the process boundary for
// its heartbeat. It is built from exactly the same discovery pass the daemon
// drives, so it reflects what Ballast would actually back up and verify.
type inventoryRecord struct {
	Record      string             `json:"record"`
	HostID      string             `json:"host_id"`
	GeneratedAt string             `json:"generated_at"`
	Services    []inventoryService `json:"services"`
}

// inventoryService is one discovered service's entry in an inventoryRecord.
// The fields are the daemon's own resolved view of the service, not a re-read
// of raw labels: runtime and runtime_ref locate the container, repo_id is where
// its backups land, and the verify_configured/probe_declared pair reports
// whether the service is set up to be proven restorable.
type inventoryService struct {
	Name             string            `json:"name"`
	Runtime          string            `json:"runtime"`
	RuntimeRef       map[string]string `json:"runtime_ref"`
	Enabled          bool              `json:"enabled"`
	RepoID           string            `json:"repo_id"`
	VerifyConfigured bool              `json:"verify_configured"`
	BackupSchedule   *string           `json:"backup_schedule"`
	ProbeDeclared    bool              `json:"probe_declared"`
}

// newInventoryCmd builds "ballast inventory".
func newInventoryCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "List the discovered service inventory",
		Long: `inventory reports every service currently discoverable and enabled via its
ballast.* (or tagwright.backup.*) labels, using the same discovery and label
semantics the daemon uses, so the inventory matches what Ballast would actually
back up and verify.

With --json a single ballast.inventory.v1 document is written to stdout: the
host_id, a generation timestamp, and one entry per service carrying its runtime
locator, repository id, and whether verify (and a probe) are configured. It is
the machine-readable view a controller like the Billet agent reads over a
process boundary for its heartbeat. Without --json a short human-readable table
is printed instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := buildCommonDeps(cfgFile)
			if err != nil {
				return err
			}
			defer func() { _ = d.Runtime.Close() }()

			// A stable host identity is required: the inventory record keys its
			// heartbeat on host_id, so inventory refuses rather than emit a
			// record with no valid host, exactly as verify does.
			hostID, herr := hostid.LoadOrCreate(d.Config.StateDir)
			if herr != nil {
				return fmt.Errorf("inventory needs a stable host identity: %w", herr)
			}

			ctx := cmd.Context()
			specs, err := discoverAllServices(ctx, d.Runtime, d.Config)
			if err != nil {
				return err
			}

			runtimeName := firstNonEmptyCLI(d.Config.Runtime, "docker")
			inv := buildInventory(specs, hostID, runtimeName, time.Now())

			out := cmd.OutOrStdout()
			if jsonOut {
				data, err := json.MarshalIndent(inv, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal inventory: %w", err)
				}
				data = append(data, '\n')
				_, err = out.Write(data)
				return err
			}
			return printInventory(out, inv)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the ballast.inventory.v1 record on stdout")
	return cmd
}

// buildInventory turns a discovery pass into a ballast.inventory.v1 record. It
// takes the already-discovered specs (every one is enabled: discovery yields no
// spec for a container that is not opted in), the host identity the record is
// keyed on, the runtime engine name, and the generation time. Pure and
// socket-free so it is unit-testable without a live runtime; the command wires
// the real discovery, host id, and runtime name into it.
func buildInventory(specs []*discovery.BackupSpec, hostID, runtimeName string, now time.Time) inventoryRecord {
	services := make([]inventoryService, 0, len(specs))
	for _, spec := range specs {
		services = append(services, inventoryServiceFromSpec(spec, runtimeName))
	}
	// A stable, name-sorted order so the same host yields the same document
	// across heartbeats regardless of the runtime's container listing order.
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	return inventoryRecord{
		Record:      inventoryRecordType,
		HostID:      hostID,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Services:    services,
	}
}

// inventoryServiceFromSpec projects one resolved BackupSpec onto its inventory
// entry, reusing the daemon's own resolved view: it never re-reads labels.
func inventoryServiceFromSpec(spec *discovery.BackupSpec, runtimeName string) inventoryService {
	ref := map[string]string{
		"container_name": spec.ContainerName,
		"container_id":   spec.ContainerID,
	}
	if spec.Project != "" {
		ref["compose_project"] = spec.Project
	}

	// spec.Schedule is the raw ballast.schedule label: empty means the global
	// default applies, which the record reports as an explicit null rather than
	// an empty string, so a reader never confuses "unset" with a schedule of "".
	var schedule *string
	if spec.Schedule != "" {
		s := spec.Schedule
		schedule = &s
	}

	return inventoryService{
		Name:             spec.Service,
		Runtime:          runtimeName,
		RuntimeRef:       ref,
		Enabled:          true,
		RepoID:           spec.Destination + ":" + spec.RepoPath,
		VerifyConfigured: spec.VerifyConfigured,
		BackupSchedule:   schedule,
		ProbeDeclared:    spec.Verify.Probe != "",
	}
}

// printInventory renders the human-readable inventory table.
func printInventory(w io.Writer, inv inventoryRecord) error {
	if len(inv.Services) == 0 {
		_, err := fmt.Fprintln(w, "no enabled services discovered")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tRUNTIME\tREPO\tVERIFY\tPROBE\tSCHEDULE")
	for _, s := range inv.Services {
		schedule := "(default)"
		if s.BackupSchedule != nil {
			schedule = *s.BackupSchedule
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%s\n",
			s.Name, s.Runtime, s.RepoID, s.VerifyConfigured, s.ProbeDeclared, schedule)
	}
	return tw.Flush()
}
