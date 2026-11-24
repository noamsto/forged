package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/internal/hooks"
)

// Test seams, like serveParent and newProviders.
var (
	hooksParent    = context.Background()
	newHooksSource = func() hooks.Source { return client.New("") }
	hooksViewer    = hooks.GHViewer
)

type hooksFlags struct {
	config string
}

// newHooksFlagSet is shared with skill_test.go so the skill's examples are
// checked against the flags hooks really accepts.
func newHooksFlagSet(stderr io.Writer) (*flag.FlagSet, *hooksFlags) {
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hooksFlags
	fs.StringVar(&h.config, "config", "", "hooks.toml `path` (default $XDG_CONFIG_HOME/forged/hooks.toml)")
	return fs, &h
}

func hooksCmd(args []string, stderr io.Writer) int {
	fs, hf := newHooksFlagSet(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	path := hf.config
	if path == "" {
		var err error
		if path, err = hooks.DefaultPath(); err != nil {
			_, _ = fmt.Fprintf(stderr, "forged: hooks config path: %v\n", err)
			return 2
		}
	}
	cfg, err := hooks.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "forged: hooks config %s: %v\n", path, err)
		return 2
	}

	r, err := hooks.New(hooks.Options{Config: cfg, Source: newHooksSource(), Viewer: hooksViewer, Log: log})
	if err != nil {
		log.Error("hooks", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(hooksParent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("hooks starting", "config", path, "rules", len(cfg.Rules))
	if err := r.Run(ctx); err != nil {
		log.Error("hooks", "err", err)
		return 1
	}
	return 0
}
