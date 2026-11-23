package github

import (
	"context"
	"fmt"
	"io"
	"net/http"

	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	"github.com/bored-engineer/github-conditional-http-transport/memory"

	"github.com/noamsto/forged/forge"
)

// probeClient puts auth outside the conditional-request cache: GitHub varies
// ETags on Authorization, so the cache must see the header to revalidate.
func probeClient(base *http.Client, rt http.RoundTripper, tokens *tokenCache) *http.Client {
	c := *base
	c.Transport = &authTransport{base: ghtransport.NewTransport(memory.NewStorage(), rt), tokens: tokens}
	return &c
}

// Probe revalidates the first page of open PRs. Changed is true unless the
// server answered 304 against a baseline stored by an earlier probe.
func (p *Provider) Probe(ctx context.Context, r forge.RepoRef) (forge.ProbeResult, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&sort=updated&direction=desc&per_page=100", p.restURL, r.Owner, r.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return forge.ProbeResult{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.probeClient.Do(req)
	if err != nil {
		return forge.ProbeResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if rl := rateLimitError(resp); rl != nil {
			return forge.ProbeResult{}, rl
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return forge.ProbeResult{}, fmt.Errorf("probe %s: %s: %s", r, resp.Status, truncate(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	// With no stored response the transport guesses an empty-array ETag; a hit
	// on that guess proves the list is empty, which is unchanged only against
	// an earlier probe's baseline.
	seen := p.markProbed(r)
	hit := resp.Header.Get("X-Cache") == "HIT"
	return forge.ProbeResult{Changed: !hit || !seen}, nil
}

// markProbed records r as probed and reports whether it already was.
func (p *Provider) markProbed(r forge.RepoRef) bool {
	p.probedMu.Lock()
	defer p.probedMu.Unlock()
	_, seen := p.probed[r]
	p.probed[r] = struct{}{}
	return seen
}
