package cli

import (
	"os"
	"regexp"
	"testing"

	"github.com/noamsto/forged/internal/hooks"
)

var tomlFence = regexp.MustCompile("(?s)```toml\n(.*?)```")

func TestDocsTOMLBlocksParse(t *testing.T) {
	for _, path := range []string{"../../README.md", "../../skills/forged/SKILL.md"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		blocks := tomlFence.FindAllStringSubmatch(string(body), -1)
		if len(blocks) == 0 {
			t.Errorf("%s has no toml block", path)
		}
		for i, m := range blocks {
			if _, err := hooks.Parse([]byte(m[1])); err != nil {
				t.Errorf("%s toml block %d: %v", path, i+1, err)
			}
		}
	}
}
