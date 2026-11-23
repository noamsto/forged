package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
)

// watch streams NDJSON: one line per current matching change, then one per
// Event. Subscribing before the snapshot read means no update is missed; the
// events the read itself publishes are dropped as repeats of the last line sent.
func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, err := repoParam(q)
	if err != nil {
		writeError(w, err)
		return
	}
	f := cache.Filter{Repo: repo}
	k := forge.RepoOpen(repo)
	if v := q.Get("number"); v != "" {
		if f.Number, err = strconv.Atoi(v); err != nil || f.Number <= 0 {
			writeError(w, badf("invalid number %q", v))
			return
		}
		k = forge.ChangeKey(repo, f.Number)
	}

	events, cancel := s.c.Subscribe(f)
	defer cancel()
	res, err := s.c.Read(r.Context(), k, cache.ReadOpts{Focus: true})
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher := http.NewResponseController(w)
	last := map[forge.Key]forge.Change{}
	send := func(ev forge.Event) bool {
		if prev, ok := last[ev.Key]; ok && reflect.DeepEqual(prev, ev.Change) {
			return true
		}
		last[ev.Key] = ev.Change
		if enc.Encode(ev) != nil {
			return false
		}
		return flusher.Flush() == nil
	}
	for _, c := range res.Changes {
		if !send(forge.Event{Key: forge.ChangeKey(c.Repo, c.Number), Change: c}) {
			return
		}
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
