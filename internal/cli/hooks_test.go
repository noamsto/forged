package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/hooks"
)

const hooksRule = `
[[rule]]
name = "r"
repos = ["o/r"]
on = "state.changed"
command = ["true"]
`

const hooksRuleAuthor = hooksRule + `author = "me"` + "\n"

// idleSource ends every subscription immediately; the runner only sees a closed stream.
type idleSource struct{}

func (idleSource) Subscribe(context.Context, client.SubscribeOptions) (<-chan forge.ChangeEvent, error) {
	ch := make(chan forge.ChangeEvent)
	close(ch)
	return ch, nil
}

func (idleSource) Change(context.Context, forge.RepoRef, int, client.ReadOptions) (forge.Change, error) {
	return forge.Change{}, errors.New("unused")
}

func (idleSource) ByBranch(context.Context, forge.RepoRef, string, client.ReadOptions) ([]forge.Change, error) {
	return nil, nil
}

func stubHooksSeams(t *testing.T, viewer func(context.Context) (string, error)) {
	t.Helper()
	oldSrc, oldViewer, oldParent := newHooksSource, hooksViewer, hooksParent
	t.Cleanup(func() { newHooksSource, hooksViewer, hooksParent = oldSrc, oldViewer, oldParent })
	newHooksSource = func() hooks.Source { return idleSource{} }
	hooksViewer = viewer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hooksParent = ctx
}

func writeHooksConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hooks.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHooksFlagSet(t *testing.T) {
	var stderr bytes.Buffer
	fs, hf := newHooksFlagSet(&stderr)
	if err := fs.Parse([]string{"--config", "/x/y.toml"}); err != nil {
		t.Fatal(err)
	}
	if hf.config != "/x/y.toml" {
		t.Errorf("config = %q", hf.config)
	}
}

func TestHooksUsageErrors(t *testing.T) {
	stubHooksSeams(t, func(context.Context) (string, error) { return "me", nil })
	cfg := writeHooksConfig(t, hooksRule)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--bogus"}},
		{"positional", []string{"--config", cfg, "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := hooksCmd(tc.args, &stderr); got != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", got, stderr.String())
			}
		})
	}
}

func TestHooksConfigErrorsNameThePath(t *testing.T) {
	stubHooksSeams(t, func(context.Context) (string, error) { return "me", nil })
	missing := filepath.Join(t.TempDir(), "nope.toml")
	invalid := writeHooksConfig(t, "[[rule]]\nbogus = 1\n")
	for name, path := range map[string]string{"missing": missing, "invalid": invalid} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := hooksCmd([]string{"--config", path}, &stderr); got != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", got, stderr.String())
			}
			if !strings.Contains(stderr.String(), path) {
				t.Errorf("stderr does not name %q: %s", path, stderr.String())
			}
		})
	}
}

func TestHooksDefaultConfigPath(t *testing.T) {
	stubHooksSeams(t, func(context.Context) (string, error) { return "me", nil })
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	want, err := hooks.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if got := hooksCmd(nil, &stderr); got != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr does not name default path %q: %s", want, stderr.String())
	}

	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte(hooksRuleAuthor), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if got := hooksCmd(nil, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", got, stderr.String())
	}
}

func TestHooksViewerUnresolvableExits1(t *testing.T) {
	stubHooksSeams(t, func(context.Context) (string, error) { return "", errors.New("gh not logged in") })
	cfg := writeHooksConfig(t, hooksRule)
	var stderr bytes.Buffer
	if got := hooksCmd([]string{"--config", cfg}, &stderr); got != 1 {
		t.Fatalf("exit = %d, want 1; stderr: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gh not logged in") {
		t.Errorf("stderr missing cause: %s", stderr.String())
	}
}

func TestHooksCancelledExits0AndLogsStartup(t *testing.T) {
	stubHooksSeams(t, func(context.Context) (string, error) { return "me", nil })
	cfg := writeHooksConfig(t, hooksRule)
	var stderr bytes.Buffer
	if got := hooksCmd([]string{"--config", cfg}, &stderr); got != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, cfg) || !strings.Contains(out, "rules=1") {
		t.Errorf("startup log missing config path or rule count: %s", out)
	}
}
