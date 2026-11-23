// Package server exposes the cache over HTTP+JSON.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/cache"
)

type server struct {
	c       *cache.Cache
	version string
	closing chan struct{}
	once    sync.Once
}

// Handler is the daemon's HTTP handler.
type Handler struct {
	http.Handler
	s *server
}

// EndWatches ends every open watch stream and any started later, leaving other
// in-flight requests to finish. It is safe to call more than once.
func (h *Handler) EndWatches() { h.s.once.Do(func() { close(h.s.closing) }) }

// New returns the daemon's HTTP handler.
func New(c *cache.Cache, version string) *Handler {
	s := &server{c: c, version: version, closing: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/changes", s.changes)
	mux.HandleFunc("GET /v1/change", s.change)
	mux.HandleFunc("GET /v1/search", s.search)
	mux.HandleFunc("POST /v1/invalidate", s.invalidate)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/watch", s.watch)
	return &Handler{Handler: mux, s: s}
}

const (
	defaultSearchLimit = 30
	maxSearchLimit     = 100
)

type badRequest struct{ error }

func badf(format string, a ...any) error { return badRequest{fmt.Errorf(format, a...)} }

type response struct {
	Changes   []forge.Change `json:"changes"`
	FetchedAt time.Time      `json:"fetchedAt"`
	Negative  bool           `json:"negative"`
}

func (s *server) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, err := repoParam(q)
	if err != nil {
		writeError(w, err)
		return
	}
	k := forge.RepoOpen(repo)
	if head := q.Get("head"); head != "" {
		k = forge.ByBranch(repo, head)
	}
	s.read(w, r, k)
}

func (s *server) change(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, err := repoParam(q)
	if err != nil {
		writeError(w, err)
		return
	}
	n, err := strconv.Atoi(q.Get("number"))
	if err != nil {
		writeError(w, badf("invalid number %q", q.Get("number")))
		return
	}
	if !validNumber(n) {
		writeError(w, badf("invalid number %q", q.Get("number")))
		return
	}
	s.read(w, r, forge.ChangeKey(repo, n))
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fg, query := q.Get("forge"), q.Get("q")
	if fg == "" || query == "" {
		writeError(w, badf("forge and q are required"))
		return
	}
	limit := defaultSearchLimit
	if v := q.Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || !validLimit(limit) {
			writeError(w, badf("invalid limit %q", v))
			return
		}
	}
	s.read(w, r, forge.Query(fg, query, limit))
}

func (s *server) read(w http.ResponseWriter, r *http.Request, k forge.Key) {
	opts, err := readOpts(r.URL.Query())
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := s.c.Read(r.Context(), k, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response{Changes: nonNil(res.Changes), FetchedAt: res.FetchedAt, Negative: res.Negative})
}

func (s *server) invalidate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key forge.Key `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, badf("invalid body: %v", err))
		return
	}
	if err := validateKey(body.Key); err != nil {
		writeError(w, err)
		return
	}
	if err := s.c.Invalidate(r.Context(), body.Key); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.version, "stats": s.c.Stats()})
}

func validNumber(n int) bool { return n >= 1 && n <= math.MaxInt32 }

func validLimit(n int) bool { return n >= 1 && n <= maxSearchLimit }

func validateKey(k forge.Key) error {
	switch k.Kind {
	case forge.KindRepoOpen, forge.KindChange, forge.KindByBranch:
		if k.Repo.Owner == "" || k.Repo.Name == "" {
			return badf("key needs repo owner and name")
		}
	case forge.KindQuery:
		if k.Repo.Forge == "" || k.Query == "" || !validLimit(k.Limit) {
			return badf("query key needs forge, query and limit 1..%d", maxSearchLimit)
		}
	default:
		return badf("unknown key kind %q", k.Kind)
	}
	if k.Kind == forge.KindChange && !validNumber(k.Number) {
		return badf("invalid change number %d", k.Number)
	}
	if k.Kind == forge.KindByBranch && k.Branch == "" {
		return badf("by-branch key needs a branch")
	}
	return nil
}

func repoParam(q url.Values) (forge.RepoRef, error) {
	repo, err := forge.ParseRepo(q.Get("repo"))
	if err != nil {
		return forge.RepoRef{}, badRequest{err}
	}
	return repo, nil
}

func readOpts(q url.Values) (cache.ReadOpts, error) {
	o := cache.ReadOpts{Focus: q.Get("focus") == "1"}
	v := q.Get("max_age")
	if v == "" {
		return o, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, serr := strconv.Atoi(v)
		if serr != nil {
			return o, badf("invalid max_age %q", v)
		}
		d = time.Duration(secs) * time.Second
	}
	o.MaxAge, o.MaxAgeSet = d, true
	return o, nil
}

func nonNil(c []forge.Change) []forge.Change {
	if c == nil {
		return []forge.Change{}
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var br badRequest
	switch {
	case errors.As(err, &br):
		status = http.StatusBadRequest
	case errors.Is(err, forge.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, forge.ErrUnsupported):
		status = http.StatusNotImplemented
	}
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(err.Error())})
}
