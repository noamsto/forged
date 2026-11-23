package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
)

var (
	alpha = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "acme", Name: "alpha"}
	beta  = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "acme", Name: "beta"}
)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (r gqlRequest) hasVar(value any) bool {
	for _, v := range r.Variables {
		if v == value {
			return true
		}
	}
	return false
}

type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	requests []gqlRequest
	handle   func(w http.ResponseWriter, req gqlRequest, n int)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "bearer ") {
		f.t.Errorf("Authorization = %q, want bearer token", got)
	}
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode graphql body: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	n := len(f.requests)
	f.mu.Unlock()
	f.handle(w, req, n)
}

func (f *fakeGitHub) posts() []gqlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func newProvider(t *testing.T, handle func(w http.ResponseWriter, req gqlRequest, n int)) (*Provider, *fakeGitHub) {
	t.Helper()
	return newProviderToken(t, func(context.Context) (string, error) { return "test-token", nil }, handle)
}

func newProviderToken(t *testing.T, token func(context.Context) (string, error), handle func(w http.ResponseWriter, req gqlRequest, n int)) (*Provider, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{t: t, handle: handle}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p := New(Options{
		GraphQLURL: srv.URL + "/graphql",
		RESTURL:    srv.URL,
		HTTPClient: srv.Client(),
		Token:      token,
	})
	return p, fake
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serveFixture(name string, t *testing.T) func(http.ResponseWriter, gqlRequest, int) {
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ gqlRequest, _ int) { _, _ = w.Write(body) }
}

func minimalPR(n int) string {
	return fmt.Sprintf(`{"number":%d,"url":"https://github.com/acme/alpha/pull/%d","title":"t%d",`+
		`"author":{"__typename":"User","login":"ada","id":"MDQ6VXNlcjE=","name":"Ada"},"state":"OPEN","isDraft":false,`+
		`"headRefName":"b%d","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",`+
		`"reviewDecision":null,"updatedAt":"2024-05-01T10:00:00Z","autoMergeRequest":null,"commits":{"nodes":[]}}`, n, n, n, n)
}

var prAlias = regexp.MustCompile(`pr(\d+):pullRequest\(number:(\d+)\)`)

// aliasedResponse answers a single-repo batch by resolving every pr<N> alias.
func aliasedResponse(w http.ResponseWriter, req gqlRequest, _ int) {
	var prs []string
	for _, m := range prAlias.FindAllStringSubmatch(req.Query, -1) {
		n, _ := strconv.Atoi(m[1])
		prs = append(prs, fmt.Sprintf(`"pr%d":%s`, n, minimalPR(n)))
	}
	_, _ = fmt.Fprintf(w, `{"data":{"r0":{%s}}}`, strings.Join(prs, ","))
}

func TestGetManyBatchesRepoGroupsIntoOnePost(t *testing.T) {
	p, fake := newProvider(t, serveFixture("batch_two_repos.json", t))
	refs := []forge.ChangeRef{{Repo: alpha, Number: 5}, {Repo: alpha, Number: 6}, {Repo: beta, Number: 1}}

	got, err := p.GetMany(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}

	posts := fake.posts()
	if len(posts) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(posts))
	}
	q := posts[0].Query
	for _, want := range []string{
		"$o0:String!", "$n0:String!", "$o1:String!", "$n1:String!",
		"r0:repository(owner:$o0,name:$n0)", "r1:repository(owner:$o1,name:$n1)",
		"pr5:pullRequest(number:5)", "pr6:pullRequest(number:6)", "pr1:pullRequest(number:1)",
		"headRefOid", "statusCheckRollup", "autoMergeRequest", "__typename",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
	vars := posts[0].Variables
	for k, want := range map[string]string{"o0": "acme", "n0": "alpha", "o1": "acme", "n1": "beta"} {
		if vars[k] != want {
			t.Errorf("variable %s = %v, want %q", k, vars[k], want)
		}
	}
	if len(got) != len(refs) {
		t.Fatalf("results = %d, want %d", len(got), len(refs))
	}
	for _, ref := range refs {
		if got[ref].Err != nil {
			t.Errorf("%v: unexpected error %v", ref, got[ref].Err)
		}
	}
}

func TestGetManyChunksAtMaxBatch(t *testing.T) {
	p, fake := newProvider(t, aliasedResponse)
	if got := p.Capabilities(); got != (forge.Capabilities{Events: false, ConditionalRequests: true, MaxBatch: 50}) {
		t.Fatalf("Capabilities = %+v", got)
	}
	var refs []forge.ChangeRef
	for n := 1; n <= 60; n++ {
		refs = append(refs, forge.ChangeRef{Repo: alpha, Number: n})
	}

	got, err := p.GetMany(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}

	posts := fake.posts()
	if len(posts) != 2 {
		t.Fatalf("POSTs = %d, want 2", len(posts))
	}
	selections := 0
	for _, post := range posts {
		n := len(prAlias.FindAllString(post.Query, -1))
		if n > 50 {
			t.Errorf("chunk has %d selections, want <= 50", n)
		}
		selections += n
	}
	if selections != 60 {
		t.Errorf("selections = %d, want 60", selections)
	}
	for _, ref := range refs {
		f, ok := got[ref]
		if !ok || f.Err != nil || f.Change.Number != ref.Number {
			t.Errorf("%v: got %+v ok=%v", ref, f, ok)
		}
	}
}

func TestMapChange(t *testing.T) {
	p, _ := newProvider(t, serveFixture("batch_two_repos.json", t))
	refs := []forge.ChangeRef{{Repo: alpha, Number: 5}, {Repo: alpha, Number: 6}, {Repo: beta, Number: 1}}
	got, err := p.GetMany(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	t.Run("open change with mixed checks", func(t *testing.T) {
		f := got[refs[0]]
		if f.Err != nil {
			t.Fatal(f.Err)
		}
		c := f.Change
		if c.Repo != alpha || c.Number != 5 || c.URL != "https://github.com/acme/alpha/pull/5" || c.Title != "Add retry to the uploader" {
			t.Errorf("identity = %+v", c)
		}
		if c.Author != (forge.Actor{Login: "ada", Name: "Ada Lovelace", ID: "MDQ6VXNlcjE=", IsBot: false}) {
			t.Errorf("author = %+v", c.Author)
		}
		if c.State != forge.StateOpen || c.Draft || c.Mergeable != forge.MergeableClean || c.MergeState != "unstable" {
			t.Errorf("state = %q draft=%v mergeable=%q mergeState=%q", c.State, c.Draft, c.Mergeable, c.MergeState)
		}
		if c.Head.Ref != "feat/uploader-retry" || c.Head.SHA != "9f2c1d7e5a3b4c6d8e0f1a2b3c4d5e6f70818293" || c.Base.Ref != "main" {
			t.Errorf("head/base = %+v / %+v", c.Head, c.Base)
		}
		if c.ReviewDecision != forge.ReviewNone {
			t.Errorf("ReviewDecision = %q, want none for null", c.ReviewDecision)
		}
		if !c.UpdatedAt.Equal(ts("2024-05-01T10:30:00Z")) {
			t.Errorf("UpdatedAt = %v", c.UpdatedAt)
		}

		wantReviews := []forge.Review{
			{Author: forge.Actor{Login: "grace", Name: "Grace Hopper", ID: "MDQ6VXNlcjI="}, State: "approved", SubmittedAt: ts("2024-05-01T10:20:00Z")},
			{Author: forge.Actor{Login: "copilot", IsBot: true}, State: "commented", SubmittedAt: ts("2024-05-01T10:25:00Z")},
		}
		if !slices.EqualFunc(c.Reviews, wantReviews, func(a, b forge.Review) bool {
			return a.Author == b.Author && a.State == b.State && a.SubmittedAt.Equal(b.SubmittedAt)
		}) {
			t.Errorf("Reviews = %+v, want %+v", c.Reviews, wantReviews)
		}

		am := c.AutoMerge
		if am == nil {
			t.Fatal("AutoMerge = nil")
		}
		if am.Method != "squash" || !am.EnabledAt.Equal(ts("2024-05-01T10:15:00Z")) ||
			am.EnabledBy != (forge.Actor{Login: "ada", Name: "Ada Lovelace", ID: "MDQ6VXNlcjE="}) {
			t.Errorf("AutoMerge = %+v", am)
		}
		if am.AuthorEmail == nil || *am.AuthorEmail != "ada@example.com" ||
			am.CommitHeadline == nil || *am.CommitHeadline != "Add retry to the uploader (#5)" ||
			am.CommitBody == nil || *am.CommitBody != "Retries with backoff." {
			t.Errorf("AutoMerge strings = %v %v %v", am.AuthorEmail, am.CommitHeadline, am.CommitBody)
		}

		if c.Checks.Rollup != forge.RollupPending || !c.Checks.Truncated {
			t.Errorf("rollup = %q truncated=%v, want pending/true", c.Checks.Rollup, c.Checks.Truncated)
		}
		want := []forge.CheckRun{
			{
				Kind: "check", Name: "build", Workflow: "CI", URL: "https://github.com/acme/alpha/actions/runs/100/job/1",
				Status: "completed", Conclusion: "success",
				StartedAt: ts("2024-05-01T10:01:00Z"), CompletedAt: ts("2024-05-01T10:05:00Z"),
			},
			{
				Kind: "check", Name: "integration", Workflow: "CI", URL: "https://github.com/acme/alpha/actions/runs/100/job/2",
				Status: "in_progress", Conclusion: "",
				StartedAt: ts("2024-05-01T10:06:00Z"),
			},
			{
				Kind: "status", Name: "ci/lint", URL: "https://lint.example.com/runs/7",
				Status: "completed", Conclusion: "success", StartedAt: ts("2024-05-01T10:02:00Z"),
			},
			{
				Kind: "status", Name: "ci/deploy-preview", URL: "https://deploy.example.com/runs/8",
				Status: "pending", Conclusion: "", StartedAt: ts("2024-05-01T10:03:00Z"),
			},
		}
		if len(c.Checks.Runs) != len(want) {
			t.Fatalf("runs = %d, want %d: %+v", len(c.Checks.Runs), len(want), c.Checks.Runs)
		}
		for i, w := range want {
			r := c.Checks.Runs[i]
			if r.Kind != w.Kind || r.Name != w.Name || r.Workflow != w.Workflow || r.URL != w.URL ||
				r.Status != w.Status || r.Conclusion != w.Conclusion ||
				!r.StartedAt.Equal(w.StartedAt) || !r.CompletedAt.Equal(w.CompletedAt) {
				t.Errorf("run %d = %+v, want %+v", i, r, w)
			}
		}
		if !c.Checks.Runs[1].CompletedAt.IsZero() {
			t.Errorf("in-progress CompletedAt = %v, want zero", c.Checks.Runs[1].CompletedAt)
		}
	})

	t.Run("merged bot change without checks", func(t *testing.T) {
		c := got[refs[1]].Change
		if c.Author != (forge.Actor{Login: "dependabot", IsBot: true}) {
			t.Errorf("author = %+v", c.Author)
		}
		if c.State != forge.StateMerged || c.Mergeable != forge.MergeableConflict || c.MergeState != "dirty" ||
			c.ReviewDecision != forge.ReviewApproved {
			t.Errorf("state=%q mergeable=%q mergeState=%q review=%q", c.State, c.Mergeable, c.MergeState, c.ReviewDecision)
		}
		if c.AutoMerge != nil {
			t.Errorf("AutoMerge = %+v, want nil", c.AutoMerge)
		}
		if len(c.Reviews) != 0 {
			t.Errorf("Reviews = %+v, want none", c.Reviews)
		}
		if c.Checks.Rollup != forge.RollupNone || len(c.Checks.Runs) != 0 || c.Checks.Truncated {
			t.Errorf("checks = %+v, want none/empty/not truncated", c.Checks)
		}
	})

	t.Run("draft change with failing rollup", func(t *testing.T) {
		c := got[refs[2]].Change
		if c.Repo != beta || !c.Draft || c.Mergeable != forge.MergeableUnknown || c.MergeState != "draft" ||
			c.ReviewDecision != forge.ReviewChangesRequested {
			t.Errorf("change = %+v", c)
		}
		if c.Checks.Rollup != forge.RollupFail || c.Checks.Truncated {
			t.Errorf("rollup = %q truncated=%v, want fail/false", c.Checks.Rollup, c.Checks.Truncated)
		}
		if len(c.Checks.Runs) != 1 || c.Checks.Runs[0].Conclusion != "failure" || c.Checks.Runs[0].Workflow != "Test" {
			t.Errorf("runs = %+v", c.Checks.Runs)
		}
	})
}

func TestGetManyPerRefOutcomes(t *testing.T) {
	resp := fmt.Sprintf(`{
		"data": {"r0": {"pr5": %s, "pr7": null}, "r1": null},
		"errors": [
			{"type": "NOT_FOUND", "path": ["r0", "pr7"], "message": "Could not resolve to a PullRequest with the number of 7."},
			{"type": "NOT_FOUND", "path": ["r1"], "message": "Could not resolve to a Repository with the name 'acme/beta'."}
		]
	}`, minimalPR(5))
	p, _ := newProvider(t, func(w http.ResponseWriter, _ gqlRequest, _ int) { _, _ = w.Write([]byte(resp)) })
	refs := []forge.ChangeRef{{Repo: alpha, Number: 5}, {Repo: alpha, Number: 7}, {Repo: beta, Number: 1}}

	got, err := p.GetMany(context.Background(), refs)
	if err != nil {
		t.Fatalf("top-level err = %v, want nil", err)
	}
	if f := got[refs[0]]; f.Err != nil || f.Change.Number != 5 {
		t.Errorf("alpha#5 = %+v", f)
	}
	if f := got[refs[1]]; !errors.Is(f.Err, forge.ErrNotFound) {
		t.Errorf("alpha#7 err = %v, want ErrNotFound", f.Err)
	}
	f := got[refs[2]]
	if f.Err == nil || errors.Is(f.Err, forge.ErrNotFound) || !strings.Contains(f.Err.Error(), "Could not resolve to a Repository") {
		t.Errorf("beta#1 err = %v, want the repo error", f.Err)
	}
}

func TestGetManyHTTPFailureFailsChunkRefs(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, _ gqlRequest, _ int) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	})
	refs := []forge.ChangeRef{{Repo: alpha, Number: 5}, {Repo: beta, Number: 1}}

	got, err := p.GetMany(context.Background(), refs)
	if err != nil {
		t.Fatalf("top-level err = %v, want nil", err)
	}
	for _, ref := range refs {
		f, ok := got[ref]
		if !ok || f.Err == nil || errors.Is(f.Err, forge.ErrNotFound) || !strings.Contains(f.Err.Error(), "502") {
			t.Errorf("%v: got %+v ok=%v, want a 502 error", ref, f, ok)
		}
	}
}

func TestGetNotFound(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, _ gqlRequest, _ int) {
		_, _ = w.Write([]byte(`{"data":{"r0":{"pr9":null}},"errors":[{"type":"NOT_FOUND","path":["r0","pr9"],"message":"Could not resolve to a PullRequest with the number of 9."}]}`))
	})
	if _, err := p.Get(context.Background(), alpha, 9); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGet(t *testing.T) {
	p, fake := newProvider(t, aliasedResponse)
	c, err := p.Get(context.Background(), alpha, 8)
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != 8 || c.Repo != alpha {
		t.Errorf("change = %+v", c)
	}
	if n := len(fake.posts()); n != 1 {
		t.Errorf("POSTs = %d, want 1", n)
	}
}

func TestListOpenPaginates(t *testing.T) {
	page1, page2 := fixture(t, "list_page1.json"), fixture(t, "list_page2.json")
	p, fake := newProvider(t, func(w http.ResponseWriter, req gqlRequest, n int) {
		if n == 1 {
			_, _ = w.Write(page1)
			return
		}
		_, _ = w.Write(page2)
	})

	got, err := p.ListOpen(context.Background(), alpha)
	if err != nil {
		t.Fatal(err)
	}

	posts := fake.posts()
	if len(posts) != 2 {
		t.Fatalf("POSTs = %d, want 2", len(posts))
	}
	if !posts[0].hasVar("acme") || !posts[0].hasVar("alpha") {
		t.Errorf("first request variables = %v, want owner and name", posts[0].Variables)
	}
	for _, v := range posts[0].Variables {
		if v == "Y3Vyc29yOnYyOpHOAAAAAw==" {
			t.Errorf("first request carries a cursor: %v", posts[0].Variables)
		}
	}
	if !posts[1].hasVar("Y3Vyc29yOnYyOpHOAAAAAw==") {
		t.Errorf("second request variables = %v, want the page-1 endCursor", posts[1].Variables)
	}
	if !strings.Contains(posts[0].Query, "pullRequests(") || !strings.Contains(posts[0].Query, "OPEN") {
		t.Errorf("query = %s", posts[0].Query)
	}
	var numbers []int
	for _, c := range got {
		numbers = append(numbers, c.Number)
		if c.Repo != alpha {
			t.Errorf("#%d repo = %v, want %v", c.Number, c.Repo, alpha)
		}
	}
	if want := []int{12, 9, 3, 1}; !slices.Equal(numbers, want) {
		t.Errorf("numbers = %v, want %v", numbers, want)
	}
	if got[0].Checks.Rollup != forge.RollupPass || got[0].ReviewDecision != forge.ReviewRequired {
		t.Errorf("first change = %+v", got[0])
	}
}

func TestByBranchSendsHeadRefAndAllStates(t *testing.T) {
	p, fake := newProvider(t, serveFixture("bybranch.json", t))

	got, err := p.ByBranch(context.Background(), alpha, "feat/x")
	if err != nil {
		t.Fatal(err)
	}

	posts := fake.posts()
	if len(posts) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(posts))
	}
	if !strings.Contains(posts[0].Query, "headRefName:") {
		t.Errorf("query lacks headRefName argument:\n%s", posts[0].Query)
	}
	if !posts[0].hasVar("feat/x") {
		t.Errorf("variables = %v, want the branch name", posts[0].Variables)
	}
	for _, s := range []string{"OPEN", "CLOSED", "MERGED"} {
		if !strings.Contains(posts[0].Query, s) {
			t.Errorf("query lacks state %s", s)
		}
	}
	if len(got) != 2 || got[0].State != forge.StateMerged || got[1].State != forge.StateClosed {
		t.Fatalf("got = %+v", got)
	}
	if got[0].Head.Ref != "feat/x" || got[0].Repo != alpha {
		t.Errorf("first = %+v", got[0])
	}
}

func TestSearchSendsQueryAndMapsPullRequests(t *testing.T) {
	p, fake := newProvider(t, serveFixture("search.json", t))

	got, err := p.Search(context.Background(), "is:pr author:@me is:open", 25)
	if err != nil {
		t.Fatal(err)
	}

	posts := fake.posts()
	if len(posts) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(posts))
	}
	if !strings.Contains(posts[0].Query, "search(") {
		t.Errorf("query lacks search field:\n%s", posts[0].Query)
	}
	if !posts[0].hasVar("is:pr author:@me is:open") || !posts[0].hasVar(float64(25)) {
		t.Errorf("variables = %v, want the search string and first=25", posts[0].Variables)
	}
	// The empty node stands for a non-PullRequest search hit; the repo comes from the PR url.
	if len(got) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(got), got)
	}
	if got[0].Number != 7 || got[0].Repo != alpha || got[1].Number != 3 || got[1].Repo != beta {
		t.Errorf("got = %d@%v, %d@%v", got[0].Number, got[0].Repo, got[1].Number, got[1].Repo)
	}
}

func TestRateLimitForbiddenWithReset(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	p, _ := newProvider(t, func(w http.ResponseWriter, _ gqlRequest, _ int) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})

	_, err := p.Get(context.Background(), alpha, 5)

	var rl *forge.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *forge.RateLimitError", err)
	}
	if !rl.RetryAfter.Equal(reset) {
		t.Errorf("RetryAfter = %v, want %v", rl.RetryAfter, reset)
	}
}

func TestRateLimitRetryAfterHeader(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, _ gqlRequest, _ int) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	before := time.Now()
	_, err := p.Get(context.Background(), alpha, 5)
	after := time.Now()

	var rl *forge.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *forge.RateLimitError", err)
	}
	if rl.RetryAfter.Before(before.Add(30*time.Second)) || rl.RetryAfter.After(after.Add(30*time.Second)) {
		t.Errorf("RetryAfter = %v, want now+30s (now in [%v, %v])", rl.RetryAfter, before, after)
	}
}

func TestUnauthorizedRereadsTokenOnce(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	token := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tokens = append(tokens, fmt.Sprintf("tok%d", len(tokens)+1))
		return tokens[len(tokens)-1], nil
	}
	p, _ := newProviderToken(t, token, func(w http.ResponseWriter, req gqlRequest, n int) {
		if n == 1 {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		aliasedResponse(w, req, n)
	})

	c, err := p.Get(context.Background(), alpha, 5)
	if err != nil {
		t.Fatal(err)
	}
	if c.Number != 5 {
		t.Errorf("change = %+v", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 {
		t.Errorf("token func called %d times, want 2", len(tokens))
	}
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "x") // registers restoration of the original value
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func stubGh(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] || exit 1\necho stub-gh-token\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestTokenPrefersGHTokenOverGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	t.Setenv("PATH", t.TempDir())

	got, err := TokenFromEnvOrGh(context.Background())
	if err != nil || got != "from-gh-token" {
		t.Fatalf("token = %q, err = %v", got, err)
	}
}

func TestTokenFallsBackToGitHubToken(t *testing.T) {
	unsetenv(t, "GH_TOKEN")
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	t.Setenv("PATH", t.TempDir())

	got, err := TokenFromEnvOrGh(context.Background())
	if err != nil || got != "from-github-token" {
		t.Fatalf("token = %q, err = %v", got, err)
	}
}

func TestTokenFallsBackToGhAuthToken(t *testing.T) {
	unsetenv(t, "GH_TOKEN")
	unsetenv(t, "GITHUB_TOKEN")
	stubGh(t)

	got, err := TokenFromEnvOrGh(context.Background())
	if err != nil || got != "stub-gh-token" {
		t.Fatalf("token = %q, err = %v", got, err)
	}
}
