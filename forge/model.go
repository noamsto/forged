// Package forge defines the forge-agnostic change model and the Provider
// interface. It holds no forge-specific types: provider packages translate
// their wire formats into these.
package forge

import (
	"fmt"
	"strings"
	"time"
)

const (
	forgeGitHub = "github"
	hostGitHub  = "github.com"
)

type RepoRef struct {
	Forge string `json:"forge"`
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

func (r RepoRef) String() string { return r.Host + "/" + r.Owner + "/" + r.Name }

// ParseRepo accepts "owner/name" (host defaults to github.com) or "host/owner/name".
func ParseRepo(s string) (RepoRef, error) {
	parts := strings.Split(s, "/")
	host := hostGitHub
	switch len(parts) {
	case 2:
	case 3:
		host, parts = parts[0], parts[1:]
	default:
		return RepoRef{}, fmt.Errorf("invalid repo %q: want owner/name or host/owner/name", s)
	}
	if host == "" || parts[0] == "" || parts[1] == "" {
		return RepoRef{}, fmt.Errorf("invalid repo %q: empty component", s)
	}
	return RepoRef{Forge: forgeGitHub, Host: host, Owner: parts[0], Name: parts[1]}, nil
}

type State string

const (
	StateOpen   State = "open"
	StateClosed State = "closed"
	StateMerged State = "merged"
)

type Mergeable string

const (
	MergeableClean    Mergeable = "clean"
	MergeableConflict Mergeable = "conflict"
	MergeableUnknown  Mergeable = "unknown"
)

type ReviewDecision string

const (
	ReviewApproved         ReviewDecision = "approved"
	ReviewChangesRequested ReviewDecision = "changes_requested"
	ReviewRequired         ReviewDecision = "review_required"
	ReviewNone             ReviewDecision = "none"
)

type Rollup string

const (
	RollupPass    Rollup = "pass"
	RollupFail    Rollup = "fail"
	RollupPending Rollup = "pending"
	RollupNone    Rollup = "none"
)

type Actor struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	ID    string `json:"id"`
	IsBot bool   `json:"isBot"`
}

type Change struct {
	Repo   RepoRef `json:"repo"`
	Number int     `json:"number"`
	URL    string  `json:"url"`
	Title  string  `json:"title"`
	Author Actor   `json:"author"`
	State  State   `json:"state"`
	Draft  bool    `json:"draft"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Mergeable      Mergeable      `json:"mergeable"`
	MergeState     string         `json:"mergeState"`
	ReviewDecision ReviewDecision `json:"reviewDecision"`
	Checks         Checks         `json:"checks"`
	AutoMerge      *AutoMerge     `json:"autoMerge,omitempty"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

func (c Change) IsOpen() bool { return c.State == StateOpen }

func (c Change) IsTerminal() bool { return c.State == StateMerged || c.State == StateClosed }

type Checks struct {
	Rollup    Rollup     `json:"rollup"`
	Runs      []CheckRun `json:"runs"`
	Truncated bool       `json:"truncated"`
}

type CheckRun struct {
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Workflow    string    `json:"workflow"`
	URL         string    `json:"url"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt"`
}

type AutoMerge struct {
	Method         string    `json:"method"`
	EnabledAt      time.Time `json:"enabledAt"`
	EnabledBy      Actor     `json:"enabledBy"`
	AuthorEmail    *string   `json:"authorEmail,omitempty"`
	CommitHeadline *string   `json:"commitHeadline,omitempty"`
	CommitBody     *string   `json:"commitBody,omitempty"`
}

// Event reports that the stored value of a change differs from the previous one.
type Event struct {
	Key    Key    `json:"key"`
	Change Change `json:"change"`
}
