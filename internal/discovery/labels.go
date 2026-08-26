// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"sort"
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
