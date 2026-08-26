// SPDX-License-Identifier: GPL-3.0-or-later

// Package discovery turns a container's Ballast labels into a validated
// per-service BackupSpec: the label reader and the volume/service discovery
// logic the orchestrator drives everything else from.
//
// It recognizes two label prefixes, "ballast." (primary) and
// "tagwright.backup." (org-namespaced alias), holding one internal grammar
// with two accepted spellings on the outside. It reads no state, executes no
// commands, and touches no repository: it is pure translation from a
// runtime.Container plus a config.Config into a BackupSpec (or a reason the
// service was skipped). See the Ballast Label Grammar for the full contract
// this package implements.
package discovery

import (
	"fmt"
	"strings"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/pkg/runtime"
)

// Discover reads c's labels against cfg and returns the service's
// BackupSpec.
//
// A nil spec with a nil error means the container is not opted in
// (ballast.enable absent or "false"): this is the normal, silent case, not a
// failure. A non-nil error means the service was skipped for a validation
// reason (a prefix conflict, a malformed label value, an incompatible
// combination such as stop+streams, or a primitive used without its global
// gate) and should be surfaced to the caller so it can alert. Warnings are
// non-fatal notices, most commonly a bind or volume mount whose host source
// does not resolve under any configured BALLAST_HOST_ROOTS entry; they are
// returned even alongside a validation error so the caller has as much
// context as possible.
func Discover(c runtime.Container, cfg *config.Config) (*BackupSpec, []string, error) {
	norm, err := normalizeLabels(c.Labels)
	if err != nil {
		return nil, nil, err
	}

	enabled, err := parseBool(norm, "enable", false)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return nil, nil, nil
	}

	service := resolveServiceName(c, norm)

	spec := &BackupSpec{
		Service:        service,
		Project:        c.Project,
		ContainerID:    c.ID,
		ContainerName:  c.Name,
		Destination:    firstNonEmpty(norm["repo"], cfg.DefaultDestination),
		RepoPath:       firstNonEmpty(norm["repo.path"], service),
		PasswordSecret: norm["password-secret"],
		Schedule:       norm["schedule"],
	}

	if spec.Retention, err = parseRetention(norm); err != nil {
		return nil, nil, err
	}
	if spec.Excludes, err = parseExcludes(norm); err != nil {
		return nil, nil, err
	}
	if spec.ExcludeCaches, err = parseBool(norm, "exclude-caches", true); err != nil {
		return nil, nil, err
	}
	spec.Tags = splitCSV(norm["tags"])

	if spec.NotifySuppress, err = parseBool(norm, "notify.suppress", false); err != nil {
		return nil, nil, err
	}
	if spec.NotifyOnSuccess, err = parseBool(norm, "notify.on-success", false); err != nil {
		return nil, nil, err
	}

	if spec.Stop, err = parseBool(norm, "stop", false); err != nil {
		return nil, nil, err
	}
	if spec.Streams, err = parseStreams(norm); err != nil {
		return nil, nil, err
	}
	if spec.ExecPre, err = parseHook(norm, "pre"); err != nil {
		return nil, nil, err
	}
	if spec.ExecPost, err = parseHook(norm, "post"); err != nil {
		return nil, nil, err
	}

	var volWarnings []string
	spec.Paths, volWarnings = resolveVolumes(c, norm, cfg)

	var warnings []string
	for _, w := range volWarnings {
		warnings = append(warnings, fmt.Sprintf("%s: %s", service, w))
	}

	if err := validate(spec, cfg); err != nil {
		return nil, warnings, err
	}

	return spec, warnings, nil
}

// resolveServiceName applies the service-identity precedence: ballast.name,
// then the compose service label, then the container name with any leading
// "/" stripped (the runtime adapters already strip it, but discovery does
// not depend on that).
func resolveServiceName(c runtime.Container, norm map[string]string) string {
	if v := norm["name"]; v != "" {
		return v
	}
	if c.Service != "" {
		return c.Service
	}
	return strings.TrimPrefix(c.Name, "/")
}

// firstNonEmpty returns a if it is non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// validate enforces the grammar's cross-field rules that discovery, not the
// orchestrator, is responsible for rejecting up front.
func validate(spec *BackupSpec, cfg *config.Config) error {
	if spec.Stop && len(spec.Streams) > 0 {
		return fmt.Errorf("discovery: service %q: stop=true is incompatible with stream backups", spec.Service)
	}
	if (len(spec.Streams) > 0 || spec.ExecPre != nil || spec.ExecPost != nil) && !cfg.EnableExec {
		return fmt.Errorf("discovery: service %q: exec hooks and stream backups require BALLAST_ENABLE_EXEC=true", spec.Service)
	}
	if spec.Stop && !cfg.EnableStop {
		return fmt.Errorf("discovery: service %q: stop=true requires BALLAST_ENABLE_STOP=true", spec.Service)
	}
	return nil
}
