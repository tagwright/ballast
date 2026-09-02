// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Command ballast is the label-driven backup daemon and its companion CLI.
package main

import (
	"os"

	"github.com/tagwright/ballast/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// It defaults to the tree's current VERSION.
var version = "00.02.00"

func main() {
	if err := cli.Execute(version); err != nil {
		os.Exit(1)
	}
}
