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

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/ghcompat"
)

// watchParent is a test seam, like serveParent.
var watchParent = context.Background()

var eventTypes = []forge.EventType{
	forge.EventChecksStarted, forge.EventChecksCompleted, forge.EventReviewSubmitted,
	forge.EventReviewDecisionChanged, forge.EventStateChanged, forge.EventDraftChanged,
	forge.EventHeadChanged, forge.EventMergeableChanged,
}

type watchFlags struct {
	repo   string
	pr     int
	events string
	since  uint64
}

// newWatchFlagSet is shared with skill_test.go so the skill's examples are
// checked against the flags watch really accepts.
func newWatchFlagSet(stderr io.Writer) (*flag.FlagSet, *watchFlags) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var w watchFlags
	fs.StringVar(&w.repo, "R", "", "watch `owner/repo` (default: GH_REPO or the git remote)")
	fs.StringVar(&w.repo, "repo", "", "watch `owner/repo` (default: GH_REPO or the git remote)")
	fs.IntVar(&w.pr, "pr", 0, "watch only pull request `N`")
	fs.StringVar(&w.events, "event", "", "comma-separated event `types` to print (default all)")
	fs.Uint64Var(&w.since, "since", 0, "replay buffered events after `SEQ` first")
	return fs, &w
}

func watch(args []string, stdout, stderr io.Writer) int {
	fs, wf := newWatchFlagSet(stderr)
	repoFlag, pr, events, since := &wf.repo, &wf.pr, &wf.events, &wf.since
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
				_, _ = fmt.Fprintf(stderr, "forged: unknown event type %q\n", name)
				return 2
			}
			types = append(types, t)
		}
	}

	ctx, stop := signal.NotifyContext(watchParent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, fb := ghcompat.ResolveRepo(ctx, ghcompat.Request{Repo: *repoFlag}, os.Getenv, "")
	if fb != nil {
		_, _ = fmt.Fprintf(stderr, "forged: cannot resolve repo: %v\n", fb)
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
		_, _ = fmt.Fprintf(stderr, "forged: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	var last uint64
	resumable := opts.Since != nil
	if resumable {
		last = *opts.Since
	}
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			_, _ = fmt.Fprintf(stderr, "forged: %v\n", err)
			return 1
		}
		last, resumable = ev.Seq, true
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return 0
	}
	if !resumable {
		_, _ = fmt.Fprintln(stderr, "forged: stream ended before any event")
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "forged: stream ended after seq %d; reconnect with --since %d\n", last, last)
	return 1
}
