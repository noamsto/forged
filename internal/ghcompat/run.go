package ghcompat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

// Run serves a `gh pr view|list` invocation from the daemon and returns the
// exit code. Anything it cannot answer exactly is exec'd to the real gh.
func Run(args []string, stdout, stderr io.Writer, env func(string) string) int {
	if err := serve(args, stdout, env); err != nil {
		return fallback(args, stderr, env, err)
	}
	return 0
}

func serve(args []string, stdout io.Writer, env func(string) string) error {
	req, fb := Parse(args)
	if fb != nil {
		return errors.New(fb.Reason)
	}
	ctx := context.Background()
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, fb := ResolveRepo(ctx, req, env, dir)
	if fb != nil {
		return errors.New(fb.Reason)
	}
	opts, err := readOptions(req, env)
	if err != nil {
		return err
	}

	c := client.New("")
	var out any
	switch req.Cmd {
	case "view":
		change, err := c.Change(ctx, repo, req.Number, opts)
		if err != nil {
			return err
		}
		if slices.Contains(req.Fields, "statusCheckRollup") && change.Checks.Truncated {
			return errors.New("check rollup is truncated")
		}
		out = Shape(change, req.Fields)
	default:
		changes, err := list(ctx, c, repo, req, opts)
		if err != nil {
			return err
		}
		shaped := make([]map[string]any, 0, len(changes))
		for _, ch := range changes {
			shaped = append(shaped, Shape(ch, req.Fields))
		}
		out = shaped
	}

	// Buffered so a mid-stream jq error leaves stdout empty before gh reruns.
	var buf bytes.Buffer
	if req.JQ != "" {
		err = ApplyJQ(&buf, out, req.JQ)
	} else {
		err = Encode(&buf, out, isTTY(stdout))
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(buf.Bytes())
	return err
}

func list(ctx context.Context, c *client.Client, repo forge.RepoRef, req Request, opts client.ReadOptions) ([]forge.Change, error) {
	var changes []forge.Change
	var err error
	if req.Head != "" {
		changes, err = c.ByBranch(ctx, repo, req.Head, opts)
	} else {
		changes, err = c.ListOpen(ctx, repo, opts)
	}
	if err != nil {
		return nil, err
	}
	return SelectList(changes, req.State, req.Limit), nil
}

func readOptions(req Request, env func(string) string) (client.ReadOptions, error) {
	if req.MaxAge != nil {
		return client.ReadOptions{MaxAge: req.MaxAge}, nil
	}
	v := env("FORGED_MAX_AGE")
	if v == "" {
		return client.ReadOptions{}, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return client.ReadOptions{}, fmt.Errorf("invalid FORGED_MAX_AGE %q", v)
	}
	return client.ReadOptions{MaxAge: &d}, nil
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func fallback(args []string, stderr io.Writer, env func(string) string, cause error) int {
	if env("FORGED_NO_FALLBACK") == "1" {
		_, _ = fmt.Fprintf(stderr, "forged: %v\n", cause)
		return 1
	}
	path, err := exec.LookPath("gh")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "forged: %v; gh not found for fallback: %v\n", cause, err)
		return 1
	}
	err = syscall.Exec(path, append([]string{"gh"}, withoutMaxAge(args)...), os.Environ()) //nolint:gosec // G204: fixed gh invocation
	_, _ = fmt.Fprintf(stderr, "forged: exec gh: %v\n", err)
	return 1
}

// withoutMaxAge drops forged's own --max-age flag, which gh rejects.
func withoutMaxAge(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--max-age":
			i++
		case strings.HasPrefix(args[i], "--max-age="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}
