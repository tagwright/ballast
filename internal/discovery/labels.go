// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Recognized label prefixes. ballastPrefix is primary and leads all
// documentation and examples; tagwrightPrefix is the org-namespaced alias.
// Both carry the identical suffix grammar; see the Namespace section of the
// label grammar doc.
const (
	ballastPrefix   = "ballast."
	tagwrightPrefix = "tagwright.backup."
)

// stripPrefix removes whichever recognized prefix key carries, returning the
// canonical suffix (e.g. "enable", "retention.daily", "stream.db.command")
// and whether key was recognized at all.
func stripPrefix(key string) (string, bool) {
	if suffix, ok := strings.CutPrefix(key, ballastPrefix); ok && suffix != "" {
		return suffix, true
	}
	if suffix, ok := strings.CutPrefix(key, tagwrightPrefix); ok && suffix != "" {
		return suffix, true
	}
	return "", false
}

// normalizeLabels strips recognized prefixes off a container's labels and
// folds them into a single suffix -> value map. The same suffix may appear
// under both ballast.* and tagwright.backup.*: identical values collapse
// harmlessly, but differing values are a validation error (the conflict
// rule), since there is no silent precedence between the two prefixes.
//
// Keys are walked in sorted order so the error message naming the two
// conflicting label keys is deterministic regardless of Go's randomized map
// iteration.
func normalizeLabels(labels map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	norm := make(map[string]string, len(keys))
	firstKey := make(map[string]string, len(keys))

	for _, k := range keys {
		suffix, ok := stripPrefix(k)
		if !ok {
			continue
		}

		v := labels[k]
		if existingKey, seen := firstKey[suffix]; seen {
			if norm[suffix] != v {
				return nil, fmt.Errorf("discovery: label %q conflicts with %q: %q != %q",
					existingKey, k, norm[suffix], v)
			}
			continue
		}

		norm[suffix] = v
		firstKey[suffix] = k
	}

	return norm, nil
}

// knownSuffixes is the closed set of exact ballast.*/tagwright.backup.* label
// suffixes discovery understands. It backs the unknown-suffix diagnostic: a
// suffix under the namespace that is neither here nor a member of a recognized
// indexed/grouped family (see recognizedFamily) is a typo or a label from a
// newer ballast version. Ballast does not silently drop it (that would leave a
// service backing up under a configuration the operator did not actually get,
// e.g. "retenton.last" applying no retention) nor skip the service over it
// (a typo must never cost a working backup): instead the daemon raises a loud
// alert and backs up under the recognized labels. The set is the exact grammar
// documented in docs/LABELS.md; keep the two in lockstep.
var knownSuffixes = map[string]bool{
	"enable":          true,
	"name":            true,
	"repo":            true,
	"repo.path":       true,
	"password-secret": true,
	"volumes":         true,
	"volumes.exclude": true,
	"exclude":         true,
	"exclude-caches":  true,

	"retention.last":      true,
	"retention.hourly":    true,
	"retention.daily":     true,
	"retention.weekly":    true,
	"retention.monthly":   true,
	"retention.yearly":    true,
	"retention.within":    true,
	"retention.keep-tags": true,

	"exec.pre":          true,
	"exec.pre.timeout":  true,
	"exec.pre.user":     true,
	"exec.post":         true,
	"exec.post.timeout": true,
	"exec.post.user":    true,

	"stop":     true,
	"schedule": true,
	"tags":     true,

	"notify.suppress":   true,
	"notify.on-success": true,

	"verify":             true,
	"verify.mode":        true,
	"verify.probe":       true,
	"verify.expect":      true,
	"verify.timeout":     true,
	"verify.image":       true,
	"verify.restore":     true,
	"verify.ready":       true,
	"verify.user":        true,
	"verify.data-engine": true,
	"verify.schedule":    true,
}

// recognizedFamily reports whether suffix belongs to one of the three grammar
// families whose members are open-ended rather than a fixed name, so the flat
// whitelist cannot enumerate them:
//
//   - exclude.<n>: the indexed exclude escape hatch (exclude.0, exclude.1, ...).
//     Only an integer index is a member; exclude.<word> is a typo, not a family
//     member, so it is left to be flagged.
//   - stream.<id>.<field>: parseStreams validates the id/field shape itself and
//     rejects an unknown field with a specific message, so the whole family is
//     accepted here and that validation is not duplicated.
//   - verify.env.<KEY>: throwaway-container environment entries with an
//     operator-chosen KEY.
func recognizedFamily(suffix string) bool {
	if rest, ok := strings.CutPrefix(suffix, "exclude."); ok {
		_, err := strconv.Atoi(rest)
		return err == nil
	}
	if strings.HasPrefix(suffix, "stream.") {
		return true
	}
	if rest, ok := strings.CutPrefix(suffix, "verify.env."); ok {
		return rest != ""
	}
	return false
}

// unknownSuffixes returns, sorted, every normalized label suffix that is
// neither a recognized exact suffix nor a member of a recognized family. norm
// holds only suffixes under the ballast./tagwright.backup. namespace (a wholly
// foreign label like backup.* never reaches here and stays silently ignored),
// so a non-empty result is always a namespace typo or a forward-version label.
// The caller records these on the spec so the daemon can alert on them without
// skipping the backup.
func unknownSuffixes(norm map[string]string) []string {
	var unknown []string
	for suffix := range norm {
		if knownSuffixes[suffix] || recognizedFamily(suffix) {
			continue
		}
		unknown = append(unknown, suffix)
	}
	sort.Strings(unknown)
	return unknown
}
