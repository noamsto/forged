// Package cli dispatches the forge-cache subcommands.
package cli

import (
	"fmt"
	"os"
)

var version = "dev"

const usage = "usage: forge-cache <serve|pr|invalidate|status|version|help>\n"

// Main runs the subcommand named by args[0] and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("forge-cache " + version)
		return 0
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}
