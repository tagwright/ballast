// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tagwright/ballast/internal/engine"
)

// defaultRetentionSpec is the sane global default applied when a service's
// spec.Retention is unset (the zero value) and Config.Retention is also
// empty.
const defaultRetentionSpec = "daily=7,weekly=4,monthly=6"

// isZeroRetention reports whether p is the zero value of
// engine.RetentionPolicy: per the label grammar, a service's labeled
// retention policy always replaces the global default wholesale, so the
// caller only reaches for the global default when nothing at all was set.
func isZeroRetention(p engine.RetentionPolicy) bool {
	return p.Last == 0 && p.Hourly == 0 && p.Daily == 0 && p.Weekly == 0 &&
		p.Monthly == 0 && p.Yearly == 0 && p.Within == "" && len(p.KeepTags) == 0
}

// defaultRetentionPolicy parses spec (Config.Retention) into a
// RetentionPolicy, falling back to defaultRetentionSpec if spec is blank.
func defaultRetentionPolicy(spec string) (engine.RetentionPolicy, error) {
	if strings.TrimSpace(spec) == "" {
		spec = defaultRetentionSpec
	}
	return ParseRetentionPolicy(spec)
}

// ParseRetentionPolicy parses the compact "key=value,key=value" retention
// form Config.Retention (and, for symmetry, a service's global-default
// fallback) uses, e.g. "daily=7,weekly=4,monthly=6". Recognized keys are the
// lowercased RetentionPolicy field names: last, hourly, daily, weekly,
// monthly, yearly (integers), within (a raw restic duration string, e.g.
// "7d"), and keep-tags (a comma-separated... list, itself separated from
// its peers by the outer comma, so keep-tags is best kept to a single tag
// per policy string, or set via a service's own ballast.retention.keep-tags
// label instead).
func ParseRetentionPolicy(spec string) (engine.RetentionPolicy, error) {
	var policy engine.RetentionPolicy

	for _, term := range strings.Split(spec, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}

		key, value, ok := strings.Cut(term, "=")
		if !ok {
			return engine.RetentionPolicy{}, fmt.Errorf("orchestrator: invalid retention term %q, want key=value", term)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "within":
			policy.Within = value
			continue
		case "keep-tags":
			policy.KeepTags = append(policy.KeepTags, value)
			continue
		}

		n, err := strconv.Atoi(value)
		if err != nil {
			return engine.RetentionPolicy{}, fmt.Errorf("orchestrator: retention %q: invalid value %q: %w", key, value, err)
		}
		switch key {
		case "last":
			policy.Last = n
		case "hourly":
			policy.Hourly = n
		case "daily":
			policy.Daily = n
		case "weekly":
			policy.Weekly = n
		case "monthly":
			policy.Monthly = n
		case "yearly":
			policy.Yearly = n
		default:
			return engine.RetentionPolicy{}, fmt.Errorf("orchestrator: unknown retention key %q", key)
		}
	}

	return policy, nil
}
