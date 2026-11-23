package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/cache"
	"github.com/noamsto/forged/internal/clock"
	"github.com/noamsto/forged/internal/forgetest"
	"github.com/noamsto/forged/internal/server"
)

var repo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestRoundTripAgainstRealServer(t *testing.T) {
	fake := forgetest.New()
	o := cache.DefaultOptions()
	o.Clock = clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	o.BatchWindow = 0
	c := cache.New(map[string]forge.Provider{"github/github.com": fake}, o)
	fake.SetChanges(repo, []forge.Change{{Repo: repo, Number: 7, Title: "t", State: forge.StateOpen}})

	cl := client.New(serve(t, server.New(c, "v1")))
	got, err := cl.Change(context.Background(), repo, 7, client.ReadOptions{})
	if err != nil || got.Number != 7 || got.Title != "t" {
		t.Fatalf("Change = %+v, %v", got, err)
	}
}

type captured struct {
	mu    sync.Mutex
	path  string
	query url.Values
}

func (c *captured) get() (string, url.Values) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.query
}

func TestRequestParams(t *testing.T) {
	var got captured
	sock := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.path, got.query = r.URL.Path, r.URL.Query()
		got.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"changes": []forge.Change{}})
	}))
	cl := client.New(sock)
	ctx := context.Background()
	maxAge := 90 * time.Second

	if _, err := cl.ListOpen(ctx, repo, client.ReadOptions{MaxAge: &maxAge, Focus: true}); err != nil {
		t.Fatal(err)
	}
	path, q := got.get()
	if path != "/v1/changes" || q.Get("repo") != "github.com/o/r" || q.Get("state") != "open" || q.Get("focus") != "1" || q.Get("max_age") == "" {
		t.Fatalf("ListOpen request = %s?%v", path, q)
	}

	if _, err := cl.ByBranch(ctx, repo, "feat/x", client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	path, q = got.get()
	if path != "/v1/changes" || q.Get("head") != "feat/x" || q.Has("max_age") || q.Has("focus") {
		t.Fatalf("ByBranch request = %s?%v", path, q)
	}
}

func TestAPIErrorCarriesStatusAndMessage(t *testing.T) {
	sock := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream said no"}`))
	}))
	_, err := client.New(sock).ListOpen(context.Background(), repo, client.ReadOptions{})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Message != "upstream said no" {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultSocket(t *testing.T) {
	t.Setenv("FORGED_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	if got, want := client.DefaultSocket(), "/run/user/1/forged/forged.sock"; got != want {
		t.Fatalf("DefaultSocket = %q, want %q", got, want)
	}
	t.Setenv("FORGED_SOCKET", "/x/y.sock")
	if got := client.DefaultSocket(); got != "/x/y.sock" {
		t.Fatalf("DefaultSocket with override = %q", got)
	}
}

func TestSubscribeSendsParamsAndClosesWhenStreamEnds(t *testing.T) {
	var got captured
	sock := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.path, got.query = r.URL.Path, r.URL.Query()
		got.mu.Unlock()
		enc := json.NewEncoder(w)
		_ = enc.Encode(forge.ChangeEvent{Seq: 8, Type: forge.EventStateChanged, Repo: repo, Number: 3, New: "merged"})
		_ = enc.Encode(forge.ChangeEvent{Seq: 9, Type: forge.EventResync})
	}))
	cl := client.New(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	since := uint64(7)
	ch, err := cl.Subscribe(ctx, client.SubscribeOptions{
		Repo: repo, Number: 3, Since: &since,
		Types: []forge.EventType{forge.EventStateChanged, forge.EventResync},
	})
	if err != nil {
		t.Fatal(err)
	}
	var evs []forge.ChangeEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	if len(evs) != 2 || evs[0].Seq != 8 || evs[0].New != "merged" || evs[1].Type != forge.EventResync {
		t.Fatalf("events = %+v", evs)
	}
	path, q := got.get()
	if path != "/v1/watch" || q.Get("repo") != "github.com/o/r" || q.Get("number") != "3" ||
		q.Get("types") != "state.changed,resync" || q.Get("since") != "7" {
		t.Fatalf("request = %s?%v", path, q)
	}

	zero := uint64(0)
	if _, err := cl.Subscribe(ctx, client.SubscribeOptions{Repo: repo, Since: &zero}); err != nil {
		t.Fatal(err)
	}
	if _, q := got.get(); q.Get("since") != "0" || q.Has("number") || q.Has("types") {
		t.Fatalf("zero-since request = %v", q)
	}
	if _, err := cl.Subscribe(ctx, client.SubscribeOptions{Repo: repo}); err != nil {
		t.Fatal(err)
	}
	if _, q := got.get(); q.Has("since") {
		t.Fatalf("nil-since request = %v", q)
	}
}

func TestSubscribeAgainstRealServerHonoursSince(t *testing.T) {
	fake := forgetest.New()
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	o := cache.DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	c := cache.New(map[string]forge.Provider{"github/github.com": fake}, o)
	cl := client.New(serve(t, server.New(c, "v1")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch := forge.Change{Repo: repo, Number: 7, State: forge.StateOpen}
	fake.SetChanges(repo, []forge.Change{ch})
	if _, err := cl.Change(ctx, repo, 7, client.ReadOptions{}); err != nil {
		t.Fatal(err)
	}
	ch.State = forge.StateMerged
	fake.SetChanges(repo, []forge.Change{ch})
	if err := cl.Invalidate(ctx, forge.ChangeKey(repo, 7)); err != nil {
		t.Fatal(err)
	}

	since := uint64(clk.Now().UnixMicro())
	events, err := cl.Subscribe(ctx, client.SubscribeOptions{Repo: repo, Number: 7, Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Type != forge.EventStateChanged || ev.Number != 7 {
			t.Fatalf("event = %+v, ok=%v", ev, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no replayed event")
	}
}
