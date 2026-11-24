package hooks

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
)

const cmdHelperEnv = "HOOKS_TEST_HELPER_OUT"

type cmdHelperRecord struct {
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Stdin string            `json:"stdin"`
}

// TestMain turns the test binary into the stub hook command when re-executed
// with cmdHelperEnv set.
func TestMain(m *testing.M) {
	if out := os.Getenv(cmdHelperEnv); out != "" {
		cmdHelperMain(out)
		return
	}
	os.Exit(m.Run())
}

func cmdHelperMain(out string) {
	in, _ := io.ReadAll(os.Stdin)
	rec := cmdHelperRecord{Args: os.Args[1:], Env: map[string]string{}, Stdin: string(in)}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "FORGED_") || strings.HasPrefix(k, "HOOKS_TEST_") {
			rec.Env[k] = v
		}
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		os.Exit(3)
	}
	if os.Getenv("HOOKS_TEST_HELPER_HANG") != "" {
		child := exec.Command("sleep", "30")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		_ = child.Start()
		time.Sleep(30 * time.Second)
	}
}

func cmdFixture() (forge.ChangeEvent, forge.Change) {
	repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	ev := forge.ChangeEvent{Seq: 7, Type: forge.EventStateChanged, Repo: repo, Number: 42, URL: "https://x/42", Old: "open", New: "merged"}
	ch := forge.Change{Repo: repo, Number: 42, URL: "https://x/42", Title: "A title", Author: forge.Actor{Login: "alice"}}
	ch.Head.Ref = "feat/x"
	ch.Head.SHA = "abc123"
	ch.Base.Ref = "main"
	return ev, ch
}

func cmdRun(t *testing.T, r Rule, ev forge.ChangeEvent, ch forge.Change) (cmdHelperRecord, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "rec.json")
	t.Setenv(cmdHelperEnv, out)
	t.Setenv("GORACE", "atexit_sleep_ms=0") // the race runtime otherwise sleeps 1s at helper exit
	err := runCommand(context.Background(), r, ev, ch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var rec cmdHelperRecord
	if b, rerr := os.ReadFile(out); rerr == nil {
		if jerr := json.Unmarshal(b, &rec); jerr != nil {
			t.Fatal(jerr)
		}
	}
	return rec, err
}

func cmdRule(args ...string) Rule {
	return Rule{Name: "t", Command: append([]string{os.Args[0]}, args...), Timeout: 10 * time.Second}
}

func TestCommandExpandSubstitution(t *testing.T) {
	ev, ch := cmdFixture()
	got, err := expandArgv([]string{"cmd", "{repo}", "{number}", "pr-{number}-{branch}", "{url}", "{type}", "{old}>{new}", "{base}", "{sha}", "{title}", "{author}", "{unknown}", "{{.Name}}", ".x | {a: 1}"}, ev, ch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd", "github.com/o/r", "42", "pr-42-feat/x", "https://x/42", "state.changed", "open>merged", "main", "abc123", "A title", "alice", "{unknown}", "{{.Name}}", ".x | {a: 1}"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCommandExpandNumberFallback(t *testing.T) {
	ev, ch := cmdFixture()
	ev.Number = 0
	got, err := expandArgv([]string{"{number}"}, ev, ch)
	if err != nil || got[0] != "42" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestCommandHostileBranchesStayOneInertElement(t *testing.T) {
	ev, ch := cmdFixture()
	tmp := t.TempDir()
	branches := []string{
		"x;touch " + filepath.Join(tmp, "pwn"),
		"$(id)",
		"`id`",
		"a b",
		"line1\nline2",
		"{title}",
		"x && touch " + filepath.Join(tmp, "pwn"),
	}
	for _, b := range branches {
		ch.Head.Ref = b
		for name, tmpl := range map[string][]string{
			"whole":     {"{branch}"},
			"substring": {"pre-{branch}-post"},
		} {
			t.Run(name+"/"+b, func(t *testing.T) {
				rec, err := cmdRun(t, cmdRule(append([]string{"fixed"}, tmpl...)...), ev, ch)
				if err != nil {
					t.Fatal(err)
				}
				want := b
				if name == "substring" {
					want = "pre-" + b + "-post"
				}
				if !slices.Equal(rec.Args, []string{"fixed", want}) {
					t.Fatalf("args %q, want one element %q", rec.Args, want)
				}
				if _, err := os.Stat(filepath.Join(tmp, "pwn")); err == nil {
					t.Fatal("injected command ran")
				}
			})
		}
	}
}

func TestCommandNoReexpansion(t *testing.T) {
	ev, ch := cmdFixture()
	ch.Head.Ref = "{title}"
	got, err := expandArgv([]string{"c", "{branch}:{title}"}, ev, ch)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != "{title}:A title" {
		t.Fatalf("got %q", got[1])
	}
}

func TestCommandOptionInjectionGuard(t *testing.T) {
	ev, ch := cmdFixture()
	ch.Head.Ref = "--force"

	if _, err := expandArgv([]string{"git", "push", "{branch}"}, ev, ch); err == nil {
		t.Fatal("whole-element placeholder expanding to an option must be refused")
	}
	if _, err := expandArgv([]string{"git", "push", "{branch}-suffix"}, ev, ch); err == nil {
		t.Fatal("leading placeholder expanding to an option must be refused")
	}
	got, err := expandArgv([]string{"git", "push", "--", "{branch}"}, ev, ch)
	if err != nil || got[3] != "--force" {
		t.Fatalf("after --: got %q, %v", got, err)
	}
	got, err = expandArgv([]string{"c", "--x={branch}"}, ev, ch)
	if err != nil || got[1] != "--x=--force" {
		t.Fatalf("literal prefix: got %q, %v", got, err)
	}
	if _, err := cmdRun(t, cmdRule("{branch}"), ev, ch); err == nil {
		t.Fatal("runCommand must refuse")
	}
	if _, err := os.Stat(os.Getenv(cmdHelperEnv)); err == nil {
		t.Fatal("command ran despite refusal")
	}
}

func TestCommandEnvAndStdin(t *testing.T) {
	ev, ch := cmdFixture()
	t.Setenv("FORGED_TOKEN", "leak")
	t.Setenv("FORGED_BRANCH", "inherited")
	t.Setenv("FORGED_SOCKET", "/run/forged.sock")
	t.Setenv("HOOKS_TEST_KEEP", "yes")

	rec, err := cmdRun(t, cmdRule(), ev, ch)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FORGED_EVENT_TYPE": "state.changed", "FORGED_REPO": "github.com/o/r", "FORGED_NUMBER": "42",
		"FORGED_URL": "https://x/42", "FORGED_OLD": "open", "FORGED_NEW": "merged",
		"FORGED_BRANCH": "feat/x", "FORGED_BASE": "main", "FORGED_SHA": "abc123",
		"FORGED_TITLE": "A title", "FORGED_AUTHOR": "alice",
		"FORGED_SOCKET": "/run/forged.sock", "HOOKS_TEST_KEEP": "yes",
	}
	for k, v := range want {
		if rec.Env[k] != v {
			t.Errorf("%s = %q, want %q", k, rec.Env[k], v)
		}
	}
	if _, ok := rec.Env["FORGED_TOKEN"]; ok {
		t.Error("inherited FORGED_TOKEN leaked")
	}

	var in Event
	if err := json.Unmarshal([]byte(rec.Stdin), &in); err != nil {
		t.Fatal(err)
	}
	if in.Event.Seq != ev.Seq || in.Event.Type != ev.Type || in.Change.Title != ch.Title || in.Change.Head.Ref != "feat/x" {
		t.Fatalf("stdin payload %+v", in)
	}
}

func TestCommandEnvPure(t *testing.T) {
	ev, ch := cmdFixture()
	env := commandEnv([]string{"A=1", "FORGED_X=2", "FORGED_SOCKET=s", "FORGED_TITLE=old"}, ev, ch)
	if !slices.Contains(env, "A=1") || !slices.Contains(env, "FORGED_SOCKET=s") || slices.Contains(env, "FORGED_X=2") || slices.Contains(env, "FORGED_TITLE=old") {
		t.Fatalf("env %q", env)
	}
}

func TestCommandFailureReturnsError(t *testing.T) {
	ev, ch := cmdFixture()
	err := runCommand(context.Background(), Rule{Name: "t", Command: []string{"false"}}, ev, ch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "false") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandNULInEnvReturnsError(t *testing.T) {
	ev, ch := cmdFixture()
	ch.Title = "a\x00b"
	if _, err := cmdRun(t, cmdRule(), ev, ch); err == nil {
		t.Fatal("want error")
	}
}

func TestCommandTimeoutKillsChildHoldingStdout(t *testing.T) {
	ev, ch := cmdFixture()
	t.Setenv("HOOKS_TEST_HELPER_HANG", "1")
	r := cmdRule()
	r.Timeout = 500 * time.Millisecond
	start := time.Now()
	_, err := cmdRun(t, r, ev, ch)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestCommandRealShellConsumerStaysInert(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	ev, ch := cmdFixture()
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	ch.Head.Ref = "x;touch " + filepath.Join(tmp, "pwn") + " $(touch " + filepath.Join(tmp, "pwn2") + ")"
	r := Rule{Name: "t", Timeout: 10 * time.Second, Command: []string{sh, "-c", `printf %s "$1" > "$2"`, "sh", "{branch}", out}}
	if err := runCommand(context.Background(), r, ev, ch, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != ch.Head.Ref {
		t.Fatalf("out %q, %v", b, err)
	}
	for _, f := range []string{"pwn", "pwn2"} {
		if _, err := os.Stat(filepath.Join(tmp, f)); err == nil {
			t.Fatalf("%s created", f)
		}
	}
}
