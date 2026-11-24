package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

var runRepo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

type runSub struct {
	err    error
	events []forge.ChangeEvent
	hold   bool // keep the stream open until ctx is done
}

// runSource scripts one runSub per Subscribe call; after the script is used up
// it hands out held-open streams.
type runSource struct {
	mu      sync.Mutex
	subs    []runSub
	calls   []client.SubscribeOptions
	changes map[int]forge.Change
	chErrs  map[int]error
	chReads int
}

func (s *runSource) Subscribe(ctx context.Context, o client.SubscribeOptions) (<-chan forge.ChangeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.Since != nil {
		v := *o.Since
		o.Since = &v
	}
	s.calls = append(s.calls, o)
	sub := runSub{hold: true}
	if len(s.subs) > 0 {
		sub, s.subs = s.subs[0], s.subs[1:]
	}
	if sub.err != nil {
		return nil, sub.err
	}
	ch := make(chan forge.ChangeEvent, len(sub.events))
	for _, ev := range sub.events {
		ch <- ev
	}
	if sub.hold {
		go func() { <-ctx.Done(); close(ch) }()
	} else {
		close(ch)
	}
	return ch, nil
}

func (s *runSource) Change(_ context.Context, _ forge.RepoRef, n int, _ client.ReadOptions) (forge.Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chReads++
	if err := s.chErrs[n]; err != nil {
		return forge.Change{}, err
	}
	return s.changes[n], nil
}

func (s *runSource) ByBranch(context.Context, forge.RepoRef, string, client.ReadOptions) ([]forge.Change, error) {
	return nil, nil
}

func (s *runSource) subscribeCalls() []client.SubscribeOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.SubscribeOptions(nil), s.calls...)
}

type runRecorder struct {
	mu         sync.Mutex
	cmds       []int // PR numbers of command runs
	removed    []int
	reconciled []forge.RepoRef
	active     atomic.Int32
	maxActive  atomic.Int32
	delay      time.Duration
}

func (rc *runRecorder) enter() {
	n := rc.active.Add(1)
	for {
		m := rc.maxActive.Load()
		if n <= m || rc.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(rc.delay)
}

func (rc *runRecorder) leave() { rc.active.Add(-1) }

func (rc *runRecorder) install(r *Runner) {
	r.runCmd = func(_ context.Context, _ Rule, ev forge.ChangeEvent, _ forge.Change, _ *slog.Logger) error {
		rc.enter()
		defer rc.leave()
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.cmds = append(rc.cmds, ev.Number)
		return nil
	}
	r.remove = func(_ context.Context, _ Rule, ch forge.Change, _ *slog.Logger) error {
		rc.enter()
		defer rc.leave()
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.removed = append(rc.removed, ch.Number)
		return errors.New("boom") // errors must not stop the loop
	}
	r.reconcile = func(_ context.Context, rule Rule, _ Source, _ string, _ *slog.Logger) error {
		rc.enter()
		defer rc.leave()
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.reconciled = append(rc.reconciled, rule.Repos[0])
		return nil
	}
}

func (rc *runRecorder) snapshot() (cmds, removed []int, reconciled []forge.RepoRef) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]int(nil), rc.cmds...), append([]int(nil), rc.removed...), append([]forge.RepoRef(nil), rc.reconciled...)
}

func runEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runNoSleep(context.Context, time.Duration) error { return nil }

func runLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runStart(t *testing.T, cfg Config, src Source, rc *runRecorder, viewer func(context.Context) (string, error), sleep func(context.Context, time.Duration) error) (stop func()) {
	t.Helper()
	if sleep == nil {
		sleep = runNoSleep
	}
	r, err := New(Options{Config: cfg, Source: src, Viewer: viewer, Log: runLog(), Sleep: sleep})
	if err != nil {
		t.Fatal(err)
	}
	if rc != nil {
		rc.install(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func runViewer(login string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return login, nil }
}

func runEvent(seq uint64, n int) forge.ChangeEvent {
	return forge.ChangeEvent{Seq: seq, Type: forge.EventStateChanged, Repo: runRepo, Number: n, Old: "open", New: "merged"}
}

func runChange(n int, author, branch string) forge.Change {
	ch := forge.Change{Repo: runRepo, Number: n, State: forge.StateMerged}
	ch.Author.Login = author
	ch.Head.Ref = branch
	return ch
}

func runCmdRule(name string) Rule {
	return Rule{Name: name, Repos: []forge.RepoRef{runRepo}, On: forge.EventStateChanged, Command: []string{"x"}}
}

func runBuiltinRule() Rule {
	return Rule{Name: "wt", Repos: []forge.RepoRef{runRepo}, On: forge.EventStateChanged, To: "merged", Builtin: BuiltinRemoveMergedWorktree, Checkout: "/nonexistent"}
}

func TestRunnerMatchingRuleRuns(t *testing.T) {
	src := &runSource{
		subs:    []runSub{{events: []forge.ChangeEvent{runEvent(1, 5)}, hold: true}},
		changes: map[int]forge.Change{5: runChange(5, "Me", "feat/x")},
	}
	rc := &runRecorder{}
	stop := runStart(t, Config{Rules: []Rule{runCmdRule("a"), runCmdRule("b")}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "both rules", func() bool { c, _, _ := rc.snapshot(); return len(c) == 2 })
	if src.chReads != 1 {
		t.Errorf("change read %d times, want once per event", src.chReads)
	}
}

func TestRunnerFilters(t *testing.T) {
	other := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "other"}
	rule := runCmdRule("a")
	rule.Branch = "feat/*"
	rule.Repos = []forge.RepoRef{runRepo, other}
	typeEv := runEvent(1, 1)
	typeEv.Type = forge.EventDraftChanged
	repoEv := runEvent(2, 2)
	repoEv.Repo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "elsewhere"}
	src := &runSource{
		subs: []runSub{{hold: true, events: []forge.ChangeEvent{
			typeEv, repoEv,
			runEvent(3, 3), // other author
			runEvent(4, 4), // other branch
			runEvent(5, 5), // matches
		}}},
		changes: map[int]forge.Change{
			1: runChange(1, "me", "feat/a"), 2: runChange(2, "me", "feat/a"),
			3: runChange(3, "stranger", "feat/a"), 4: runChange(4, "me", "fix/a"), 5: runChange(5, "ME", "feat/a/b"),
		},
	}
	rc := &runRecorder{}
	stop := runStart(t, Config{Rules: []Rule{rule}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "sentinel", func() bool { c, _, _ := rc.snapshot(); return len(c) > 0 })
	time.Sleep(20 * time.Millisecond)
	if c, _, _ := rc.snapshot(); len(c) != 1 || c[0] != 5 {
		t.Fatalf("ran for %v, want only [5]", c)
	}
}

func TestRunnerChangeReadFailureSkips(t *testing.T) {
	src := &runSource{
		subs:    []runSub{{hold: true, events: []forge.ChangeEvent{runEvent(1, 1), runEvent(2, 2)}}},
		changes: map[int]forge.Change{2: runChange(2, "me", "b")},
		chErrs:  map[int]error{1: errors.New("cache down")},
	}
	rc := &runRecorder{}
	stop := runStart(t, Config{Rules: []Rule{runCmdRule("a")}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "second event", func() bool { c, _, _ := rc.snapshot(); return len(c) > 0 })
	if c, _, _ := rc.snapshot(); len(c) != 1 || c[0] != 2 {
		t.Fatalf("ran for %v, want [2]", c)
	}
}

func TestRunnerBuiltinOnlyForMerged(t *testing.T) {
	open := runChange(1, "me", "b")
	open.State = forge.StateOpen
	src := &runSource{
		subs:    []runSub{{hold: true, events: []forge.ChangeEvent{runEvent(1, 1), runEvent(2, 2)}}},
		changes: map[int]forge.Change{1: open, 2: runChange(2, "me", "b")},
	}
	rc := &runRecorder{}
	stop := runStart(t, Config{Rules: []Rule{runBuiltinRule()}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "removal", func() bool { _, r, _ := rc.snapshot(); return len(r) > 0 })
	if _, r, _ := rc.snapshot(); len(r) != 1 || r[0] != 2 {
		t.Fatalf("removed %v, want [2]", r)
	}
}

func TestRunnerReconcileOnStartupAndResync(t *testing.T) {
	resync := forge.ChangeEvent{Seq: 9, Type: forge.EventResync} // zero Repo and Number, like the cache's
	src := &runSource{subs: []runSub{{hold: true}}}
	rc := &runRecorder{}
	stop := runStart(t, Config{Rules: []Rule{runBuiltinRule(), runCmdRule("cmd")}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "startup reconcile", func() bool { _, _, r := rc.snapshot(); return len(r) == 1 })

	src2 := &runSource{subs: []runSub{{hold: true, events: []forge.ChangeEvent{resync}}}}
	rc2 := &runRecorder{}
	stop2 := runStart(t, Config{Rules: []Rule{runBuiltinRule()}}, src2, rc2, runViewer("me"), nil)
	defer stop2()
	runEventually(t, "startup + resync reconcile", func() bool { _, _, r := rc2.snapshot(); return len(r) == 2 })
	if _, _, r := rc2.snapshot(); r[1] != runRepo {
		t.Errorf("resync reconciled %v, want the subscription repo", r[1])
	}
	if c := src2.subscribeCalls(); len(c) != 1 {
		t.Errorf("subscribe calls = %d, want 1", len(c))
	}
}

func TestRunnerReconnect(t *testing.T) {
	src := &runSource{
		subs: []runSub{
			{err: errors.New("daemon down")},
			{}, // ends without events
			{events: []forge.ChangeEvent{runEvent(7, 1), runEvent(8, 1)}}, // ends after events
			{hold: true},
		},
		changes: map[int]forge.Change{1: runChange(1, "me", "b")},
	}
	rc := &runRecorder{}
	var sleeps []time.Duration
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
		return nil
	}
	stop := runStart(t, Config{Rules: []Rule{runBuiltinRule()}}, src, rc, runViewer("me"), sleep)
	defer stop()
	runEventually(t, "4 subscribes", func() bool { return len(src.subscribeCalls()) == 4 })
	calls := src.subscribeCalls()
	for i, want := range []*uint64{nil, nil, nil} {
		if calls[i].Since != want {
			t.Errorf("call %d Since = %v, want nil", i, calls[i].Since)
		}
	}
	if s := calls[3].Since; s == nil || *s != 8 {
		t.Errorf("reconnect after events Since = %v, want 8", s)
	}
	// The failed subscribe does not reconcile; the two successful cursorless ones do; the cursored one does not.
	if _, _, r := rc.snapshot(); len(r) != 2 {
		t.Errorf("reconciles = %d, want 2", len(r))
	}
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(sleeps) < 3 || sleeps[0] != want[0] || sleeps[1] != want[1] || sleeps[2] != time.Second {
		t.Errorf("sleeps = %v, want %v then a reset to 1s after events", sleeps, want[:2])
	}
}

func TestRunnerSerializesActions(t *testing.T) {
	other := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "other"}
	rule := runCmdRule("a")
	rule.Repos = []forge.RepoRef{runRepo, other}
	b2 := runBuiltinRule()
	b2.Repos = []forge.RepoRef{other}
	src := &runSource{
		subs: []runSub{
			{hold: true, events: []forge.ChangeEvent{runEvent(1, 1), runEvent(2, 1), runEvent(3, 1)}},
			{hold: true, events: []forge.ChangeEvent{runEvent(1, 1), runEvent(2, 1), runEvent(3, 1)}},
		},
		changes: map[int]forge.Change{1: runChange(1, "me", "b")},
	}
	rc := &runRecorder{delay: 5 * time.Millisecond}
	stop := runStart(t, Config{Rules: []Rule{rule, runBuiltinRule(), b2}}, src, rc, runViewer("me"), nil)
	defer stop()
	runEventually(t, "all actions", func() bool {
		c, rm, rec := rc.snapshot()
		return len(c) >= 6 && len(rm) >= 6 && len(rec) >= 2
	})
	if m := rc.maxActive.Load(); m != 1 {
		t.Errorf("max concurrent actions = %d, want 1", m)
	}
}

func TestNewViewerResolution(t *testing.T) {
	needs := Config{Rules: []Rule{runCmdRule("a")}}
	boom := func(context.Context) (string, error) { return "", errors.New("gh missing") }
	if _, err := New(Options{Config: needs, Source: &runSource{}, Viewer: boom}); err == nil {
		t.Error("want an error when the viewer is needed and cannot be resolved")
	}
	if _, err := New(Options{Config: needs, Source: &runSource{}}); err == nil {
		t.Error("want an error with no resolver")
	}
	if _, err := New(Options{Config: needs, Source: &runSource{}, Viewer: runViewer("")}); err == nil {
		t.Error("want an error for an empty login")
	}

	explicit := runCmdRule("a")
	explicit.Author = "alice"
	if _, err := New(Options{Config: Config{Rules: []Rule{explicit}}, Source: &runSource{}, Viewer: boom}); err != nil {
		t.Errorf("explicit author must not need the viewer: %v", err)
	}

	called := false
	spy := func(context.Context) (string, error) { called = true; return "other", nil }
	r, err := New(Options{Config: Config{Viewer: "cfg", Rules: needs.Rules}, Source: &runSource{}, Viewer: spy})
	if err != nil {
		t.Fatal(err)
	}
	if called || r.viewer != "cfg" {
		t.Errorf("resolver called=%v viewer=%q, want the config viewer without a call", called, r.viewer)
	}
}

func TestRunnerCancelReturnsNilWhileBackingOff(t *testing.T) {
	src := &runSource{subs: []runSub{{err: errors.New("down")}, {err: errors.New("down")}}}
	r, err := New(Options{Config: Config{Viewer: "me", Rules: []Rule{runCmdRule("a")}}, Source: src, Log: runLog()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	runEventually(t, "first subscribe", func() bool { return len(src.subscribeCalls()) >= 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly during the real backoff sleep")
	}
}

func TestRunnerRunsRealCommand(t *testing.T) {
	out := filepath.Join(t.TempDir(), "rec.json")
	t.Setenv(cmdHelperEnv, out)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	rule := runCmdRule("real")
	rule.Command = []string{os.Args[0], "pr-{number}", "{branch}"}
	rule.Timeout = 10 * time.Second
	src := &runSource{
		subs:    []runSub{{hold: true, events: []forge.ChangeEvent{runEvent(1, 5)}}},
		changes: map[int]forge.Change{5: runChange(5, "me", "feat/x")},
	}
	stop := runStart(t, Config{Rules: []Rule{rule}}, src, nil, runViewer("me"), nil)
	defer stop()
	var rec cmdHelperRecord
	runEventually(t, "helper record", func() bool {
		b, err := os.ReadFile(out)
		return err == nil && json.Unmarshal(b, &rec) == nil
	})
	if len(rec.Args) != 2 || rec.Args[0] != "pr-5" || rec.Args[1] != "feat/x" || rec.Env["FORGED_REPO"] != "github.com/o/r" {
		t.Fatalf("record = %+v", rec)
	}
}
