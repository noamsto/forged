package forge

import "fmt"

type Kind string

const (
	KindRepoOpen Kind = "repo-open"
	KindChange   Kind = "change"
	KindByBranch Kind = "change-by-branch"
	KindQuery    Kind = "query"
)

// Key identifies a cached resource. It is comparable and usable as a map key.
// For KindQuery, Repo.Forge carries the forge name.
type Key struct {
	Kind   Kind    `json:"kind"`
	Repo   RepoRef `json:"repo"`
	Number int     `json:"number"`
	Branch string  `json:"branch"`
	Query  string  `json:"query"`
	Limit  int     `json:"limit"`
}

func RepoOpen(r RepoRef) Key { return Key{Kind: KindRepoOpen, Repo: r} }

func ChangeKey(r RepoRef, n int) Key { return Key{Kind: KindChange, Repo: r, Number: n} }

func ByBranch(r RepoRef, branch string) Key {
	return Key{Kind: KindByBranch, Repo: r, Branch: branch}
}

func Query(forge, q string, limit int) Key {
	return Key{Kind: KindQuery, Repo: RepoRef{Forge: forge}, Query: q, Limit: limit}
}

func (k Key) IsList() bool { return k.Kind != KindChange }

func (k Key) String() string {
	switch k.Kind {
	case KindChange:
		return fmt.Sprintf("%s:%s#%d", k.Kind, k.Repo, k.Number)
	case KindByBranch:
		return fmt.Sprintf("%s:%s@%s", k.Kind, k.Repo, k.Branch)
	case KindQuery:
		return fmt.Sprintf("%s:%s:%s#%d", k.Kind, k.Repo.Forge, k.Query, k.Limit)
	default:
		return fmt.Sprintf("%s:%s", k.Kind, k.Repo)
	}
}
