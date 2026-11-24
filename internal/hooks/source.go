package hooks

import (
	"context"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

// Source is the slice of the daemon client that hooks depend on.
type Source interface {
	Subscribe(ctx context.Context, o client.SubscribeOptions) (<-chan forge.ChangeEvent, error)
	Change(ctx context.Context, repo forge.RepoRef, n int, o client.ReadOptions) (forge.Change, error)
	ByBranch(ctx context.Context, repo forge.RepoRef, branch string, o client.ReadOptions) ([]forge.Change, error)
}

var _ Source = (*client.Client)(nil)
