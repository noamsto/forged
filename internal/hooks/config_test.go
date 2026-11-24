package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
)

func TestConfigValid(t *testing.T) {
	cfg, err := Parse([]byte(`
viewer = "noams"

[[rule]]
name = "notify"
repos = ["acme/app", "git.example.com/acme/lib"]
on = "checks.completed"
conclusion = "fail"
author = "*"
branch = "feat/*"
command = ["notify-send", "{repo}#{number}", "{title}"]
timeout = "30s"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Viewer != "noams" || len(cfg.Rules) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
	r := cfg.Rules[0]
	if r.Name != "notify" || r.On != forge.EventChecksCompleted || r.Conclusion != "fail" ||
		r.Author != "*" || r.Branch != "feat/*" || r.Timeout != 30*time.Second {
		t.Fatalf("rule = %+v", r)
	}
	if len(r.Repos) != 2 || r.Repos[0].Owner != "acme" || r.Repos[1].Host != "git.example.com" {
		t.Fatalf("repos = %+v", r.Repos)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
[[rule]]
repos = ["acme/app"]
on = "head.changed"
command = ["true"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
	r := cfg.Rules[0]
	if r.Author != "" || r.Timeout != 60*time.Second {
		t.Fatalf("rule = %+v", r)
	}
}

func TestConfigBuiltin(t *testing.T) {
	cfg, err := Parse([]byte(`
[[rule]]
repos = ["acme/app"]
builtin = "remove-merged-worktree"
checkout = "/does/not/exist"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
	r := cfg.Rules[0]
	if r.Builtin != BuiltinRemoveMergedWorktree || r.On != forge.EventStateChanged || r.To != "merged" ||
		r.Checkout != "/does/not/exist" {
		t.Fatalf("rule = %+v", r)
	}
}

func TestConfigTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Parse([]byte(`
[[rule]]
repos = ["acme/app"]
builtin = "remove-merged-worktree"
checkout = "~/git/app"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 1 {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
	if got, want := cfg.Rules[0].Checkout, filepath.Join(home, "git/app"); got != want {
		t.Fatalf("checkout = %q, want %q", got, want)
	}
}

func TestConfigInvalid(t *testing.T) {
	const cmd = `repos = ["acme/app"]
on = "state.changed"
command = ["true"]
`
	tests := []struct {
		name string
		toml string
		want string
	}{
		{"missing repos", "[[rule]]\non = \"state.changed\"\ncommand = [\"true\"]\n", "repos"},
		{"bad repo", "[[rule]]\nrepos = [\"nope\"]\non = \"state.changed\"\ncommand = [\"true\"]\n", "repo"},
		{"builtin two repos", "[[rule]]\nrepos = [\"a/b\", \"c/d\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\n", "exactly one repo"},
		{"builtin no checkout", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\n", "checkout"},
		{"unknown builtin", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"nope\"\ncheckout = \"/x\"\n", "builtin"},
		{"builtin with on", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\non = \"state.changed\"\n", "on"},
		{"builtin with from", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\nfrom = \"open\"\n", "from"},
		{"builtin with to", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\nto = \"merged\"\n", "to"},
		{"builtin with command", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\ncommand = [\"true\"]\n", "command"},
		{"builtin with timeout", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\ntimeout = \"5s\"\n", "timeout is not allowed"},
		{"both command and builtin", "[[rule]]\nrepos = [\"a/b\"]\nbuiltin = \"remove-merged-worktree\"\ncheckout = \"/x\"\ncommand = [\"true\"]\n", "command"},
		{"neither command nor builtin", "[[rule]]\nrepos = [\"a/b\"]\non = \"state.changed\"\n", "command"},
		{"empty command", "[[rule]]\nrepos = [\"a/b\"]\non = \"state.changed\"\ncommand = []\n", "command"},
		{"command with checkout", "[[rule]]\n" + cmd + "checkout = \"/x\"\n", "checkout"},
		{"unknown key", "[[rule]]\n" + cmd + "bogus = 1\n", "bogus"},
		{"unknown top-level key", "bogus = 1\n[[rule]]\n" + cmd, "bogus"},
		{"missing on", "[[rule]]\nrepos = [\"a/b\"]\ncommand = [\"true\"]\n", "on"},
		{"unknown event", "[[rule]]\nrepos = [\"a/b\"]\non = \"nope\"\ncommand = [\"true\"]\n", "event"},
		{"resync event", "[[rule]]\nrepos = [\"a/b\"]\non = \"resync\"\ncommand = [\"true\"]\n", "event"},
		{"conclusion wrong event", "[[rule]]\n" + cmd + "conclusion = \"pass\"\n", "conclusion"},
		{"conclusion bad value", "[[rule]]\nrepos = [\"a/b\"]\non = \"checks.completed\"\ncommand = [\"true\"]\nconclusion = \"maybe\"\n", "conclusion"},
		{"placeholder in argv0", "[[rule]]\nrepos = [\"a/b\"]\non = \"state.changed\"\ncommand = [\"{branch}\"]\n", "command[0]"},
		{"unknown placeholder", "[[rule]]\nrepos = [\"a/b\"]\non = \"state.changed\"\ncommand = [\"echo\", \"{nosuchfield}\"]\n", "nosuchfield"},
		{"bad glob", "[[rule]]\n" + cmd + "branch = \"feat/[a-z]*\"\n", "branch"},
		{"bad timeout", "[[rule]]\n" + cmd + "timeout = \"soon\"\n", "timeout"},
		{"non-positive timeout", "[[rule]]\n" + cmd + "timeout = \"0s\"\n", "timeout"},
		{"bad toml", "[[rule", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestConfigFromToValues(t *testing.T) {
	rule := func(on, field, val string) string {
		return "[[rule]]\nrepos = [\"a/b\"]\non = \"" + on + "\"\ncommand = [\"true\"]\n" + field + " = \"" + val + "\"\n"
	}
	tests := []struct {
		on  string
		ok  []string
		bad []string
	}{
		{"state.changed", []string{"open", "closed", "merged"}, []string{"Merged", "merge"}},
		{"draft.changed", []string{"ready", "draft"}, []string{"true", "Draft"}},
		{"checks.started", []string{"pass", "fail", "pending", "none"}, []string{"success", "failure"}},
		{"checks.completed", []string{"pass", "fail", "pending", "none"}, []string{"success", "failure"}},
		{"review_decision.changed", []string{"approved", "changes_requested", "review_required", "none"}, []string{"APPROVED", "approve"}},
		{"mergeable.changed", []string{"clean", "conflict", "unknown"}, []string{"conflicting", "mergeable"}},
		{"head.changed", []string{"abc123", "anything"}, nil},
		{"review.submitted", []string{"approved", "commented", "whatever"}, nil},
	}
	for _, tt := range tests {
		for _, field := range []string{"from", "to"} {
			for _, v := range tt.ok {
				t.Run(tt.on+"/"+field+"/ok/"+v, func(t *testing.T) {
					if _, err := Parse([]byte(rule(tt.on, field, v))); err != nil {
						t.Fatal(err)
					}
				})
			}
			for _, v := range tt.bad {
				t.Run(tt.on+"/"+field+"/bad/"+v, func(t *testing.T) {
					_, err := Parse([]byte(rule(tt.on, field, v)))
					if err == nil {
						t.Fatal("want error")
					}
					for _, want := range []string{field, v, tt.on, tt.ok[0]} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("err = %v, want mention of %q", err, want)
						}
					}
				})
			}
		}
	}
}

func TestConfigBracesPassThrough(t *testing.T) {
	_, err := Parse([]byte(`
[[rule]]
repos = ["acme/app"]
on = "state.changed"
command = ["gh", "pr", "view", "{number}", "--jq", "{a: .b}", "{{.Name}}", "{Upper}"]
`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigAllPlaceholdersAccepted(t *testing.T) {
	var elems []string
	for _, p := range PlaceholderNames {
		elems = append(elems, `"{`+p+`}"`)
	}
	_, err := Parse([]byte("[[rule]]\nrepos = [\"a/b\"]\non = \"state.changed\"\ncommand = [\"echo\", " + strings.Join(elems, ", ") + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndDefaultPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte("viewer = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Viewer != "x" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	if _, err := Load(filepath.Join(dir, "missing.toml")); err == nil {
		t.Fatal("want error for missing file")
	}

	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, err := DefaultPath(); err != nil || p != "/xdg/forged/hooks.toml" {
		t.Fatalf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if p, err := DefaultPath(); err != nil || p != "/home/u/.config/forged/hooks.toml" {
		t.Fatalf("DefaultPath = %q, %v", p, err)
	}
}
