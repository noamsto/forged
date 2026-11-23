// Package ghcompat serves a subset of `gh pr view` / `gh pr list` from the
// forged daemon, falling back to the real gh for everything else.
package ghcompat

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/forged/forge"
)

// fieldShapers is the single table of supported --json fields: Parse
// validates against its keys and Shape renders through its values.
var fieldShapers = map[string]func(forge.Change) any{
	"number":           func(c forge.Change) any { return c.Number },
	"url":              func(c forge.Change) any { return c.URL },
	"title":            func(c forge.Change) any { return c.Title },
	"state":            func(c forge.Change) any { return strings.ToUpper(string(c.State)) },
	"isDraft":          func(c forge.Change) any { return c.Draft },
	"headRefName":      func(c forge.Change) any { return c.Head.Ref },
	"headRefOid":       func(c forge.Change) any { return c.Head.SHA },
	"baseRefName":      func(c forge.Change) any { return c.Base.Ref },
	"mergeable":        func(c forge.Change) any { return shapeMergeable(c.Mergeable) },
	"mergeStateStatus": func(c forge.Change) any { return strings.ToUpper(c.MergeState) },
	"reviewDecision":   func(c forge.Change) any { return shapeReviewDecision(c.ReviewDecision) },
	"statusCheckRollup": func(c forge.Change) any {
		return shapeRollup(c.Checks.Runs)
	},
	"autoMergeRequest": func(c forge.Change) any { return shapeAutoMerge(c.AutoMerge) },
	"updatedAt":        func(c forge.Change) any { return formatTime(c.UpdatedAt) },
	"author":           func(c forge.Change) any { return shapeActor(c.Author) },
}

// Shape renders the requested fields of c in gh's --json shape.
func Shape(c forge.Change, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if shape, ok := fieldShapers[f]; ok {
			out[f] = shape(c)
		}
	}
	return out
}

// SelectList filters by state ("all" keeps everything), orders by number
// descending and truncates to limit.
func SelectList(cs []forge.Change, state string, limit int) []forge.Change {
	out := make([]forge.Change, 0, len(cs))
	for _, c := range cs {
		if state == "all" || string(c.State) == state {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b forge.Change) int { return b.Number - a.Number })
	if limit < len(out) {
		out = out[:limit]
	}
	return out
}

// Encode writes v as gh does: compact with sorted keys, HTML unescaped; a TTY
// gets two-space indentation.
func Encode(w io.Writer, v any, tty bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if tty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func shapeMergeable(m forge.Mergeable) string {
	switch m {
	case forge.MergeableClean:
		return "MERGEABLE"
	case forge.MergeableConflict:
		return "CONFLICTING"
	default:
		return "UNKNOWN"
	}
}

func shapeReviewDecision(d forge.ReviewDecision) string {
	if d == forge.ReviewNone {
		return ""
	}
	return strings.ToUpper(string(d))
}

func shapeActor(a forge.Actor) map[string]any {
	if a.IsBot {
		return map[string]any{"is_bot": true, "login": "app/" + a.Login}
	}
	return map[string]any{"id": a.ID, "is_bot": false, "login": a.Login, "name": a.Name}
}

func shapeRollup(runs []forge.CheckRun) []any {
	out := make([]any, 0, len(runs))
	for _, r := range runs {
		if r.Kind == "status" {
			state := r.Conclusion
			if state == "" {
				state = r.Status
			}
			out = append(out, map[string]any{
				"__typename": "StatusContext",
				"context":    r.Name,
				"state":      strings.ToUpper(state),
				"targetUrl":  r.URL,
				"startedAt":  formatTime(r.StartedAt),
			})
			continue
		}
		out = append(out, map[string]any{
			"__typename":   "CheckRun",
			"name":         r.Name,
			"workflowName": r.Workflow,
			"status":       strings.ToUpper(r.Status),
			"conclusion":   strings.ToUpper(r.Conclusion),
			"startedAt":    formatTime(r.StartedAt),
			"completedAt":  formatTime(r.CompletedAt),
			"detailsUrl":   r.URL,
		})
	}
	return out
}

func shapeAutoMerge(a *forge.AutoMerge) any {
	if a == nil {
		return nil
	}
	return map[string]any{
		"authorEmail":    a.AuthorEmail,
		"commitBody":     a.CommitBody,
		"commitHeadline": a.CommitHeadline,
		"mergeMethod":    strings.ToUpper(a.Method),
		"enabledAt":      formatTime(a.EnabledAt),
		"enabledBy":      shapeActor(a.EnabledBy),
	}
}
