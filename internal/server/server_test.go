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
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func setup(t *testing.T) *harness {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	fake := forgetest.New()
	o := cache.DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	c := cache.New(map[string]forge.Provider{"github/github.com": fake}, o)

	sock := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server.New(c, "v-test")}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
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
	defer resp.Body.Close()
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

func TestWatchSnapshotThenEvent(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.fake.SetChanges(repo, []forge.Change{change(1, "b")})

	ch, err := h.cl.Watch(ctx, repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	ev := recv(t, ch)
	if ev.Change.Number != 1 || ev.Change.Title != "change" {
		t.Fatalf("snapshot event = %+v", ev)
	}

	updated := change(1, "b")
	updated.Title = "renamed"
	h.fake.SetChanges(repo, []forge.Change{updated})
	if err := h.cl.Invalidate(ctx, forge.ChangeKey(repo, 1)); err != nil {
		t.Fatal(err)
	}
	ev = recv(t, ch)
	if ev.Change.Title != "renamed" {
		t.Fatalf("update event = %+v", ev)
	}
}

func recv(t *testing.T, ch <-chan forge.Event) forge.Event {
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
	return forge.Event{}
}

func TestUnavailableSocket(t *testing.T) {
	cl := client.New(filepath.Join(shortDir(t), "missing.sock"))
	_, err := cl.Status(context.Background())
	if !errors.Is(err, client.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}
