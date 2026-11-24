package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func viewerStub(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	old := ghViewerArgv
	ghViewerArgv = []string{p, "api", "user", "--jq", ".login"}
	t.Cleanup(func() { ghViewerArgv = old })
}

func TestGHViewer(t *testing.T) {
	viewerStub(t, `[ "$1 $2 $3 $4" = "api user --jq .login" ] && printf 'octo\n'`)
	got, err := GHViewer(context.Background())
	if err != nil || got != "octo" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGHViewerFailures(t *testing.T) {
	viewerStub(t, "exit 1")
	if _, err := GHViewer(context.Background()); err == nil {
		t.Error("want an error on a failing gh")
	}
	viewerStub(t, "printf '\\n'")
	if _, err := GHViewer(context.Background()); err == nil {
		t.Error("want an error on empty output")
	}
}
