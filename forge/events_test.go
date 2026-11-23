package forge_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

var diffRepo = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

func baseChange() forge.Change {
	c := forge.Change{
		Repo:           diffRepo,
		Number:         7,
		URL:            "https://github.com/o/r/pull/7",
		State:          forge.StateOpen,
		Mergeable:      forge.MergeableClean,
		ReviewDecision: forge.ReviewNone,
	}
	c.Head.SHA = "aaa"
	c.Checks.Rollup = forge.RollupPass
	return c
}

func with(mut func(*forge.Change)) forge.Change {
	c := baseChange()
	mut(&c)
	return c
}

func review(login, state string, at time.Time) forge.Review {
	return forge.Review{Author: forge.Actor{Login: login}, State: state, SubmittedAt: at}
}

type wantEvent struct {
	typ      forge.EventType
	old, new string
}

func TestDiff(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rollup := func(r forge.Rollup) func(*forge.Change) {
		return func(c *forge.Change) { c.Checks.Rollup = r }
	}
	rollupSHA := func(r forge.Rollup, sha string) func(*forge.Change) {
		return func(c *forge.Change) { c.Checks.Rollup = r; c.Head.SHA = sha }
	}

	tests := []struct {
		name     string
		old, cur forge.Change
		want     []wantEvent
	}{
		{"identical", baseChange(), baseChange(), nil},
		{"checks started from pass", baseChange(), with(rollup(forge.RollupPending)),
			[]wantEvent{{forge.EventChecksStarted, "pass", "pending"}}},
		{"checks started from none", with(rollup(forge.RollupNone)), with(rollup(forge.RollupPending)),
			[]wantEvent{{forge.EventChecksStarted, "none", "pending"}}},
		{"pending stays pending", with(rollup(forge.RollupPending)), with(rollup(forge.RollupPending)), nil},
		{"completed pending to pass", with(rollup(forge.RollupPending)), baseChange(),
			[]wantEvent{{forge.EventChecksCompleted, "pending", "pass"}}},
		{"completed pending to fail", with(rollup(forge.RollupPending)), with(rollup(forge.RollupFail)),
			[]wantEvent{{forge.EventChecksCompleted, "pending", "fail"}}},
		{"completed none to pass (fast CI)", with(rollup(forge.RollupNone)), baseChange(),
			[]wantEvent{{forge.EventChecksCompleted, "none", "pass"}}},
		{"completed fail to pass (rerun)", with(rollup(forge.RollupFail)), baseChange(),
			[]wantEvent{{forge.EventChecksCompleted, "fail", "pass"}}},
		{"completed pass to pass with new head",
			with(rollupSHA(forge.RollupPass, "aaa")), with(rollupSHA(forge.RollupPass, "bbb")),
			[]wantEvent{{forge.EventChecksCompleted, "pass", "pass"}, {forge.EventHeadChanged, "aaa", "bbb"}}},
		{"pass to pass same head", baseChange(), baseChange(), nil},
		{"pass to none emits nothing", baseChange(), with(rollup(forge.RollupNone)), nil},
		{"decision changed", baseChange(), with(func(c *forge.Change) { c.ReviewDecision = forge.ReviewApproved }),
			[]wantEvent{{forge.EventReviewDecisionChanged, "none", "approved"}}},
		{"merged", baseChange(), with(func(c *forge.Change) { c.State = forge.StateMerged }),
			[]wantEvent{{forge.EventStateChanged, "open", "merged"}}},
		{"closed", baseChange(), with(func(c *forge.Change) { c.State = forge.StateClosed }),
			[]wantEvent{{forge.EventStateChanged, "open", "closed"}}},
		{"reopened",
			with(func(c *forge.Change) { c.State = forge.StateClosed }), baseChange(),
			[]wantEvent{{forge.EventStateChanged, "closed", "open"}}},
		{"draft to ready",
			with(func(c *forge.Change) { c.Draft = true }), baseChange(),
			[]wantEvent{{forge.EventDraftChanged, "draft", "ready"}}},
		{"ready to draft",
			baseChange(), with(func(c *forge.Change) { c.Draft = true }),
			[]wantEvent{{forge.EventDraftChanged, "ready", "draft"}}},
		{"head changed", with(rollupSHA(forge.RollupNone, "aaa")), with(rollupSHA(forge.RollupNone, "bbb")),
			[]wantEvent{{forge.EventHeadChanged, "aaa", "bbb"}}},
		{"mergeable changed", baseChange(), with(func(c *forge.Change) { c.Mergeable = forge.MergeableConflict }),
			[]wantEvent{{forge.EventMergeableChanged, "clean", "conflict"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forge.Diff(tt.old, tt.cur)
			var gotSummary []wantEvent
			for _, e := range got {
				gotSummary = append(gotSummary, wantEvent{e.Type, e.Old, e.New})
				if e.Seq != 0 || !e.At.IsZero() {
					t.Errorf("Seq/At must be zero, got %d / %v", e.Seq, e.At)
				}
				if e.Repo != tt.cur.Repo || e.Number != tt.cur.Number || e.URL != tt.cur.URL {
					t.Errorf("identity not taken from cur: %+v", e)
				}
			}
			if !reflect.DeepEqual(gotSummary, tt.want) {
				t.Errorf("events = %v, want %v", gotSummary, tt.want)
			}
		})
	}

	t.Run("two new reviews", func(t *testing.T) {
		old := with(func(c *forge.Change) { c.Reviews = []forge.Review{review("a", "commented", t0)} })
		cur := with(func(c *forge.Change) {
			c.Reviews = []forge.Review{
				review("a", "commented", t0),
				review("b", "approved", t0.Add(time.Minute)),
				review("c", "changes_requested", t0.Add(2*time.Minute)),
			}
		})
		got := forge.Diff(old, cur)
		if len(got) != 2 {
			t.Fatalf("got %d events, want 2: %+v", len(got), got)
		}
		for i, w := range []struct{ login, state string }{{"b", "approved"}, {"c", "changes_requested"}} {
			e := got[i]
			if e.Type != forge.EventReviewSubmitted || e.Old != "" || e.New != w.state {
				t.Errorf("event %d = %+v", i, e)
			}
			if e.Review == nil || e.Review.Author.Login != w.login {
				t.Errorf("event %d review = %+v, want author %s", i, e.Review, w.login)
			}
		}
	})

	t.Run("pending review ignored", func(t *testing.T) {
		cur := with(func(c *forge.Change) { c.Reviews = []forge.Review{review("a", "pending", t0)} })
		if got := forge.Diff(baseChange(), cur); len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})

	t.Run("same author new submission is a new review", func(t *testing.T) {
		old := with(func(c *forge.Change) { c.Reviews = []forge.Review{review("a", "commented", t0)} })
		cur := with(func(c *forge.Change) { c.Reviews = []forge.Review{review("a", "approved", t0.Add(time.Hour))} })
		got := forge.Diff(old, cur)
		if len(got) != 1 || got[0].Type != forge.EventReviewSubmitted || got[0].New != "approved" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("unchanged reviews emit nothing", func(t *testing.T) {
		c := with(func(c *forge.Change) { c.Reviews = []forge.Review{review("a", "approved", t0)} })
		if got := forge.Diff(c, c); len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})

	t.Run("multi-field change follows table order", func(t *testing.T) {
		old := with(func(c *forge.Change) {
			c.Checks.Rollup = forge.RollupPending
			c.Draft = true
		})
		cur := with(func(c *forge.Change) {
			c.Checks.Rollup = forge.RollupFail
			c.Reviews = []forge.Review{review("a", "approved", t0)}
			c.ReviewDecision = forge.ReviewApproved
			c.State = forge.StateMerged
			c.Head.SHA = "bbb"
			c.Mergeable = forge.MergeableConflict
		})
		cur.Draft = true
		old.Draft = false
		var types []forge.EventType
		for _, e := range forge.Diff(old, cur) {
			types = append(types, e.Type)
		}
		want := []forge.EventType{
			forge.EventChecksCompleted,
			forge.EventReviewSubmitted,
			forge.EventReviewDecisionChanged,
			forge.EventStateChanged,
			forge.EventDraftChanged,
			forge.EventHeadChanged,
			forge.EventMergeableChanged,
		}
		if !reflect.DeepEqual(types, want) {
			t.Errorf("order = %v, want %v", types, want)
		}
	})
}

func TestEventTypeValues(t *testing.T) {
	for typ, want := range map[forge.EventType]string{
		forge.EventChecksStarted:         "checks.started",
		forge.EventChecksCompleted:       "checks.completed",
		forge.EventReviewSubmitted:       "review.submitted",
		forge.EventReviewDecisionChanged: "review_decision.changed",
		forge.EventStateChanged:          "state.changed",
		forge.EventDraftChanged:          "draft.changed",
		forge.EventHeadChanged:           "head.changed",
		forge.EventMergeableChanged:      "mergeable.changed",
		forge.EventResync:                "resync",
	} {
		if string(typ) != want {
			t.Errorf("got %q, want %q", typ, want)
		}
	}
}

func TestChangeEventJSONKeys(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := forge.ChangeEvent{
		Seq:    42,
		Type:   forge.EventReviewSubmitted,
		Repo:   diffRepo,
		Number: 7,
		URL:    "https://github.com/o/r/pull/7",
		Old:    "",
		New:    "approved",
		At:     at,
		Review: &forge.Review{Author: forge.Actor{Login: "a"}, State: "approved", SubmittedAt: at},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "type", "repo", "number", "url", "old", "new", "at", "review"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q in %s", k, b)
		}
	}
	if len(m) != 9 {
		t.Errorf("got %d keys, want 9: %s", len(m), b)
	}
	if m["seq"] != float64(42) || m["type"] != "review.submitted" {
		t.Errorf("seq/type = %v / %v", m["seq"], m["type"])
	}

	var back forge.ChangeEvent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev, back) {
		t.Errorf("round trip = %+v, want %+v", back, ev)
	}
}
