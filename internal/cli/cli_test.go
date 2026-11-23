package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/client"
	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/forgetest"
)

func TestMainBasics(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"empty args", []string{}, 2},
		{"bogus", []string{"bogus"}, 2},
		{"version", []string{"version"}, 0},
		{"help", []string{"help"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Main(tc.args); got != tc.want {
				t.Fatalf("Main(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestUsageListsSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := run(nil, &stdout, &stderr); got != 2 {
		t.Fatalf("exit = %d, want 2", got)
	}
	for _, sub := range []string{"serve", "pr", "invalidate", "watch", "status", "version", "help"} {
		if !strings.Contains(stderr.String(), sub) {
			t.Errorf("usage missing %q: %s", sub, stderr.String())
		}
	}
}

type daemon struct {
	sock, state string
	fake        *forgetest.Fake
	cancel      context.CancelFunc
	done        chan int
	stderr      *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startDaemon runs `serve` against a fake provider holding github.com/o/r#5.
func startDaemon(t *testing.T) *daemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	sock := filepath.Join(dir, "s.sock")
	t.Setenv("FORGE_CACHE_SOCKET", sock)

	repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	fake := forgetest.New()
	fake.SetChanges(repo, []forge.Change{{Repo: repo, Number: 5, State: forge.StateOpen, Title: "t"}})

	ctx, cancel := context.WithCancel(context.Background())
	oldProviders, oldParent := newProviders, serveParent
	newProviders = func() map[string]forge.Provider { return map[string]forge.Provider{"github/github.com": fake} }
	serveParent = ctx
	t.Cleanup(func() { newProviders, serveParent = oldProviders, oldParent })

	d := &daemon{sock: sock, state: state, fake: fake, cancel: cancel, done: make(chan int, 1), stderr: &syncBuffer{}}
	go func() { d.done <- run([]string{"serve", "--socket", sock}, &bytes.Buffer{}, d.stderr) }()
	t.Cleanup(cancel)

	cl := client.New(sock)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := cl.Status(context.Background()); err == nil {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up; stderr: %s", d.stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (d *daemon) stop(t *testing.T) int {
	t.Helper()
	d.cancel()
	select {
	case code := <-d.done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after cancel")
		return -1
	}
}

func TestServeLifecycle(t *testing.T) {
	d := startDaemon(t)
	if !strings.Contains(d.stderr.String(), d.sock) {
		t.Errorf("stderr does not name the socket %s: %s", d.sock, d.stderr)
	}
	if code := d.stop(t); code != 0 {
		t.Fatalf("serve exit = %d; stderr: %s", code, d.stderr)
	}
	if _, err := os.Stat(filepath.Join(d.state, "forge-cache", "snapshot.json")); err != nil {
		t.Errorf("snapshot not written: %v", err)
	}
	if _, err := os.Stat(d.sock); !os.IsNotExist(err) {
		t.Errorf("socket file still present: %v", err)
	}
}

func TestInvalidate(t *testing.T) {
	d := startDaemon(t)
	for _, tc := range []struct {
		name   string
		arg    string
		method string
		want   int
	}{
		{"change", "github.com/o/r#5", "GetMany", 0},
		{"change without host", "o/r#5", "GetMany", 0},
		{"repo", "github.com/o/r", "ListOpen", 0},
		{"bare owner", "o", "", 2},
		{"bad number", "github.com/o/r#x", "", 2},
		{"missing arg", "", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"invalidate"}
			if tc.arg != "" {
				args = append(args, tc.arg)
			}
			var stdout, stderr bytes.Buffer
			before := d.fake.Calls(tc.method)
			if got := run(args, &stdout, &stderr); got != tc.want {
				t.Fatalf("exit = %d, want %d; stderr: %s", got, tc.want, &stderr)
			}
			if tc.want == 2 {
				if !strings.Contains(stderr.String(), "usage") {
					t.Errorf("no usage on stderr: %s", &stderr)
				}
				return
			}
			if got := d.fake.Calls(tc.method); got != before+1 {
				t.Errorf("%s calls = %d, want %d", tc.method, got, before+1)
			}
		})
	}
	if code := d.stop(t); code != 0 {
		t.Fatalf("serve exit = %d", code)
	}
}

func TestStatus(t *testing.T) {
	d := startDaemon(t)
	var stdout, stderr bytes.Buffer
	if got := run([]string{"status"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit = %d; stderr: %s", got, &stderr)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, &stdout)
	}
	if _, ok := out["version"]; !ok {
		t.Errorf("no version key: %s", &stdout)
	}
	d.stop(t)
}

func TestClientCommandsWithoutDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("FORGE_CACHE_SOCKET", filepath.Join(dir, "none.sock"))
	for _, args := range [][]string{{"status"}, {"invalidate", "o/r#1"}} {
		var stdout, stderr bytes.Buffer
		if got := run(args, &stdout, &stderr); got != 1 {
			t.Errorf("run(%v) = %d, want 1", args, got)
		}
	}
}

// startWatch runs `watch args…` against the current daemon and returns a stop
// func that cancels it and yields the exit code.
func startWatch(t *testing.T, args ...string) (out, errBuf *syncBuffer, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	old := watchParent
	watchParent = ctx
	t.Cleanup(func() { watchParent = old })
	out, errBuf = &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- run(append([]string{"watch"}, args...), out, errBuf) }()
	t.Cleanup(cancel)
	return out, errBuf, func() int {
		t.Helper()
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("watch did not return after cancel")
			return -1
		}
	}
}

// driveUntil flips the checks rollup of every change in the fake and
// invalidates until done reports true.
func driveUntil(t *testing.T, d *daemon, done func() bool, numbers ...int) {
	t.Helper()
	repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	cl := client.New(d.sock)
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; !done(); i++ {
		if time.Now().After(deadline) {
			t.Fatalf("watch output never satisfied the test; stderr: %s", d.stderr)
		}
		rollup := forge.RollupPass
		if i%2 == 1 {
			rollup = forge.RollupFail
		}
		var cs []forge.Change
		for _, n := range numbers {
			c := forge.Change{Repo: repo, Number: n, State: forge.StateOpen, Title: "t"}
			c.Checks.Rollup = rollup
			cs = append(cs, c)
		}
		d.fake.SetChanges(repo, cs)
		if err := cl.Invalidate(context.Background(), forge.RepoOpen(repo)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func decodeEvents(t *testing.T, out *syncBuffer) []forge.ChangeEvent {
	t.Helper()
	var evs []forge.ChangeEvent
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var ev forge.ChangeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("not a JSON line %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func baseline(t *testing.T, d *daemon, repo forge.RepoRef, numbers ...int) {
	t.Helper()
	var cs []forge.Change
	for _, n := range numbers {
		cs = append(cs, forge.Change{Repo: repo, Number: n, State: forge.StateOpen, Title: "t"})
	}
	d.fake.SetChanges(repo, cs)
	if err := client.New(d.sock).Invalidate(context.Background(), forge.RepoOpen(repo)); err != nil {
		t.Fatal(err)
	}
}

func TestWatchFiltersByPRAndType(t *testing.T) {
	d := startDaemon(t)
	repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	baseline(t, d, repo, 5, 6)
	out, errBuf, stop := startWatch(t, "-R", "o/r", "--pr", "5", "--event", "checks.completed")
	driveUntil(t, d, func() bool { return out.String() != "" }, 5, 6)
	if code := stop(); code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, errBuf)
	}
	for _, ev := range decodeEvents(t, out) {
		if ev.Number != 5 || ev.Type != forge.EventChecksCompleted {
			t.Errorf("unexpected event %+v", ev)
		}
	}
	d.stop(t)
}

func TestWatchRepoWideIncludesOtherNumbers(t *testing.T) {
	d := startDaemon(t)
	repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	baseline(t, d, repo, 5, 6)
	out, errBuf, stop := startWatch(t, "--repo", "o/r")
	driveUntil(t, d, func() bool {
		seen := map[int]bool{}
		if out.String() != "" {
			for _, ev := range decodeEvents(t, out) {
				seen[ev.Number] = true
			}
		}
		return seen[5] && seen[6]
	}, 5, 6)
	if code := stop(); code != 0 {
		t.Fatalf("exit = %d; stderr: %s", code, errBuf)
	}
	d.stop(t)
}

func TestWatchUsageErrors(t *testing.T) {
	d := startDaemon(t)
	t.Setenv("GH_REPO", "")
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown event", []string{"watch", "-R", "o/r", "--event", "bogus"}},
		{"resync is not filterable", []string{"watch", "-R", "o/r", "--event", "resync"}},
		{"no repo", []string{"watch"}},
		{"bad repo", []string{"watch", "-R", "justowner"}},
		{"extra arg", []string{"watch", "-R", "o/r", "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tc.args, &stdout, &stderr); got != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", got, &stderr)
			}
		})
	}
	d.stop(t)
}

func TestWatchWithoutDaemonOrHost(t *testing.T) {
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("FORGE_CACHE_SOCKET", filepath.Join(dir, "none.sock"))
	var stdout, stderr bytes.Buffer
	if got := run([]string{"watch", "-R", "o/r"}, &stdout, &stderr); got != 1 {
		t.Fatalf("daemon down: exit = %d, want 1", got)
	}

	d := startDaemon(t)
	stdout.Reset()
	stderr.Reset()
	if got := run([]string{"watch", "-R", "gitlab.com/o/r"}, &stdout, &stderr); got != 1 {
		t.Fatalf("unsupported host: exit = %d, want 1; stderr: %s", got, &stderr)
	}
	d.stop(t)
}

func TestWatchStreamEnded(t *testing.T) {
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	t.Setenv("FORGE_CACHE_SOCKET", sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var gotSince string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		_ = json.NewEncoder(w).Encode(forge.ChangeEvent{Seq: 42, Type: forge.EventStateChanged})
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var stdout, stderr bytes.Buffer
	if got := run([]string{"watch", "-R", "o/r", "--since", "0"}, &stdout, &stderr); got != 1 {
		t.Fatalf("exit = %d, want 1; stderr: %s", got, &stderr)
	}
	if !strings.Contains(stderr.String(), "stream ended after seq 42; reconnect with --since 42") {
		t.Errorf("stderr = %q", &stderr)
	}
	if gotSince != "0" {
		t.Errorf("since = %q, want 0", gotSince)
	}
	if evs := decodeEvents(t, &syncBuffer{b: stdout}); len(evs) != 1 || evs[0].Seq != 42 {
		t.Errorf("stdout = %q", &stdout)
	}
}
