// Package client talks to the forge-cache daemon over its unix socket.
//
//	cl := client.New("") // DefaultSocket()
//	maxAge := time.Minute
//	c, err := cl.Change(ctx, forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}, 7,
//		client.ReadOptions{MaxAge: &maxAge})
//	if errors.Is(err, client.ErrUnavailable) {
//		// daemon not running: fall back to the forge directly
//	}
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

const callTimeout = 15 * time.Second

var (
	// ErrUnavailable means the daemon could not be reached or did not answer in time.
	ErrUnavailable = errors.New("forge-cache daemon unavailable")
	// ErrNotFound means the daemon answered 404 for the requested change.
	ErrNotFound = errors.New("not found")
)

// APIError is a non-2xx daemon response other than 404.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("forge-cache: %d: %s", e.Status, e.Message) }

// ReadOptions tunes freshness. A nil MaxAge uses the daemon default.
type ReadOptions struct {
	MaxAge *time.Duration
	Focus  bool
}

// Status is the daemon's build version and cache statistics.
type Status struct {
	Version string          `json:"version"`
	Stats   json.RawMessage `json:"stats"`
}

type Client struct {
	hc *http.Client
}

// DefaultSocket returns $FORGE_CACHE_SOCKET, else $XDG_RUNTIME_DIR/forge-cache/forge-cache.sock.
func DefaultSocket() string {
	if s := os.Getenv("FORGE_CACHE_SOCKET"); s != "" {
		return s
	}
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "forge-cache", "forge-cache.sock")
}

// New returns a client for the daemon at socket; empty means DefaultSocket().
func New(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket()
	}
	return &Client{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

const base = "http://forge-cache"

func (o ReadOptions) apply(q url.Values) {
	if o.MaxAge != nil {
		q.Set("max_age", o.MaxAge.String())
	}
	if o.Focus {
		q.Set("focus", "1")
	}
}

type changesResponse struct {
	Changes []forge.Change `json:"changes"`
}

// Change returns one change, or ErrNotFound.
func (c *Client) Change(ctx context.Context, repo forge.RepoRef, n int, o ReadOptions) (forge.Change, error) {
	q := url.Values{"repo": {repo.String()}, "number": {strconv.Itoa(n)}}
	o.apply(q)
	var r changesResponse
	if err := c.do(ctx, http.MethodGet, "/v1/change", q, nil, &r); err != nil {
		return forge.Change{}, err
	}
	if len(r.Changes) == 0 {
		return forge.Change{}, ErrNotFound
	}
	return r.Changes[0], nil
}

// ListOpen returns the open changes of repo.
func (c *Client) ListOpen(ctx context.Context, repo forge.RepoRef, o ReadOptions) ([]forge.Change, error) {
	return c.list(ctx, "/v1/changes", url.Values{"repo": {repo.String()}, "state": {"open"}}, o)
}

// ByBranch returns the changes of repo whose head is branch, in any state.
func (c *Client) ByBranch(ctx context.Context, repo forge.RepoRef, branch string, o ReadOptions) ([]forge.Change, error) {
	return c.list(ctx, "/v1/changes", url.Values{"repo": {repo.String()}, "head": {branch}}, o)
}

// Search runs a forge search query.
func (c *Client) Search(ctx context.Context, forgeName, q string, limit int, o ReadOptions) ([]forge.Change, error) {
	return c.list(ctx, "/v1/search", url.Values{"forge": {forgeName}, "q": {q}, "limit": {strconv.Itoa(limit)}}, o)
}

func (c *Client) list(ctx context.Context, path string, q url.Values, o ReadOptions) ([]forge.Change, error) {
	o.apply(q)
	var r changesResponse
	if err := c.do(ctx, http.MethodGet, path, q, nil, &r); err != nil {
		return nil, err
	}
	return r.Changes, nil
}

// Invalidate makes the daemon refetch k now.
func (c *Client) Invalidate(ctx context.Context, k forge.Key) error {
	body, err := json.Marshal(map[string]forge.Key{"key": k})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/invalidate", nil, body, nil)
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, nil, &s)
	return s, err
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	respBody, err := c.send(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer func() { _ = respBody.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, respBody)
		return err
	}
	if err := json.NewDecoder(respBody).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// send issues the request and maps transport failures and non-2xx statuses to typed errors.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body []byte) (io.ReadCloser, error) {
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return nil, &APIError{Status: resp.StatusCode, Message: e.Error}
}
