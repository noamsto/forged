package forge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound    = errors.New("forge: not found")
	ErrUnsupported = errors.New("forge: unsupported forge or host")
)

// RateLimitError is returned (wrapped) when the forge rate-limits a request.
type RateLimitError struct{ RetryAfter time.Time }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("forge: rate limited until %s", e.RetryAfter.Format(time.RFC3339))
}

type Capabilities struct {
	Events              bool
	ConditionalRequests bool
	MaxBatch            int
}

type ChangeRef struct {
	Repo   RepoRef
	Number int
}

// Fetched is one GetMany result. Err is nil, ErrNotFound (definitive absence),
// or the repo/transport error for that ref.
type Fetched struct {
	Change Change
	Err    error
}

type ProbeResult struct{ Changed bool }

type Provider interface {
	Capabilities() Capabilities
	ListOpen(ctx context.Context, r RepoRef) ([]Change, error)
	// Get returns ErrNotFound when the change is absent.
	Get(ctx context.Context, r RepoRef, number int) (Change, error)
	// GetMany has every requested ref present unless err != nil (whole call failed).
	GetMany(ctx context.Context, refs []ChangeRef) (map[ChangeRef]Fetched, error)
	ByBranch(ctx context.Context, r RepoRef, branch string) ([]Change, error)
	// Search limit is at most 100.
	Search(ctx context.Context, query string, limit int) ([]Change, error)
	// Probe reports Changed=true when there is no baseline.
	Probe(ctx context.Context, r RepoRef) (ProbeResult, error)
}

// EventSource is an optional push seam; a tick means "mark hot and refetch".
// A future event-listen source marks the change hot and refetches; the
// refetch's diff feeds the same typed ChangeEvent stream.
type EventSource interface {
	Subscribe(ctx context.Context, ref ChangeRef) (<-chan struct{}, error)
}
