package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/noamsto/forged/forge"
)

type probeServer struct {
	mu   sync.Mutex
	etag string
	body string
	// reqs records If-None-Match per request.
	reqs []string
}

func (s *probeServer) set(etag, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.etag, s.body = etag, body
}

func (s *probeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Header.Get("If-None-Match"))
	if r.URL.Path != "/repos/acme/alpha/pulls" {
		http.NotFound(w, r)
		return
	}
	if got := r.URL.RawQuery; got != "state=open&sort=updated&direction=desc&per_page=100" {
		http.Error(w, "bad query "+got, http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "bearer test-token" {
		http.Error(w, "no auth", http.StatusUnauthorized)
		return
	}
	if s.etag != "" && r.Header.Get("If-None-Match") == s.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(s.body))
}

func newProbeProvider(t *testing.T, h http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Options{
		GraphQLURL: srv.URL + "/graphql",
		RESTURL:    srv.URL,
		HTTPClient: srv.Client(),
		Token:      func(context.Context) (string, error) { return "test-token", nil },
	})
}

func probeChanged(t *testing.T, p *Provider) bool {
	t.Helper()
	res, err := p.Probe(context.Background(), alpha)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return res.Changed
}

func TestProbeChangedThenUnchangedThenChanged(t *testing.T) {
	s := &probeServer{}
	s.set(`"abc"`, `[{"number":1}]`)
	p := newProbeProvider(t, s)

	if !probeChanged(t, p) {
		t.Error("probe #1: Changed = false, want true (no baseline)")
	}
	if probeChanged(t, p) {
		t.Error("probe #2: Changed = true, want false (304 revalidated)")
	}
	if got := s.reqs[1]; got != `"abc"` {
		t.Errorf("probe #2 If-None-Match = %q, want %q", got, `"abc"`)
	}
	s.set(`"def"`, `[{"number":1},{"number":2}]`)
	if !probeChanged(t, p) {
		t.Error("probe #3: Changed = false, want true (new etag)")
	}
	if probeChanged(t, p) {
		t.Error("probe #4: Changed = true, want false")
	}
}

func TestProbeSpeculativeHitIsChanged(t *testing.T) {
	p := newProbeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "" {
			t.Error("transport sent no speculative If-None-Match")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	if !probeChanged(t, p) {
		t.Error("speculative 304: Changed = false, want true")
	}
	if !probeChanged(t, p) {
		t.Error("speculative 304 again: Changed = false, want true (never a stored baseline)")
	}
}

func TestProbeFirstPerRepoIsChanged(t *testing.T) {
	p := newProbeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
		if r.Header.Get("If-None-Match") == `"x"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[{"number":9}]`))
	}))
	for _, repo := range []forge.RepoRef{alpha, beta} {
		for i, want := range []bool{true, false} {
			res, err := p.Probe(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			if res.Changed != want {
				t.Errorf("%s probe #%d: Changed = %v, want %v", repo, i+1, res.Changed, want)
			}
		}
	}
}

func TestProbeRateLimited(t *testing.T) {
	p := newProbeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	_, err := p.Probe(context.Background(), alpha)
	var rl *forge.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *forge.RateLimitError", err)
	}
}

func TestProbeServerError(t *testing.T) {
	p := newProbeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	if _, err := p.Probe(context.Background(), alpha); err == nil {
		t.Fatal("want error on 500")
	}
}
