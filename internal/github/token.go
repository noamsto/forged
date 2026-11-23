package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const ghAuthTimeout = 10 * time.Second

// TokenFromEnvOrGh resolves a token from GH_TOKEN, then GITHUB_TOKEN, then
// `gh auth token`. Empty variables count as unset.
func TokenFromEnvOrGh(ctx context.Context) (string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			return v, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, ghAuthTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output() //nolint:gosec // G204: fixed gh invocation
	if err != nil {
		return "", fmt.Errorf("github token: set GH_TOKEN or GITHUB_TOKEN, or run gh auth login: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("github token: gh auth token printed nothing")
	}
	return token, nil
}
