package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/noamsto/forge-cache/client"
	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/ghcompat"
)

// watchParent is a test seam, like serveParent.
var watchParent = context.Background()

var eventTypes = []forge.EventType{
	forge.EventChecksStarted, forge.EventChecksCompleted, forge.EventReviewSubmitted,
	forge.EventReviewDecisionChanged, forge.EventStateChanged, forge.EventDraftChanged,
	forge.EventHeadChanged, forge.EventMergeableChanged,
}

func watch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var repoFlag string
	fs.StringVar(&repoFlag, "R", "", "watch `owner/repo` (default: GH_REPO or the git remote)")
	fs.StringVar(&repoFlag, "repo", "", "watch `owner/repo` (default: GH_REPO or the git remote)")
	pr := fs.Int("pr", 0, "watch only pull request `N`")
	events := fs.String("event", "", "comma-separated event `types` to print (default all)")
	since := fs.Uint64("since", 0, "replay buffered events after `SEQ` first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *pr < 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}

	var types []forge.EventType
	if *events != "" {
		for name := range strings.SplitSeq(*events, ",") {
			t := forge.EventType(name)
			if !slices.Contains(eventTypes, t) {
				_, _ = fmt.Fprintf(stderr, "forge-cache: unknown event type %q\n", name)
				return 2
			}
			types = append(types, t)
		}
	}

	ctx, stop := signal.NotifyContext(watchParent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, fb := ghcompat.ResolveRepo(ctx, ghcompat.Request{Repo: repoFlag}, os.Getenv, "")
	if fb != nil {
		_, _ = fmt.Fprintf(stderr, "forge-cache: cannot resolve repo: %v\n", fb)
		return 2
	}

	opts := client.SubscribeOptions{Repo: repo, Number: *pr, Types: types}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "since" {
			opts.Since = since
		}
	})
	ch, err := client.New("").Subscribe(ctx, opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "forge-cache: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	var last uint64
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			_, _ = fmt.Fprintf(stderr, "forge-cache: %v\n", err)
			return 1
		}
		last = ev.Seq
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "forged: stream ended after seq %d; reconnect with --since %d\n", last, last)
	return 1
}
