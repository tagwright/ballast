// SPDX-License-Identifier: GPL-3.0-or-later

// Command ballast is the label-driven backup daemon and its companion CLI.
package main

import (
	"os"

	"github.com/tagwright/ballast/internal/cli"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// It defaults to the tree's current beta.
var version = "00.01.00b1"

func main() {
	if err := cli.Execute(version); err != nil {
		os.Exit(1)
	}
}
