// Package github implements forge.Provider for GitHub over GraphQL (batched
// reads, lists, search) and REST (conditional probe).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/noamsto/forged/forge"
)

const (
	defaultGraphQLURL = "https://api.github.com/graphql"
	defaultRESTURL    = "https://api.github.com"
	maxBatch          = 50
	maxErrBody        = 200
)

type Options struct {
	GraphQLURL, RESTURL string
	HTTPClient          *http.Client
	Token               func(context.Context) (string, error)
}

const (
	defaultTimeout    = 30 * time.Second
	defaultRetryAfter = time.Minute
)

type Provider struct {
	graphqlURL  string
	restURL     string
	client      *http.Client
	probeClient *http.Client

	probedMu sync.Mutex
	probed   map[forge.RepoRef]struct{}
}

var _ forge.Provider = (*Provider)(nil)

func New(o Options) *Provider {
	if o.GraphQLURL == "" {
		o.GraphQLURL = defaultGraphQLURL
	}
	if o.RESTURL == "" {
		o.RESTURL = defaultRESTURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	if o.Token == nil {
		o.Token = TokenFromEnvOrGh
	}
	client := *o.HTTPClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	tokens := &tokenCache{fetch: o.Token}
	client.Transport = &authTransport{base: base, tokens: tokens}
	return &Provider{
		graphqlURL:  o.GraphQLURL,
		restURL:     o.RESTURL,
		client:      &client,
		probeClient: probeClient(&client, base, tokens),
		probed:      map[forge.RepoRef]struct{}{},
	}
}

func (p *Provider) Capabilities() forge.Capabilities {
	return forge.Capabilities{Events: false, ConditionalRequests: true, MaxBatch: maxBatch}
}

// tokenCache memoizes the token; both transports share one so a 401 refresh
// reaches them together.
type tokenCache struct {
	fetch func(context.Context) (string, error)

	mu     sync.Mutex
	cached string
}

func (c *tokenCache) get(ctx context.Context, refresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == "" || refresh {
		tok, err := c.fetch(ctx)
		if err != nil {
			return "", err
		}
		c.cached = tok
	}
	return c.cached, nil
}

// authTransport adds the bearer token and re-reads it once on a 401.
type authTransport struct {
	base   http.RoundTripper
	tokens *tokenCache
}

func (t *authTransport) get(ctx context.Context, refresh bool) (string, error) {
	return t.tokens.get(ctx, refresh)
}

func (t *authTransport) send(req *http.Request, token string) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(r)
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.get(req.Context(), false)
	if err != nil {
		return nil, err
	}
	resp, err := t.send(req, token)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.GetBody == nil) {
		return resp, err
	}
	token, err = t.get(req.Context(), true)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	return t.send(retry, token)
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// post sends one GraphQL request and returns the decoded envelope.
func (p *Provider) post(ctx context.Context, query string, vars map[string]any) (gqlResponse, error) {
	var out gqlResponse
	reqBody, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.graphqlURL, bytes.NewReader(reqBody))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		if rl := rateLimitError(resp); rl != nil {
			return out, rl
		}
		return out, fmt.Errorf("graphql: %s: %s", resp.Status, truncate(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("graphql: parse response: %w", err)
	}
	// GraphQL reports an exhausted budget on a 200.
	if resp.Header.Get("X-RateLimit-Remaining") == "0" || slices.ContainsFunc(out.Errors, func(e gqlError) bool { return e.Type == "RATE_LIMITED" }) {
		if rl := headerRateLimit(resp.Header); rl != nil {
			return gqlResponse{}, rl
		}
		return gqlResponse{}, &forge.RateLimitError{RetryAfter: time.Now().Add(defaultRetryAfter)}
	}
	return out, nil
}

func rateLimitError(resp *http.Response) *forge.RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	return headerRateLimit(resp.Header)
}

func headerRateLimit(h http.Header) *forge.RateLimitError {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return &forge.RateLimitError{RetryAfter: time.Now().Add(time.Duration(s) * time.Second)}
	}
	if h.Get("X-RateLimit-Remaining") != "0" {
		return nil
	}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		return &forge.RateLimitError{RetryAfter: time.Unix(reset, 0)}
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > maxErrBody {
		b = b[:maxErrBody]
	}
	return string(bytes.TrimSpace(b))
}

// firstError turns the first GraphQL error into an error, wrapping NOT_FOUND.
func firstError(errs []gqlError) error {
	if len(errs) == 0 {
		return nil
	}
	if errs[0].Type == "NOT_FOUND" {
		return fmt.Errorf("%w: %s", forge.ErrNotFound, errs[0].Message)
	}
	return fmt.Errorf("graphql: %s", errs[0].Message)
}

func (p *Provider) Get(ctx context.Context, r forge.RepoRef, number int) (forge.Change, error) {
	ref := forge.ChangeRef{Repo: r, Number: number}
	got, err := p.GetMany(ctx, []forge.ChangeRef{ref})
	if err != nil {
		return forge.Change{}, err
	}
	f := got[ref]
	return f.Change, f.Err
}
