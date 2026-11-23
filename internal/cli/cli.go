// Package cli dispatches the forge-cache subcommands.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/forge-cache/client"
	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/ghcompat"
)

var version = "dev"

const usage = `usage: forge-cache <command>

commands:
  serve [--socket PATH] [--snapshot PATH] [--lease-ttl D] [--hot-interval D] [--warm-interval D]
  pr view|list ...                gh-compatible, served from the daemon
  invalidate [host/]owner/repo[#N]  refetch a repo's open list or one change now
  status                          print daemon version and stats as JSON
  version
  help
`

// Main runs the subcommand named by args[0] and returns the process exit code.
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "forge-cache "+version)
		return 0
	case "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve":
		return serve(args[1:], stderr)
	case "pr":
		return ghcompat.Run(args, stdout, stderr, os.Getenv)
	case "invalidate":
		return invalidate(args[1:], stderr)
	case "status":
		return status(stdout, stderr)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func invalidate(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	k, err := parseKey(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "forge-cache: %v\n%s", err, usage)
		return 2
	}
	if err := client.New("").Invalidate(context.Background(), k); err != nil {
		fmt.Fprintf(stderr, "forge-cache: %v\n", err)
		return 1
	}
	return 0
}

// parseKey reads "[host/]owner/repo" as the repo's open list and
// "[host/]owner/repo#N" as one change.
func parseKey(s string) (forge.Key, error) {
	repoPart, num, hasNum := strings.Cut(s, "#")
	repo, err := forge.ParseRepo(repoPart)
	if err != nil {
		return forge.Key{}, err
	}
	if !hasNum {
		return forge.RepoOpen(repo), nil
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 {
		return forge.Key{}, fmt.Errorf("invalid change number %q", num)
	}
	return forge.ChangeKey(repo, n), nil
}

func status(stdout, stderr io.Writer) int {
	s, err := client.New("").Status(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "forge-cache: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		fmt.Fprintf(stderr, "forge-cache: %v\n", err)
		return 1
	}
	return 0
}
