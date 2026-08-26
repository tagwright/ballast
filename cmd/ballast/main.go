// SPDX-License-Identifier: GPL-3.0-or-later

// Command ballast is the label-driven backup daemon and its companion CLI.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// It defaults to the tree's current beta.
var version = "00.01.00b1"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("ballast %s\n", version)
		return
	}
	// The daemon loop and the restore CLI land next, on the Cobra command tree.
	fmt.Fprintln(os.Stderr, "ballast: not yet implemented")
	os.Exit(1)
}
