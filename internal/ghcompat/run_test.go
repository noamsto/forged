package ghcompat

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/cache"
	"github.com/noamsto/forge-cache/internal/clock"
	"github.com/noamsto/forge-cache/internal/forgetest"
	"github.com/noamsto/forge-cache/internal/server"
)

// TestMain turns the test binary into a helper that runs Run, so a fallback
// syscall.Exec replaces the helper process rather than the test.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER") == "1" {
		i := slices.Index(os.Args, "--")
		os.Exit(Run(os.Args[i+1:], os.Stdout, os.Stderr, os.Getenv))
	}
	os.Exit(m.Run())
}

const stubExit = 7

type runEnv struct {
	stubOut string
	sock    string
	extra   []string
	path    string
	dir     string
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	isolateGit(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_OUT\"\nexit " + strconv.Itoa(stubExit) + "\n"
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sd, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sd) })
	return &runEnv{
		stubOut: filepath.Join(t.TempDir(), "stub.out"),
		sock:    filepath.Join(sd, "s.sock"),
		path:    bin + string(os.PathListSeparator) + filepath.Dir(gitPath),
		dir:     t.TempDir(),
	}
}

// run executes Run in a helper process and returns exit code, stdout, stderr.
func (e *runEnv) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"--"}, args...)...)
	cmd.Dir = e.dir
	cmd.Env = append([]string{
		"PATH=" + e.path,
		"GO_WANT_HELPER=1",
		"STUB_OUT=" + e.stubOut,
		"FORGE_CACHE_SOCKET=" + e.sock,
		"GH_REPO=noamsto/prdash",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	}, e.extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func (e *runEnv) stubArgv(t *testing.T) ([]string, bool) {
	t.Helper()
	b, err := os.ReadFile(e.stubOut)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), true
}

func (e *runEnv) serve(t *testing.T) *forgetest.Fake {
	t.Helper()
	fake := forgetest.New()
	o := cache.DefaultOptions()
	o.Clock = clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	o.BatchWindow = 0
	c := cache.New(map[string]forge.Provider{"github/github.com": fake}, o)
	ln, err := net.Listen("unix", e.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server.New(c, "v-test")}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fake
}

func (e *runEnv) wantFallback(t *testing.T, code int, stdout string, args []string) {
	t.Helper()
	if code != stubExit {
		t.Errorf("exit = %d, want %d (stub's code)", code, stubExit)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	got, ok := e.stubArgv(t)
	if !ok {
		t.Fatal("stub gh was not called")
	}
	if !slices.Equal(got, args) {
		t.Errorf("stub argv = %q, want %q", got, args)
	}
}

var allFieldsArg = strings.Join(allFields, ",")

func TestRunDaemonDownFallsBack(t *testing.T) {
	e := newRunEnv(t)
	args := []string{"pr", "view", "213", "-R", "noamsto/prdash", "--json", allFieldsArg}
	code, stdout, _ := e.run(t, args...)
	e.wantFallback(t, code, stdout, args)
}

func TestRunUnsupportedFieldFallsBack(t *testing.T) {
	e := newRunEnv(t)
	e.serve(t)
	args := []string{"pr", "view", "213", "-R", "noamsto/prdash", "--json", "comments"}
	code, stdout, _ := e.run(t, args...)
	e.wantFallback(t, code, stdout, args)
}

func TestRunJQCompileErrorFallsBack(t *testing.T) {
	e := newRunEnv(t)
	fake := e.serve(t)
	fake.SetChanges(refUser().Repo, []forge.Change{refUser()})
	args := []string{"pr", "view", "213", "-R", "noamsto/prdash", "--json", "number", "--jq", ".["}
	code, stdout, _ := e.run(t, args...)
	e.wantFallback(t, code, stdout, args)
}

func TestRunDaemonNotFoundFallsBack(t *testing.T) {
	e := newRunEnv(t)
	e.serve(t) // fake knows no changes: 404
	args := []string{"pr", "view", "999", "-R", "noamsto/prdash", "--json", "number"}
	code, stdout, _ := e.run(t, args...)
	e.wantFallback(t, code, stdout, args)
}

func TestRunServesViewFromDaemon(t *testing.T) {
	e := newRunEnv(t)
	c := refUser()
	e.serve(t).SetChanges(c.Repo, []forge.Change{c})

	code, stdout, stderr := e.run(t, "pr", "view", "213", "-R", "noamsto/prdash", "--json", allFieldsArg)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "view_all_fields.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want) {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	if _, called := e.stubArgv(t); called {
		t.Error("stub gh was called")
	}
}

func TestRunServesViewWithJQ(t *testing.T) {
	e := newRunEnv(t)
	c := refUser()
	e.serve(t).SetChanges(c.Repo, []forge.Change{c})

	code, stdout, stderr := e.run(t, "pr", "view", "213", "--json", "title", "-q", ".title", "--max-age", "0")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	if want := c.Title + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if _, called := e.stubArgv(t); called {
		t.Error("stub gh was called")
	}
}

func TestRunNoFallbackDaemonDownFails(t *testing.T) {
	e := newRunEnv(t)
	e.extra = []string{"FORGE_CACHE_NO_FALLBACK=1"}
	code, stdout, stderr := e.run(t, "pr", "view", "213", "-R", "noamsto/prdash", "--json", allFieldsArg)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("no error on stderr")
	}
	if _, called := e.stubArgv(t); called {
		t.Error("stub gh was called")
	}
}
