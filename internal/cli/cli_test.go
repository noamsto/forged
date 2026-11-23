package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	for _, sub := range []string{"serve", "pr", "invalidate", "status", "version", "help"} {
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
