package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/cache"
)

var watchableTypes = []forge.EventType{
	forge.EventChecksStarted, forge.EventChecksCompleted, forge.EventReviewSubmitted,
	forge.EventReviewDecisionChanged, forge.EventStateChanged, forge.EventDraftChanged,
	forge.EventHeadChanged, forge.EventMergeableChanged,
}

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

	if v := q.Get("types"); v != "" {
		for _, name := range strings.Split(v, ",") {
			t := forge.EventType(name)
			if !slices.Contains(watchableTypes, t) {
				writeError(w, badf("unknown event type %q", name))
				return
			}
			f.Types = append(f.Types, t)
		}
	}
	if v := q.Get("since"); v != "" {
		if f.Since, err = strconv.ParseUint(v, 10, 64); err != nil {
			writeError(w, badf("invalid since %q", v))
			return
		}
		f.HasSince = true
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
	// Headers go out now so the client's request returns before the first event.
	if flusher.Flush() != nil {
		return
	}
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
