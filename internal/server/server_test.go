package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/client"
	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
	"github.com/noamsto/forge-cache/internal/clock"
	"github.com/noamsto/forge-cache/internal/forgetest"
	"github.com/noamsto/forge-cache/internal/server"
)

var repo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

type harness struct {
	sock  string
	cl    *client.Client
	fake  *forgetest.Fake
	clk   *clock.Fake
	cache *cache.Cache
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func setup(t *testing.T) *harness {
	t.Helper()
	return setupWith(t, func(*cache.Options) {})
}

func setupWith(t *testing.T, tune func(*cache.Options)) *harness {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	fake := forgetest.New()
	o := cache.DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	tune(&o)
	c := cache.New(map[string]forge.Provider{"github/github.com": fake}, o)

	sock := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server.New(c, "v-test")}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &harness{sock: sock, cl: client.New(sock), fake: fake, clk: clk, cache: c}
}

func change(n int, branch string) forge.Change {
	c := forge.Change{
		Repo:   repo,
		Number: n,
		Title:  "change",
		State:  forge.StateOpen,
		Checks: forge.Checks{Rollup: forge.RollupPass},
	}
	c.Head.Ref = branch
	return c
}

func rawGet(t *testing.T, sock, path string) int {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := hc.Get("http://forge-cache" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestRoutesReturnFakeData(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.fake.SetChanges(repo, []forge.Change{change(1, "feat-a"), change(2, "feat-b")})
	h.fake.SetSearch("is:open", []forge.Change{change(2, "feat-b")})

	got, err := h.cl.Change(ctx, repo, 2, client.ReadOptions{})
	if err != nil || got.Number != 2 {
		t.Fatalf("Change = %+v, %v", got, err)
	}
	open, err := h.cl.ListOpen(ctx, repo, client.ReadOptions{})
	if err != nil || len(open) != 2 {
		t.Fatalf("ListOpen = %d changes, %v", len(open), err)
	}
	br, err := h.cl.ByBranch(ctx, repo, "feat-a", client.ReadOptions{})
	if err != nil || len(br) != 1 || br[0].Number != 1 {
		t.Fatalf("ByBranch = %+v, %v", br, err)
	}
	found, err := h.cl.Search(ctx, "github", "is:open", 10, client.ReadOptions{})
	if err != nil || len(found) != 1 || found[0].Number != 2 {
		t.Fatalf("Search = %+v, %v", found, err)
	}
}

func TestByBranchEmptyIsNotAnError(t *testing.T) {
	h := setup(t)
	got, err := h.cl.ByBranch(context.Background(), repo, "nope", client.ReadOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("ByBranch = %+v, %v", got, err)
	}
}

func TestMaxAgeParam(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.fake.SetChanges(repo, []forge.Change{change(1, "b")})
	zero, tenMin := time.Duration(0), 10*time.Minute

	if _, err := h.cl.Change(ctx, repo, 1, client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Calls("GetMany"); n != 1 {
		t.Fatalf("calls after first read = %d, want 1", n)
	}
	if _, err := h.cl.Change(ctx, repo, 1, client.ReadOptions{MaxAge: &tenMin}); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Calls("GetMany"); n != 1 {
		t.Fatalf("calls with max_age=10m = %d, want 1 (memory)", n)
	}
	if _, err := h.cl.Change(ctx, repo, 1, client.ReadOptions{MaxAge: &zero}); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Calls("GetMany"); n != 2 {
		t.Fatalf("calls with max_age=0 = %d, want 2", n)
	}
}

func TestFocusReachesReadOpts(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.fake.SetChanges(repo, []forge.Change{change(1, "b")})

	if _, err := h.cl.Change(ctx, repo, 1, client.ReadOptions{Focus: true}); err != nil {
		t.Fatal(err)
	}
	// A focused open change is hot: the scheduler refetches it at HotInterval.
	h.clk.Advance(cache.DefaultOptions().HotInterval)
	h.cache.Tick(ctx)
	if n := h.fake.Calls("GetMany"); n != 2 {
		t.Fatalf("GetMany calls = %d, want 2 (focus made it hot)", n)
	}
}

func TestNotFoundMapsToErrNotFound(t *testing.T) {
	h := setup(t)
	_, err := h.cl.Change(context.Background(), repo, 404, client.ReadOptions{})
	if !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpstreamErrorIs502(t *testing.T) {
	h := setup(t)
	h.fake.FailRepo(repo, errors.New("boom"))
	_, err := h.cl.Change(context.Background(), repo, 1, client.ReadOptions{})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want APIError 502", err)
	}
}

func TestUnknownHostIs501(t *testing.T) {
	h := setup(t)
	other := forge.RepoRef{Forge: "gitlab", Host: "gitlab.com", Owner: "o", Name: "r"}
	_, err := h.cl.ListOpen(context.Background(), other, client.ReadOptions{})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotImplemented {
		t.Fatalf("err = %v, want APIError 501", err)
	}
}

func TestBadParamsAre400(t *testing.T) {
	h := setup(t)
	for _, path := range []string{
		"/v1/change",
		"/v1/change?repo=github.com/o/r",
		"/v1/change?repo=github.com/o/r&number=abc",
		"/v1/change?repo=bad&number=1",
		"/v1/changes",
		"/v1/changes?repo=github.com/o/r&max_age=soon",
		"/v1/search?forge=github",
		"/v1/search?forge=github&q=x&limit=zz",
		"/v1/watch",
	} {
		if got := rawGet(t, h.sock, path); got != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, got)
		}
	}
}

func TestInvalidateRefetches(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	h.fake.SetChanges(repo, []forge.Change{change(1, "b")})
	if _, err := h.cl.Change(ctx, repo, 1, client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	before := h.fake.Calls("GetMany")

	if err := h.cl.Invalidate(ctx, forge.ChangeKey(repo, 1)); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.Calls("GetMany"); n != before+1 {
		t.Fatalf("GetMany calls = %d, want %d", n, before+1)
	}
}

func TestStatusReportsVersionAndStats(t *testing.T) {
	h := setup(t)
	h.fake.SetChanges(repo, []forge.Change{change(1, "b")})
	if _, err := h.cl.Change(context.Background(), repo, 1, client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := h.cl.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "v-test" {
		t.Fatalf("version = %q", st.Version)
	}
	var stats cache.Stats
	if err := json.Unmarshal(st.Stats, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Keys != 1 {
		t.Fatalf("stats.Keys = %d, want 1", stats.Keys)
	}
}

// seed is the cursor below every event the cache will emit.
func (h *harness) seed() uint64 { return uint64(h.clk.Now().UnixMicro()) }

// primeChange stores change 1 so later updates produce events.
func (h *harness) primeChange(t *testing.T) forge.Change {
	t.Helper()
	c := change(1, "b")
	h.fake.SetChanges(repo, []forge.Change{c})
	if _, err := h.cl.Change(context.Background(), repo, 1, client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	return c
}

// update stores c on the fake and refetches it, emitting the resulting events.
func (h *harness) update(t *testing.T, c forge.Change) {
	t.Helper()
	h.fake.SetChanges(repo, []forge.Change{c})
	if err := h.cl.Invalidate(context.Background(), forge.ChangeKey(repo, c.Number)); err != nil {
		t.Fatal(err)
	}
}

func watchCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWatchStreamsTypedEvents(t *testing.T) {
	h := setup(t)
	c := h.primeChange(t)

	ch, err := h.cl.Subscribe(watchCtx(t), client.SubscribeOptions{Repo: repo, Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.Draft = true
	h.update(t, c)
	ev := recv(t, ch)
	if ev.Type != forge.EventDraftChanged || ev.Number != 1 || ev.Repo != repo || ev.New != "draft" || ev.Seq <= h.seed() {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchTypesFilter(t *testing.T) {
	h := setup(t)
	c := h.primeChange(t)

	ch, err := h.cl.Subscribe(watchCtx(t), client.SubscribeOptions{
		Repo: repo, Number: 1, Types: []forge.EventType{forge.EventStateChanged, forge.EventHeadChanged},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Draft = true
	c.State = forge.StateMerged
	h.update(t, c)
	if ev := recv(t, ch); ev.Type != forge.EventStateChanged || ev.New != string(forge.StateMerged) {
		t.Fatalf("first event = %+v, want state.changed", ev)
	}

	c.Draft = false
	c.Head.SHA = "abc"
	h.update(t, c)
	if ev := recv(t, ch); ev.Type != forge.EventHeadChanged {
		t.Fatalf("second event = %+v, want head.changed", ev)
	}
}

func TestWatchSinceReplays(t *testing.T) {
	h := setup(t)
	c := h.primeChange(t)
	c.Draft = true
	h.update(t, c)
	c.State = forge.StateMerged
	h.update(t, c)

	ch, err := h.cl.Subscribe(watchCtx(t), client.SubscribeOptions{Repo: repo, Number: 1, Since: ptr(h.seed())})
	if err != nil {
		t.Fatal(err)
	}
	first, second := recv(t, ch), recv(t, ch)
	if first.Type != forge.EventDraftChanged || second.Type != forge.EventStateChanged || second.Seq <= first.Seq {
		t.Fatalf("replay = %+v, %+v", first, second)
	}

	ch, err = h.cl.Subscribe(watchCtx(t), client.SubscribeOptions{Repo: repo, Number: 1, Since: ptr(first.Seq)})
	if err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, ch); ev.Seq != second.Seq {
		t.Fatalf("replay after first = %+v, want seq %d", ev, second.Seq)
	}
}

func TestWatchSinceBelowRingResyncsFirst(t *testing.T) {
	h := setupWith(t, func(o *cache.Options) { o.EventBuffer = 1 })
	c := h.primeChange(t)
	c.Draft = true
	h.update(t, c)
	c.State = forge.StateMerged
	h.update(t, c)

	ch, err := h.cl.Subscribe(watchCtx(t), client.SubscribeOptions{Repo: repo, Number: 1, Since: ptr(h.seed())})
	if err != nil {
		t.Fatal(err)
	}
	gap, kept := recv(t, ch), recv(t, ch)
	if gap.Type != forge.EventResync || kept.Type != forge.EventStateChanged || gap.Seq != kept.Seq-1 {
		t.Fatalf("stream = %+v, %+v", gap, kept)
	}
}

func TestWatchRejectsBadParams(t *testing.T) {
	h := setup(t)
	for path, want := range map[string]int{
		"/v1/watch?repo=github.com/o/r&types=bogus":                    http.StatusBadRequest,
		"/v1/watch?repo=github.com/o/r&types=state.changed,bogus":      http.StatusBadRequest,
		"/v1/watch?repo=github.com/o/r&types=resync":                   http.StatusBadRequest,
		"/v1/watch?repo=github.com/o/r&since=abc":                      http.StatusBadRequest,
		"/v1/watch?repo=github.com/o/r&since=-1":                       http.StatusBadRequest,
		"/v1/watch?repo=gitlab.com/o/r":                                http.StatusNotImplemented,
		"/v1/watch?repo=gitlab.com/o/r&types=state.changed&since=1234": http.StatusNotImplemented,
	} {
		if got := rawGet(t, h.sock, path); got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func recv(t *testing.T, ch <-chan forge.ChangeEvent) forge.ChangeEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
	return forge.ChangeEvent{}
}

func TestUnavailableSocket(t *testing.T) {
	cl := client.New(filepath.Join(shortDir(t), "missing.sock"))
	_, err := cl.Status(context.Background())
	if !errors.Is(err, client.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
