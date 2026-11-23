package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
)

// watch streams NDJSON, one forge.ChangeEvent per line, until the client
// leaves or the cache drops a subscriber that fell behind.
func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, err := repoParam(q)
	if err != nil {
		writeError(w, err)
		return
	}
	f := cache.Filter{Repo: repo}
	if v := q.Get("number"); v != "" {
		if f.Number, err = strconv.Atoi(v); err != nil || f.Number <= 0 {
			writeError(w, badf("invalid number %q", v))
			return
		}
	}

	events, cancel, err := s.c.Subscribe(f)
	if err != nil {
		writeError(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher := http.NewResponseController(w)
	send := func(ev forge.ChangeEvent) bool {
		if enc.Encode(ev) != nil {
			return false
		}
		return flusher.Flush() == nil
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok || !send(ev) {
				return
			}
		}
	}
}
