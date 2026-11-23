package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/noamsto/forge-cache/forge"
)

// repoGroup is one repository's slice of a batch: the PR numbers selected under
// a single aliased repository(...) block.
type repoGroup struct {
	Repo    forge.RepoRef
	Numbers []int
}

// chunkRefs groups refs by repo (sorted by repo string, numbers sorted and
// deduplicated) and packs them into chunks of at most max selections. A repo's
// numbers may straddle two chunks.
func chunkRefs(refs []forge.ChangeRef, max int) [][]repoGroup {
	byRepo := map[forge.RepoRef][]int{}
	for _, r := range refs {
		byRepo[r.Repo] = append(byRepo[r.Repo], r.Number)
	}
	repos := make([]forge.RepoRef, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	slices.SortFunc(repos, func(a, b forge.RepoRef) int { return cmp.Compare(a.String(), b.String()) })

	var chunks [][]repoGroup
	var cur []repoGroup
	room := max
	for _, repo := range repos {
		ns := slices.Compact(slices.Sorted(slices.Values(byRepo[repo])))
		for len(ns) > 0 {
			if room == 0 {
				chunks = append(chunks, cur)
				cur, room = nil, max
			}
			take := min(room, len(ns))
			cur = append(cur, repoGroup{Repo: repo, Numbers: ns[:take]})
			ns = ns[take:]
			room -= take
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// buildBatchQuery aliases one repository block per group, each holding one
// aliased PR lookup per number. Owner and name travel as variables, so nothing
// ref-derived is spliced into the query text; numbers are ints.
func buildBatchQuery(groups []repoGroup) (string, map[string]any) {
	vars := make(map[string]any, 2*len(groups))
	var decl, body strings.Builder
	for i, g := range groups {
		if i > 0 {
			decl.WriteByte(',')
		}
		fmt.Fprintf(&decl, "$o%d:String!,$n%d:String!", i, i)
		vars[fmt.Sprintf("o%d", i)] = g.Repo.Owner
		vars[fmt.Sprintf("n%d", i)] = g.Repo.Name
		fmt.Fprintf(&body, "r%d:repository(owner:$o%d,name:$n%d){", i, i, i)
		for _, n := range g.Numbers {
			fmt.Fprintf(&body, "pr%d:pullRequest(number:%d){%s}", n, n, changeFields)
		}
		body.WriteByte('}')
	}
	return "query(" + decl.String() + "){" + body.String() + "}", vars
}

// GetMany resolves refs in as few requests as the chunk cap allows. Every ref
// is present in the result; failures are per ref, so the error is always nil.
func (p *Provider) GetMany(ctx context.Context, refs []forge.ChangeRef) (map[forge.ChangeRef]forge.Fetched, error) {
	out := make(map[forge.ChangeRef]forge.Fetched, len(refs))
	for _, chunk := range chunkRefs(refs, maxBatch) {
		p.fetchChunk(ctx, chunk, out)
	}
	return out, nil
}

func (p *Provider) fetchChunk(ctx context.Context, chunk []repoGroup, out map[forge.ChangeRef]forge.Fetched) {
	set := func(g repoGroup, n int, f forge.Fetched) { out[forge.ChangeRef{Repo: g.Repo, Number: n}] = f }
	failAll := func(err error) {
		for _, g := range chunk {
			for _, n := range g.Numbers {
				set(g, n, forge.Fetched{Err: err})
			}
		}
	}

	query, vars := buildBatchQuery(chunk)
	resp, err := p.post(ctx, query, vars)
	if err != nil {
		failAll(err)
		return
	}
	var data map[string]json.RawMessage
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			failAll(fmt.Errorf("graphql: parse data: %w", err))
			return
		}
	}
	if len(data) == 0 {
		err := firstError(resp.Errors)
		if err == nil {
			err = errors.New("graphql: empty response")
		}
		failAll(err)
		return
	}

	for i, g := range chunk {
		alias := fmt.Sprintf("r%d", i)
		block := data[alias]
		if len(block) == 0 || string(block) == "null" {
			err := fmt.Errorf("graphql: %s: %s", g.Repo, repoErrorMessage(resp.Errors, alias))
			for _, n := range g.Numbers {
				set(g, n, forge.Fetched{Err: err})
			}
			continue
		}
		var prs map[string]json.RawMessage
		if err := json.Unmarshal(block, &prs); err != nil {
			err = fmt.Errorf("graphql: parse %s: %w", g.Repo, err)
			for _, n := range g.Numbers {
				set(g, n, forge.Fetched{Err: err})
			}
			continue
		}
		for _, n := range g.Numbers {
			raw := prs[fmt.Sprintf("pr%d", n)]
			if len(raw) == 0 || string(raw) == "null" {
				set(g, n, forge.Fetched{Err: forge.ErrNotFound})
				continue
			}
			var w wirePR
			if err := json.Unmarshal(raw, &w); err != nil {
				set(g, n, forge.Fetched{Err: fmt.Errorf("graphql: parse %s#%d: %w", g.Repo, n, err)})
				continue
			}
			set(g, n, forge.Fetched{Change: mapChange(g.Repo, w)})
		}
	}
}

// repoErrorMessage prefers an error scoped to the repository alias itself; an
// error without a path is the fallback.
func repoErrorMessage(errs []gqlError, alias string) string {
	var unscoped string
	for _, e := range errs {
		switch {
		case len(e.Path) == 1 && e.Path[0] == alias:
			return e.Message
		case len(e.Path) == 0 && unscoped == "":
			unscoped = e.Message
		}
	}
	return cmp.Or(unscoped, "repository not resolved")
}
