// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tagwright/ballast/internal/engine"
)

// Timeouts applied when a stream or hook label sets no timeout of its own.
const (
	defaultStreamTimeout = 15 * time.Minute
	defaultHookTimeout   = 5 * time.Minute
)

// parseBool reads a "true"/"false" label, defaulting to def when the suffix
// is absent or empty.
func parseBool(m map[string]string, key string, def bool) (bool, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("discovery: label %q: invalid bool %q: %w", key, v, err)
	}
	return b, nil
}

// parseInt reads an integer label, defaulting to zero (meaning "unset", per
// the retention convention) when the suffix is absent or empty.
func parseInt(m map[string]string, key string) (int, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("discovery: label %q: invalid int %q: %w", key, v, err)
	}
	return n, nil
}

// parseDuration reads a Go-style duration label, defaulting to def when the
// suffix is absent or empty.
func parseDuration(m map[string]string, key string, def time.Duration) (time.Duration, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("discovery: label %q: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}

// splitCSV splits a comma-separated label value, trimming whitespace and
// dropping empty elements. An absent or empty value yields nil.
func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// indexedValues collects the "<prefix>.<n>" escape-hatch labels (e.g.
// exclude.0, exclude.1, ...) in ascending index order. Non-numeric suffixes
// after prefix+"." are ignored, since they belong to a different label.
func indexedValues(m map[string]string, prefix string) []string {
	type kv struct {
		idx int
		val string
	}
	var items []kv

	want := prefix + "."
	for k, v := range m {
		rest, ok := strings.CutPrefix(k, want)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		items = append(items, kv{n, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].idx < items[j].idx })

	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.val)
	}
	return out
}

// parseExcludes resolves ballast.exclude (csv) or the indexed
// ballast.exclude.<n> escape hatch. The two forms are mutually exclusive per
// the grammar; setting both is a validation error rather than a silent
// precedence choice.
func parseExcludes(m map[string]string) ([]string, error) {
	csv, hasCSV := m["exclude"]
	indexed := indexedValues(m, "exclude")

	if hasCSV && csv != "" && len(indexed) > 0 {
		return nil, fmt.Errorf("discovery: %q and the indexed %q labels are mutually exclusive", "exclude", "exclude.<n>")
	}
	if len(indexed) > 0 {
		return indexed, nil
	}
	return splitCSV(csv), nil
}

// retentionFields maps each integer retention label to the RetentionPolicy
// field it populates.
var retentionFields = []struct {
	key string
	dst func(*engine.RetentionPolicy) *int
}{
	{"retention.last", func(p *engine.RetentionPolicy) *int { return &p.Last }},
	{"retention.hourly", func(p *engine.RetentionPolicy) *int { return &p.Hourly }},
	{"retention.daily", func(p *engine.RetentionPolicy) *int { return &p.Daily }},
	{"retention.weekly", func(p *engine.RetentionPolicy) *int { return &p.Weekly }},
	{"retention.monthly", func(p *engine.RetentionPolicy) *int { return &p.Monthly }},
	{"retention.yearly", func(p *engine.RetentionPolicy) *int { return &p.Yearly }},
}

// parseRetention builds a RetentionPolicy from exactly the retention labels
// present on the container. Per Fork 2, this is a replace, not a merge: any
// dimension the container does not label stays at RetentionPolicy's zero
// value rather than inheriting a global default. Deciding whether to fall
// back to the global policy at all (because no retention label was present)
// is the caller's job, not this function's.
func parseRetention(m map[string]string) (engine.RetentionPolicy, error) {
	var pol engine.RetentionPolicy

	for _, f := range retentionFields {
		n, err := parseInt(m, f.key)
		if err != nil {
			return engine.RetentionPolicy{}, err
		}
		*f.dst(&pol) = n
	}

	pol.Within = m["retention.within"]
	pol.KeepTags = splitCSV(m["retention.keep-tags"])

	return pol, nil
}

// parseStreams groups the stream.<id>.<field> labels by id and builds one
// StreamSpec per id. A stream with no command label is a validation error:
// there would be nothing to run.
func parseStreams(m map[string]string) ([]StreamSpec, error) {
	type partial struct {
		command, filename, user string
		timeout                 time.Duration
		timeoutSet              bool
	}

	groups := make(map[string]*partial)
	var ids []string

	for k, v := range m {
		rest, ok := strings.CutPrefix(k, "stream.")
		if !ok {
			continue
		}
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			return nil, fmt.Errorf("discovery: malformed stream label %q, want stream.<id>.<field>", k)
		}
		id, field := rest[:dot], rest[dot+1:]

		p, exists := groups[id]
		if !exists {
			p = &partial{}
			groups[id] = p
			ids = append(ids, id)
		}

		switch field {
		case "command":
			p.command = v
		case "filename":
			p.filename = v
		case "user":
			p.user = v
		case "timeout":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("discovery: label %q: invalid duration %q: %w", k, v, err)
			}
			p.timeout, p.timeoutSet = d, true
		default:
			return nil, fmt.Errorf("discovery: unknown stream field %q", k)
		}
	}

	sort.Strings(ids)
	out := make([]StreamSpec, 0, len(ids))
	for _, id := range ids {
		p := groups[id]
		if p.command == "" {
			return nil, fmt.Errorf("discovery: stream %q has no stream.%s.command label", id, id)
		}

		filename := p.filename
		if filename == "" {
			filename = id
		}
		timeout := defaultStreamTimeout
		if p.timeoutSet {
			timeout = p.timeout
		}

		out = append(out, StreamSpec{
			ID:       id,
			Command:  p.command,
			Filename: filename,
			User:     p.user,
			Timeout:  timeout,
		})
	}
	return out, nil
}

// parseHook resolves an exec.pre or exec.post hook (name is "pre" or
// "post"). It returns nil, nil when the container sets no command for it.
func parseHook(m map[string]string, name string) (*HookSpec, error) {
	base := "exec." + name
	cmd, ok := m[base]
	if !ok || cmd == "" {
		return nil, nil
	}

	timeout, err := parseDuration(m, base+".timeout", defaultHookTimeout)
	if err != nil {
		return nil, err
	}

	return &HookSpec{
		Command: cmd,
		User:    m[base+".user"],
		Timeout: timeout,
	}, nil
}
