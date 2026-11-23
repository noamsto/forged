package github

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

const changeFields = `number url title author{__typename login ...on User{id name}} state isDraft headRefName headRefOid baseRefName mergeable mergeStateStatus reviewDecision updatedAt ` +
	`autoMergeRequest{authorEmail commitBody commitHeadline mergeMethod enabledAt enabledBy{login ...on User{id name}}} ` +
	`commits(last:1){nodes{commit{statusCheckRollup{state contexts(first:100){pageInfo{hasNextPage} nodes{__typename ` +
	`...on CheckRun{name status conclusion startedAt completedAt detailsUrl checkSuite{workflowRun{workflow{name}}}} ` +
	`...on StatusContext{context state targetUrl createdAt}}}}}}}`

type wireActor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

type wireContext struct {
	Typename    string     `json:"__typename"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	DetailsURL  string     `json:"detailsUrl"`
	CheckSuite  struct {
		WorkflowRun *struct {
			Workflow struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   string     `json:"context"`
	State     string     `json:"state"`
	TargetURL string     `json:"targetUrl"`
	CreatedAt *time.Time `json:"createdAt"`
}

type wirePR struct {
	Number           int        `json:"number"`
	URL              string     `json:"url"`
	Title            string     `json:"title"`
	Author           *wireActor `json:"author"`
	State            string     `json:"state"`
	IsDraft          bool       `json:"isDraft"`
	HeadRefName      string     `json:"headRefName"`
	HeadRefOid       string     `json:"headRefOid"`
	BaseRefName      string     `json:"baseRefName"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	ReviewDecision   string     `json:"reviewDecision"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	AutoMergeRequest *struct {
		AuthorEmail    *string    `json:"authorEmail"`
		CommitBody     *string    `json:"commitBody"`
		CommitHeadline *string    `json:"commitHeadline"`
		MergeMethod    string     `json:"mergeMethod"`
		EnabledAt      *time.Time `json:"enabledAt"`
		EnabledBy      *wireActor `json:"enabledBy"`
	} `json:"autoMergeRequest"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []wireContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func mapChange(repo forge.RepoRef, w wirePR) forge.Change {
	c := forge.Change{
		Repo:           repo,
		Number:         w.Number,
		URL:            w.URL,
		Title:          w.Title,
		Author:         mapActor(w.Author),
		State:          forge.State(strings.ToLower(w.State)),
		Draft:          w.IsDraft,
		Mergeable:      mapMergeable(w.Mergeable),
		MergeState:     strings.ToLower(w.MergeStateStatus),
		ReviewDecision: mapReview(w.ReviewDecision),
		UpdatedAt:      w.UpdatedAt,
		Checks:         forge.Checks{Rollup: forge.RollupNone},
	}
	c.Head.Ref, c.Head.SHA, c.Base.Ref = w.HeadRefName, w.HeadRefOid, w.BaseRefName
	if am := w.AutoMergeRequest; am != nil {
		c.AutoMerge = &forge.AutoMerge{
			Method:         strings.ToLower(am.MergeMethod),
			EnabledAt:      timeOrZero(am.EnabledAt),
			EnabledBy:      mapActor(am.EnabledBy),
			AuthorEmail:    am.AuthorEmail,
			CommitHeadline: am.CommitHeadline,
			CommitBody:     am.CommitBody,
		}
	}
	if len(w.Commits.Nodes) > 0 {
		if r := w.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			c.Checks.Rollup = mapRollup(r.State)
			c.Checks.Truncated = r.Contexts.PageInfo.HasNextPage
			for _, n := range r.Contexts.Nodes {
				c.Checks.Runs = append(c.Checks.Runs, mapRun(n))
			}
		}
	}
	return c
}

func mapActor(a *wireActor) forge.Actor {
	if a == nil {
		return forge.Actor{}
	}
	return forge.Actor{Login: a.Login, Name: a.Name, ID: a.ID, IsBot: a.Typename == "Bot"}
}

func mapMergeable(s string) forge.Mergeable {
	switch s {
	case "MERGEABLE":
		return forge.MergeableClean
	case "CONFLICTING":
		return forge.MergeableConflict
	default:
		return forge.MergeableUnknown
	}
}

func mapReview(s string) forge.ReviewDecision {
	if s == "" {
		return forge.ReviewNone
	}
	return forge.ReviewDecision(strings.ToLower(s))
}

func mapRollup(s string) forge.Rollup {
	switch s {
	case "SUCCESS":
		return forge.RollupPass
	case "FAILURE", "ERROR":
		return forge.RollupFail
	case "PENDING", "EXPECTED":
		return forge.RollupPending
	default:
		return forge.RollupNone
	}
}

func mapRun(n wireContext) forge.CheckRun {
	if n.Typename == "StatusContext" {
		run := forge.CheckRun{Kind: "status", Name: n.Context, URL: n.TargetURL, StartedAt: timeOrZero(n.CreatedAt)}
		switch n.State {
		case "PENDING", "EXPECTED":
			run.Status = "pending"
		default:
			run.Status, run.Conclusion = "completed", strings.ToLower(n.State)
		}
		return run
	}
	run := forge.CheckRun{
		Kind:        "check",
		Name:        n.Name,
		URL:         n.DetailsURL,
		Status:      strings.ToLower(n.Status),
		Conclusion:  strings.ToLower(n.Conclusion),
		StartedAt:   timeOrZero(n.StartedAt),
		CompletedAt: timeOrZero(n.CompletedAt),
	}
	if wr := n.CheckSuite.WorkflowRun; wr != nil {
		run.Workflow = wr.Workflow.Name
	}
	return run
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// repoFromURL derives the repo from a pull request URL (https://host/owner/name/pull/N).
func repoFromURL(raw string) (forge.RepoRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return forge.RepoRef{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return forge.RepoRef{}, fmt.Errorf("unexpected pull request url %q", raw)
	}
	return forge.RepoRef{Forge: "github", Host: u.Host, Owner: parts[0], Name: parts[1]}, nil
}
