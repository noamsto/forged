// Package hooks runs local commands in response to forged change events.
package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/noamsto/forged/forge"
)

const BuiltinRemoveMergedWorktree = "remove-merged-worktree"

const defaultTimeout = 60 * time.Second

// PlaceholderNames are the {name} tokens a command element may use.
var PlaceholderNames = []string{"repo", "number", "url", "type", "old", "new", "branch", "base", "sha", "title", "author"}

var placeholderToken = regexp.MustCompile(`\{[a-z_]+\}`)

var eventTypes = []forge.EventType{
	forge.EventChecksStarted, forge.EventChecksCompleted, forge.EventReviewSubmitted,
	forge.EventReviewDecisionChanged, forge.EventStateChanged, forge.EventDraftChanged,
	forge.EventHeadChanged, forge.EventMergeableChanged,
}

type Rule struct {
	Name  string
	Repos []forge.RepoRef
	On    forge.EventType
	From  string
	To    string
	// Conclusion filters checks.completed on its new value: pass or fail.
	Conclusion string
	// Author is a login; "" means the viewer and "*" anyone.
	Author  string
	Branch  string
	Command []string
	Builtin string
	// Checkout is the absolute path of a clone of the repo, with ~ expanded.
	Checkout string
	Timeout  time.Duration
}

type Config struct {
	Viewer string
	Rules  []Rule
}

type wireConfig struct {
	Viewer string     `toml:"viewer"`
	Rules  []wireRule `toml:"rule"`
}

type wireRule struct {
	Name       string   `toml:"name"`
	Repos      []string `toml:"repos"`
	On         string   `toml:"on"`
	From       string   `toml:"from"`
	To         string   `toml:"to"`
	Conclusion string   `toml:"conclusion"`
	Author     string   `toml:"author"`
	Branch     string   `toml:"branch"`
	Command    []string `toml:"command"`
	Builtin    string   `toml:"builtin"`
	Checkout   string   `toml:"checkout"`
	Timeout    string   `toml:"timeout"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator's --config flag or default config location
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "forged", "hooks.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "forged", "hooks.toml"), nil
}

func Parse(data []byte) (Config, error) {
	var w wireConfig
	md, err := toml.Decode(string(data), &w)
	if err != nil {
		return Config{}, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("unknown key(s): %s", strings.Join(keys, ", "))
	}
	cfg := Config{Viewer: w.Viewer}
	for i, wr := range w.Rules {
		r, err := wr.rule()
		if err != nil {
			label := wr.Name
			if label == "" {
				label = "unnamed"
			}
			return Config{}, fmt.Errorf("rule %d (%s): %w", i+1, label, err)
		}
		cfg.Rules = append(cfg.Rules, r)
	}
	return cfg, nil
}

func (w wireRule) rule() (Rule, error) {
	r := Rule{
		Name: w.Name, On: forge.EventType(w.On), From: w.From, To: w.To,
		Conclusion: w.Conclusion, Author: w.Author, Branch: w.Branch,
		Command: w.Command, Builtin: w.Builtin, Timeout: defaultTimeout,
	}
	if len(w.Repos) == 0 {
		return Rule{}, fmt.Errorf("repos is required")
	}
	for _, s := range w.Repos {
		repo, err := forge.ParseRepo(s)
		if err != nil {
			return Rule{}, err
		}
		r.Repos = append(r.Repos, repo)
	}
	if w.Timeout != "" {
		d, err := time.ParseDuration(w.Timeout)
		if err != nil {
			return Rule{}, fmt.Errorf("timeout: %w", err)
		}
		if d <= 0 {
			return Rule{}, fmt.Errorf("timeout must be positive, got %q", w.Timeout)
		}
		r.Timeout = d
	}
	if strings.ContainsAny(w.Branch, `[]{}\`) {
		return Rule{}, fmt.Errorf("branch %q: only * and ? are supported", w.Branch)
	}
	if w.Conclusion != "" {
		if r.On != forge.EventChecksCompleted {
			return Rule{}, fmt.Errorf("conclusion needs on = %q", forge.EventChecksCompleted)
		}
		if w.Conclusion != "pass" && w.Conclusion != "fail" {
			return Rule{}, fmt.Errorf("conclusion %q: want pass or fail", w.Conclusion)
		}
	}
	if w.Builtin != "" {
		return r, w.applyBuiltin(&r)
	}
	return r, w.validateCommand(r)
}

func (w wireRule) applyBuiltin(r *Rule) error {
	if w.Builtin != BuiltinRemoveMergedWorktree {
		return fmt.Errorf("unknown builtin %q", w.Builtin)
	}
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"on", w.On != ""}, {"from", w.From != ""}, {"to", w.To != ""},
		{"command", len(w.Command) > 0}, {"conclusion", w.Conclusion != ""},
		{"timeout", w.Timeout != ""},
	} {
		if f.set {
			return fmt.Errorf("%s is not allowed with builtin %q", f.name, w.Builtin)
		}
	}
	if len(r.Repos) != 1 {
		return fmt.Errorf("builtin %q needs exactly one repo", w.Builtin)
	}
	if w.Checkout == "" {
		return fmt.Errorf("builtin %q needs checkout", w.Builtin)
	}
	co, err := expandHome(w.Checkout)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(co) {
		return fmt.Errorf("checkout %q must be an absolute path", w.Checkout)
	}
	r.Checkout = co
	r.On = forge.EventStateChanged
	r.To = string(forge.StateMerged)
	return nil
}

func (w wireRule) validateCommand(r Rule) error {
	if w.Checkout != "" {
		return fmt.Errorf("checkout is only for builtin rules")
	}
	if len(r.Command) == 0 {
		return fmt.Errorf("one of command or builtin is required")
	}
	if !slices.Contains(eventTypes, r.On) {
		return fmt.Errorf("on %q: want an event type, one of %v", w.On, eventTypes)
	}
	for _, f := range []struct{ name, val string }{{"from", r.From}, {"to", r.To}} {
		if err := checkEventValue(r.On, f.name, f.val); err != nil {
			return err
		}
	}
	if placeholderToken.MatchString(r.Command[0]) {
		return fmt.Errorf("command[0] must not contain placeholders")
	}
	for _, el := range r.Command {
		for _, tok := range placeholderToken.FindAllString(el, -1) {
			if !slices.Contains(PlaceholderNames, tok[1:len(tok)-1]) {
				return fmt.Errorf("unknown placeholder %s in command", tok)
			}
		}
	}
	return nil
}

// eventValues lists the Old/New strings each event type carries. Events absent
// here (head.changed, review.submitted) carry free-form values.
var eventValues = map[forge.EventType][]string{
	forge.EventStateChanged: {string(forge.StateOpen), string(forge.StateClosed), string(forge.StateMerged)},
	forge.EventDraftChanged: {"ready", "draft"},
	forge.EventChecksStarted: {
		string(forge.RollupPass), string(forge.RollupFail), string(forge.RollupPending), string(forge.RollupNone),
	},
	forge.EventChecksCompleted: {
		string(forge.RollupPass), string(forge.RollupFail), string(forge.RollupPending), string(forge.RollupNone),
	},
	forge.EventReviewDecisionChanged: {
		string(forge.ReviewApproved), string(forge.ReviewChangesRequested),
		string(forge.ReviewRequired), string(forge.ReviewNone),
	},
	forge.EventMergeableChanged: {
		string(forge.MergeableClean), string(forge.MergeableConflict), string(forge.MergeableUnknown),
	},
}

func checkEventValue(on forge.EventType, field, val string) error {
	allowed, ok := eventValues[on]
	if val == "" || !ok || slices.Contains(allowed, val) {
		return nil
	}
	return fmt.Errorf("%s %q: not a value of %s, want one of %v", field, val, on, allowed)
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, p[1:]), nil
}
