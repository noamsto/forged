package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// maxReconcileRetry bounds how often a failing reconcile re-queries the forge.
	maxReconcileRetry = 5 * time.Minute
)

// Options configures a Runner.
type Options struct {
	Config Config
	Source Source
	// Viewer resolves the viewer login when Config.Viewer is empty.
	Viewer func(ctx context.Context) (string, error)
	Log    *slog.Logger
	// Sleep waits between reconnect attempts; nil sleeps for real, honouring ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Runner subscribes to every repo named by the rules and runs matching actions.
type Runner struct {
	rules  []Rule
	repos  []forge.RepoRef
	src    Source
	viewer string
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) error

	// mu serializes every action across repos.
	mu        sync.Mutex
	runCmd    func(ctx context.Context, r Rule, ev forge.ChangeEvent, ch forge.Change, log *slog.Logger) error
	remove    func(ctx context.Context, r Rule, ch forge.Change, log *slog.Logger) error
	reconcile func(ctx context.Context, r Rule, src Source, viewer string, log *slog.Logger) error
}

// New resolves the viewer once and prepares a Runner. A viewer that a rule
// needs (no explicit author) but that cannot be resolved is an error.
func New(o Options) (*Runner, error) {
	r := &Runner{
		rules:     o.Config.Rules,
		src:       o.Source,
		viewer:    o.Config.Viewer,
		log:       o.Log,
		sleep:     o.Sleep,
		runCmd:    runCommand,
		remove:    removeMergedWorktree,
		reconcile: reconcileWorktrees,
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	for _, rule := range r.rules {
		for _, repo := range rule.Repos {
			if !slices.Contains(r.repos, repo) {
				r.repos = append(r.repos, repo)
			}
		}
	}
	needsViewer := slices.ContainsFunc(r.rules, func(rule Rule) bool { return rule.Author == "" })
	if r.viewer == "" && needsViewer {
		if o.Viewer == nil {
			return nil, errors.New("hooks: a rule without author needs the viewer, but none is configured")
		}
		v, err := o.Viewer(context.Background())
		if err != nil {
			return nil, fmt.Errorf("hooks: resolving the viewer: %w", err)
		}
		if v == "" {
			return nil, errors.New("hooks: resolving the viewer: empty login")
		}
		r.viewer = v
	}
	return r, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run blocks until ctx is done, one subscription loop per repo.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, repo := range r.repos {
		wg.Go(func() { r.watchRepo(ctx, repo) })
	}
	wg.Wait()
	return nil
}

func (r *Runner) watchRepo(ctx context.Context, repo forge.RepoRef) {
	log := r.log.With("repo", repo.String())
	var cursor *uint64
	reconcilePending := false
	delay := minBackoff
	for ctx.Err() == nil {
		events, err := r.src.Subscribe(ctx, client.SubscribeOptions{Repo: repo, Since: cursor})
		got := false
		if err != nil {
			log.Warn("subscribe failed", "err", err)
		} else {
			// Reconciling after subscribing means nothing is lost in between.
			if cursor == nil {
				reconcilePending = true
			}
			if reconcilePending {
				reconcilePending = r.reconcileRepo(ctx, repo, log) != nil
			}
			got = r.consume(ctx, repo, events, &cursor, &reconcilePending, log)
		}
		if ctx.Err() != nil {
			return
		}
		if got {
			delay = minBackoff
		}
		if err := r.sleep(ctx, delay); err != nil {
			return
		}
		delay = min(delay*2, maxBackoff)
	}
}

// consume handles events until the stream ends or ctx is done, reporting
// whether any event arrived. While *reconcilePending is set it retries the
// reconcile on the backoff schedule, so a quiet stream still converges.
func (r *Runner) consume(ctx context.Context, repo forge.RepoRef, events <-chan forge.ChangeEvent, cursor **uint64, reconcilePending *bool, log *slog.Logger) bool {
	got := false
	retryDelay := minBackoff
	var retry chan struct{}
	for {
		if *reconcilePending && retry == nil {
			retry = make(chan struct{}, 1)
			go func(c chan<- struct{}, d time.Duration) {
				if r.sleep(ctx, d) == nil {
					c <- struct{}{}
				}
			}(retry, retryDelay)
			retryDelay = min(retryDelay*2, maxReconcileRetry)
		}
		select {
		case <-ctx.Done():
			return got
		case <-retry:
			retry = nil
			if *reconcilePending {
				*reconcilePending = r.reconcileRepo(ctx, repo, log) != nil
			}
		case ev, ok := <-events:
			if !ok {
				log.Info("event stream ended")
				return got
			}
			got = true
			seq := ev.Seq
			*cursor = &seq
			if ev.Type == forge.EventResync {
				*reconcilePending = r.reconcileRepo(ctx, repo, log) != nil
				continue
			}
			r.handle(ctx, ev, log)
		}
	}
}

// reconcileRepo returns the joined failures of every matching rule, or nil
// when ctx is done (shutdown is not a failure to retry).
func (r *Runner) reconcileRepo(ctx context.Context, repo forge.RepoRef, log *slog.Logger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, rule := range r.rules {
		if rule.Builtin != BuiltinRemoveMergedWorktree || !slices.Contains(rule.Repos, repo) {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := r.reconcile(ctx, rule, r.src, r.viewer, log); err != nil && ctx.Err() == nil {
			log.Warn("reconcile failed", "rule", rule.Name, "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) handle(ctx context.Context, ev forge.ChangeEvent, log *slog.Logger) {
	var ch forge.Change
	fetched := false
	for _, rule := range r.rules {
		if !rule.MatchEvent(ev) {
			continue
		}
		if !fetched {
			// The event came from the cached value, so any age is acceptable;
			// the default max-age could fail during a forge rate-limit pause.
			maxAge := time.Hour
			c, err := r.src.Change(ctx, ev.Repo, ev.Number, client.ReadOptions{MaxAge: &maxAge})
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("reading the change failed; skipping its rules", "number", ev.Number, "type", ev.Type, "err", err)
				}
				return
			}
			ch, fetched = c, true
		}
		if !rule.MatchChange(ch, r.viewer) {
			continue
		}
		r.act(ctx, rule, ev, ch, log)
	}
}

func (r *Runner) act(ctx context.Context, rule Rule, ev forge.ChangeEvent, ch forge.Change, log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	switch rule.Builtin {
	case BuiltinRemoveMergedWorktree:
		if ch.State != forge.StateMerged {
			return
		}
		err = r.remove(ctx, rule, ch, log)
	case "":
		err = r.runCmd(ctx, rule, ev, ch, log)
	}
	if err != nil && ctx.Err() == nil {
		log.Warn("rule action failed", "rule", rule.Name, "number", ev.Number, "err", err)
	}
}
