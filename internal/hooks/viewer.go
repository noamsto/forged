package hooks

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const viewerTimeout = 15 * time.Second

// ghViewerArgv is a variable so tests can substitute a stub.
var ghViewerArgv = []string{"gh", "api", "user", "--jq", ".login"}

// GHViewer resolves the authenticated GitHub login with the gh CLI.
func GHViewer(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, viewerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ghViewerArgv[0], ghViewerArgv[1:]...).Output() //nolint:gosec // argv-only, no shell; fixed gh invocation
	if err != nil {
		return "", fmt.Errorf("resolve viewer with gh: %w", err)
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", fmt.Errorf("resolve viewer with gh: empty login")
	}
	return login, nil
}
