// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package schedule

import (
	"fmt"

	"github.com/robfig/cron/v3"
)

// parseCron parses a literal (non-splayed) schedule expression: a standard
// 5-field cron expression, or "@every <dur>". Both are handled by
// robfig/cron's standard parser; nothing here hand-rolls cron syntax.
func parseCron(expr string) (NextFunc, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("schedule: invalid schedule %q: %w", expr, err)
	}
	return sched.Next, nil
}
