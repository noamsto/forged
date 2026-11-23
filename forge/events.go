package forge

import "time"

type EventType string

const (
	EventChecksStarted         EventType = "checks.started"
	EventChecksCompleted       EventType = "checks.completed"
	EventReviewSubmitted       EventType = "review.submitted"
	EventReviewDecisionChanged EventType = "review_decision.changed"
	EventStateChanged          EventType = "state.changed"
	EventDraftChanged          EventType = "draft.changed"
	EventHeadChanged           EventType = "head.changed"
	EventMergeableChanged      EventType = "mergeable.changed"
	// EventResync marks a stream gap; Diff never produces it.
	EventResync EventType = "resync"
)

type ChangeEvent struct {
	Seq    uint64    `json:"seq"`
	Type   EventType `json:"type"`
	Repo   RepoRef   `json:"repo"`
	Number int       `json:"number"`
	URL    string    `json:"url"`
	Old    string    `json:"old"`
	New    string    `json:"new"`
	At     time.Time `json:"at"`
	Review *Review   `json:"review,omitempty"`
}

// Diff returns the typed events that turn old into cur, in a fixed order.
// Seq and At are left zero for the caller to stamp.
func Diff(old, cur Change) []ChangeEvent {
	var evs []ChangeEvent
	emit := func(t EventType, o, n string) *ChangeEvent {
		evs = append(evs, ChangeEvent{Type: t, Repo: cur.Repo, Number: cur.Number, URL: cur.URL, Old: o, New: n})
		return &evs[len(evs)-1]
	}

	oldRollup, curRollup := old.Checks.Rollup, cur.Checks.Rollup
	if oldRollup != RollupPending && curRollup == RollupPending {
		emit(EventChecksStarted, string(oldRollup), string(RollupPending))
	}
	if (curRollup == RollupPass || curRollup == RollupFail) &&
		(oldRollup != curRollup || old.Head.SHA != cur.Head.SHA) {
		emit(EventChecksCompleted, string(oldRollup), string(curRollup))
	}

	type reviewKey struct {
		login string
		at    time.Time
	}
	seen := make(map[reviewKey]struct{}, len(old.Reviews))
	for _, r := range old.Reviews {
		seen[reviewKey{r.Author.Login, r.SubmittedAt}] = struct{}{}
	}
	for _, r := range cur.Reviews {
		if _, ok := seen[reviewKey{r.Author.Login, r.SubmittedAt}]; ok || r.State == "pending" {
			continue
		}
		rv := r
		emit(EventReviewSubmitted, "", r.State).Review = &rv
	}

	if old.ReviewDecision != cur.ReviewDecision {
		emit(EventReviewDecisionChanged, string(old.ReviewDecision), string(cur.ReviewDecision))
	}
	if old.State != cur.State {
		emit(EventStateChanged, string(old.State), string(cur.State))
	}
	if old.Draft != cur.Draft {
		o, n := "ready", "draft"
		if old.Draft {
			o, n = n, o
		}
		emit(EventDraftChanged, o, n)
	}
	if old.Head.SHA != cur.Head.SHA {
		emit(EventHeadChanged, old.Head.SHA, cur.Head.SHA)
	}
	if old.Mergeable != cur.Mergeable {
		emit(EventMergeableChanged, string(old.Mergeable), string(cur.Mergeable))
	}
	return evs
}
