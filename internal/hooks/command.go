package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/forged/forge"
)

const (
	waitDelay      = 2 * time.Second
	maxOutputBytes = 64 << 10
)

// Event is the JSON payload a hook command receives on stdin.
type Event struct {
	Event  forge.ChangeEvent `json:"event"`
	Change forge.Change      `json:"change"`
}

var placeholderRE = regexp.MustCompile(`\{(` + strings.Join(PlaceholderNames, "|") + `)\}`)

func placeholderValues(ev forge.ChangeEvent, ch forge.Change) map[string]string {
	number := ev.Number
	if number == 0 {
		number = ch.Number
	}
	return map[string]string{
		"repo":   ev.Repo.String(),
		"number": strconv.Itoa(number),
		"url":    ev.URL,
		"type":   string(ev.Type),
		"old":    ev.Old,
		"new":    ev.New,
		"branch": ch.Head.Ref,
		"base":   ch.Base.Ref,
		"sha":    ch.Head.SHA,
		"title":  ch.Title,
		"author": ch.Author.Login,
	}
}

// expandArgv substitutes placeholders inside each element in one
// non-recursive pass, so values can neither add elements nor be re-expanded.
// An element whose template starts with a placeholder that expands to an
// option-looking value is refused unless a literal "--" precedes it.
func expandArgv(tmpl []string, ev forge.ChangeEvent, ch forge.Change) ([]string, error) {
	vals := placeholderValues(ev, ch)
	out := make([]string, len(tmpl))
	afterDashes := false
	for i, el := range tmpl {
		out[i] = placeholderRE.ReplaceAllStringFunc(el, func(tok string) string {
			return vals[tok[1:len(tok)-1]]
		})
		if el == "--" {
			afterDashes = true
		}
		if loc := placeholderRE.FindStringIndex(el); !afterDashes && loc != nil && loc[0] == 0 &&
			strings.HasPrefix(out[i], "-") {
			return nil, fmt.Errorf("argument %d expands to an option-like value; put a literal -- before it", i)
		}
	}
	return out, nil
}

// commandEnv drops inherited FORGED_* (except FORGED_SOCKET) and appends the event variables.
func commandEnv(inherited []string, ev forge.ChangeEvent, ch forge.Change) []string {
	env := make([]string, 0, len(inherited)+11)
	for _, kv := range inherited {
		if strings.HasPrefix(kv, "FORGED_") && !strings.HasPrefix(kv, "FORGED_SOCKET=") {
			continue
		}
		env = append(env, kv)
	}
	v := placeholderValues(ev, ch)
	return append(env,
		"FORGED_EVENT_TYPE="+v["type"],
		"FORGED_REPO="+v["repo"],
		"FORGED_NUMBER="+v["number"],
		"FORGED_URL="+v["url"],
		"FORGED_OLD="+v["old"],
		"FORGED_NEW="+v["new"],
		"FORGED_BRANCH="+v["branch"],
		"FORGED_BASE="+v["base"],
		"FORGED_SHA="+v["sha"],
		"FORGED_TITLE="+v["title"],
		"FORGED_AUTHOR="+v["author"],
	)
}

// cappedBuffer keeps the first max bytes and silently drops the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// runCommand executes the rule's argv directly, never through a shell.
func runCommand(ctx context.Context, r Rule, ev forge.ChangeEvent, ch forge.Change, log *slog.Logger) error {
	argv, err := expandArgv(r.Command, ev, ch)
	if err != nil {
		return fmt.Errorf("rule %q: %w", r.Name, err)
	}
	payload, err := json.Marshal(Event{Event: ev, Change: ch})
	if err != nil {
		return err
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &cappedBuffer{max: maxOutputBytes}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv-only, no shell; argv[0] is a literal from the config
	cmd.Env = commandEnv(os.Environ(), ev, ch)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = waitDelay

	err = cmd.Run()
	attrs := []any{"rule", r.Name, "command", argv[0], "output", out.buf.String()}
	if err != nil {
		log.Warn("hook command failed", append(attrs, "error", err)...)
		if ctx.Err() != nil {
			return fmt.Errorf("rule %q: command %s: %w", r.Name, argv[0], ctx.Err())
		}
		return fmt.Errorf("rule %q: command %s: %w", r.Name, argv[0], err)
	}
	log.Debug("hook command succeeded", attrs...)
	return nil
}
