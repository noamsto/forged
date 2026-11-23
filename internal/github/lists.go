package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/noamsto/forged/forge"
)

const (
	listPageSize = 50
	listCap      = 1000
	byBranchSize = 100
)

const listQuery = `query($o:String!,$n:String!,$c:String){repository(owner:$o,name:$n){` +
	`pullRequests(states:OPEN,first:50,after:$c,orderBy:{field:CREATED_AT,direction:DESC}){` +
	`pageInfo{hasNextPage endCursor} nodes{` + changeFields + `}}}}`

const byBranchQuery = `query($o:String!,$n:String!,$b:String!){repository(owner:$o,name:$n){` +
	`pullRequests(headRefName:$b,first:100,states:[OPEN,CLOSED,MERGED],orderBy:{field:CREATED_AT,direction:DESC}){` +
	`nodes{` + changeFields + `}}}}`

const searchQuery = `query($q:String!,$n:Int!){search(query:$q,type:ISSUE,first:$n){nodes{...on PullRequest{` +
	changeFields + `}}}}`

type pullRequests struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []wirePR `json:"nodes"`
}

// repoPullRequests decodes a response shaped {repository{pullRequests{...}}}.
func repoPullRequests(resp gqlResponse) (pullRequests, error) {
	var data struct {
		Repository *struct {
			PullRequests pullRequests `json:"pullRequests"`
		} `json:"repository"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return pullRequests{}, fmt.Errorf("graphql: parse data: %w", err)
		}
	}
	if data.Repository == nil {
		if err := firstError(resp.Errors); err != nil {
			return pullRequests{}, err
		}
		return pullRequests{}, fmt.Errorf("%w: repository", forge.ErrNotFound)
	}
	return data.Repository.PullRequests, nil
}

func mapAll(repo forge.RepoRef, nodes []wirePR) []forge.Change {
	changes := make([]forge.Change, 0, len(nodes))
	for _, w := range nodes {
		changes = append(changes, mapChange(repo, w))
	}
	return changes
}

func (p *Provider) ListOpen(ctx context.Context, r forge.RepoRef) ([]forge.Change, error) {
	var changes []forge.Change
	vars := map[string]any{"o": r.Owner, "n": r.Name}
	for len(changes) < listCap {
		resp, err := p.post(ctx, listQuery, vars)
		if err != nil {
			return nil, err
		}
		page, err := repoPullRequests(resp)
		if err != nil {
			return nil, err
		}
		changes = append(changes, mapAll(r, page.Nodes)...)
		if !page.PageInfo.HasNextPage {
			break
		}
		vars["c"] = page.PageInfo.EndCursor
	}
	slices.SortFunc(changes, func(a, b forge.Change) int { return b.Number - a.Number })
	return changes, nil
}

func (p *Provider) ByBranch(ctx context.Context, r forge.RepoRef, branch string) ([]forge.Change, error) {
	resp, err := p.post(ctx, byBranchQuery, map[string]any{"o": r.Owner, "n": r.Name, "b": branch})
	if err != nil {
		return nil, err
	}
	page, err := repoPullRequests(resp)
	if err != nil {
		return nil, err
	}
	return mapAll(r, page.Nodes), nil
}

// Search skips nodes that are not pull requests (they decode as empty objects).
func (p *Provider) Search(ctx context.Context, query string, limit int) ([]forge.Change, error) {
	resp, err := p.post(ctx, searchQuery, map[string]any{"q": query, "n": limit})
	if err != nil {
		return nil, err
	}
	var data struct {
		Search struct {
			Nodes []wirePR `json:"nodes"`
		} `json:"search"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		if ferr := firstError(resp.Errors); ferr != nil {
			return nil, ferr
		}
		return nil, fmt.Errorf("graphql: parse data: %w", err)
	}
	var changes []forge.Change
	for _, w := range data.Search.Nodes {
		if w.Number == 0 {
			continue
		}
		repo, err := repoFromURL(w.URL)
		if err != nil {
			return nil, err
		}
		changes = append(changes, mapChange(repo, w))
	}
	return changes, nil
}
