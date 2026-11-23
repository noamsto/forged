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

	"github.com/noamsto/forge-cache/client"
	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
	"github.com/noamsto/forge-cache/internal/clock"
	"github.com/noamsto/forge-cache/internal/forgetest"
	"github.com/noamsto/forge-cache/internal/server"
)

var repo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
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
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
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
		json.NewEncoder(w).Encode(map[string]any{"changes": []forge.Change{}})
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
		w.Write([]byte(`{"error":"upstream said no"}`))
	}))
	_, err := client.New(sock).ListOpen(context.Background(), repo, client.ReadOptions{})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Message != "upstream said no" {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultSocket(t *testing.T) {
	t.Setenv("FORGE_CACHE_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	if got, want := client.DefaultSocket(), "/run/user/1/forge-cache/forge-cache.sock"; got != want {
		t.Fatalf("DefaultSocket = %q, want %q", got, want)
	}
	t.Setenv("FORGE_CACHE_SOCKET", "/x/y.sock")
	if got := client.DefaultSocket(); got != "/x/y.sock" {
		t.Fatalf("DefaultSocket with override = %q", got)
	}
}
