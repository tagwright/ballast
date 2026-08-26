// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/pkg/runtime"
)

// localtimeClassPaths are the noise binds auto-discovery filters out even
// without a BALLAST_DISCOVER_EXCLUDE entry: the classic
// "-v /etc/localtime:/etc/localtime:ro" pattern that carries no backupable
// service data.
var localtimeClassPaths = []string{"/etc/localtime", "/etc/timezone"}

// isSocketMount reports whether m looks like a Unix domain socket bind
// (the Docker-socket-into-container pattern, among others), which is never
// something Ballast should try to read as file data.
func isSocketMount(m runtime.Mount) bool {
	return strings.HasSuffix(m.Source, ".sock") || strings.HasSuffix(m.Destination, ".sock")
}

// isLocaltimeClass reports whether m is one of the well-known timezone
// bind-mount patterns.
func isLocaltimeClass(m runtime.Mount) bool {
	for _, p := range localtimeClassPaths {
		if m.Source == p || m.Destination == p {
			return true
		}
	}
	return false
}

// matchesAnyGlob reports whether any candidate (skipping empty ones) matches
// any pattern, tried both against the full candidate and against its base
// name so a pattern like "*.log" can match a destination path.
func matchesAnyGlob(patterns []string, candidates ...string) bool {
	for _, pat := range patterns {
		for _, c := range candidates {
			if c == "" {
				continue
			}
			if ok, err := filepath.Match(pat, c); err == nil && ok {
				return true
			}
			if ok, err := filepath.Match(pat, filepath.Base(c)); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// isEligibleMount reports whether m is a candidate for filesystem
// auto-discovery at all: named volumes and bind mounts are eligible, tmpfs,
// sockets, localtime-class binds, and anything matching discoverExclude are
// not. Read-only mounts are still eligible; per Fork 1 the read/write bit
// does not affect whether Ballast can read the data.
func isEligibleMount(m runtime.Mount, discoverExclude []string) bool {
	if m.Type == runtime.MountTmpfs {
		return false
	}
	if isSocketMount(m) || isLocaltimeClass(m) {
		return false
	}
	return !matchesAnyGlob(discoverExclude, m.Name, m.Source, m.Destination)
}

// matchesToken reports whether a ballast.volumes / ballast.volumes.exclude
// token selects m. A bare word matches a named volume's Name, a token with a
// leading "/" matches the container-side Destination.
func matchesToken(token string, m runtime.Mount) bool {
	if strings.HasPrefix(token, "/") {
		return m.Destination == token
	}
	return m.Name != "" && m.Name == token
}

// selectByTokens narrows mounts to those matching at least one token.
func selectByTokens(mounts []runtime.Mount, tokens []string) []runtime.Mount {
	out := make([]runtime.Mount, 0, len(mounts))
	for _, m := range mounts {
		for _, t := range tokens {
			if matchesToken(t, m) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// excludeByTokens drops mounts matching any token from mounts.
func excludeByTokens(mounts []runtime.Mount, tokens []string) []runtime.Mount {
	if len(tokens) == 0 {
		return mounts
	}
	out := make([]runtime.Mount, 0, len(mounts))
	for _, m := range mounts {
		excluded := false
		for _, t := range tokens {
			if matchesToken(t, m) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, m)
		}
	}
	return out
}

// translateHostPath maps a mount's host-side Source to the path Ballast can
// actually read, via the longest matching prefix in hostRoots. It reports
// false if source resolves under none of them.
func translateHostPath(source string, hostRoots map[string]string) (string, bool) {
	if source == "" {
		return "", false
	}
	clean := filepath.Clean(source)

	bestPrefix, bestTarget := "", ""
	for prefix, target := range hostRoots {
		cp := filepath.Clean(prefix)
		if clean != cp && !strings.HasPrefix(clean, cp+string(filepath.Separator)) {
			continue
		}
		if len(cp) > len(bestPrefix) {
			bestPrefix, bestTarget = cp, target
		}
	}
	if bestPrefix == "" {
		return "", false
	}

	rel := strings.TrimPrefix(clean, bestPrefix)
	return filepath.Join(bestTarget, rel), true
}

// mountLabel names a mount for a warning message: its volume name if it has
// one, otherwise its container-side destination.
func mountLabel(m runtime.Mount) string {
	if m.Name != "" {
		return m.Name
	}
	return m.Destination
}

// resolveVolumes turns a container's mounts into the host-visible paths
// Ballast should back up, applying eligibility filtering, the
// ballast.volumes / ballast.volumes.exclude narrowing, and the
// config.HostRoots translation. Mounts that cannot be translated are skipped
// with a warning rather than failing discovery outright, matching the
// grammar's WARN-and-continue contract.
func resolveVolumes(c runtime.Container, norm map[string]string, cfg *config.Config) (paths []string, warnings []string) {
	volumesLabel, hasVolumes := norm["volumes"]
	if hasVolumes && strings.TrimSpace(volumesLabel) == "none" {
		return nil, nil
	}

	eligible := make([]runtime.Mount, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		if isEligibleMount(m, cfg.DiscoverExclude) {
			eligible = append(eligible, m)
		}
	}

	selected := eligible
	if hasVolumes && strings.TrimSpace(volumesLabel) != "" {
		selected = selectByTokens(eligible, splitCSV(volumesLabel))
	}
	selected = excludeByTokens(selected, splitCSV(norm["volumes.exclude"]))

	paths = make([]string, 0, len(selected))
	for _, m := range selected {
		target, ok := translateHostPath(m.Source, cfg.HostRoots)
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"mount %s (host source %q) does not resolve under any BALLAST_HOST_ROOTS entry, skipped",
				mountLabel(m), m.Source))
			continue
		}
		paths = append(paths, target)
	}
	return paths, warnings
}
