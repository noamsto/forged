package cli

import (
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/ghcompat"
)

var skillFence = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")

func skillCommands(t *testing.T) [][]string {
	t.Helper()
	body, err := os.ReadFile("../../skills/forged/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	var cmds [][]string
	for _, m := range skillFence.FindAllStringSubmatch(string(body), -1) {
		for line := range strings.SplitSeq(m[1], "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "forged" {
				cmds = append(cmds, f[1:])
			}
		}
	}
	return cmds
}

func TestSkillCommandsMatchCLI(t *testing.T) {
	cmds := skillCommands(t)
	seen := map[string]bool{}
	for _, args := range cmds {
		seen[args[0]] = true
		switch args[0] {
		case "pr":
			if _, fb := ghcompat.Parse(args); fb != nil {
				t.Errorf("forged %s: would fall back to gh: %s", strings.Join(args, " "), fb.Reason)
			}
		case "watch":
			fs, wf := newWatchFlagSet(io.Discard)
			if err := fs.Parse(args[1:]); err != nil {
				t.Errorf("forged %s: %v", strings.Join(args, " "), err)
			}
			if fs.NArg() != 0 || wf.repo == "" {
				t.Errorf("forged %s: needs flags only and -R", strings.Join(args, " "))
			}
			for name := range strings.SplitSeq(wf.events, ",") {
				if wf.events != "" && !slices.Contains(eventTypes, forge.EventType(name)) {
					t.Errorf("forged %s: unknown event %q", strings.Join(args, " "), name)
				}
			}
		case "invalidate":
			if len(args) != 2 {
				t.Errorf("forged %s: want exactly one key", strings.Join(args, " "))
			} else if _, err := parseKey(args[1]); err != nil {
				t.Errorf("forged %s: %v", strings.Join(args, " "), err)
			}
		default:
			t.Errorf("forged %s: command not covered by the skill test", strings.Join(args, " "))
		}
	}
	for _, c := range []string{"pr", "watch", "invalidate"} {
		if !seen[c] {
			t.Errorf("SKILL.md has no %s example", c)
		}
	}
}
