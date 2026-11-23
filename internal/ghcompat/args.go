package ghcompat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListLimit = 30
	maxHeadLimit     = 100
)

// Request is a parsed `gh pr view|list` invocation inside the supported subset.
type Request struct {
	Cmd    string // "view" or "list"
	Repo   string
	Number int
	URL    string
	Head   string
	State  string
	Limit  int
	Fields []string
	JQ     string
	MaxAge *time.Duration
}

// Fallback reports why an invocation must be handed to the real gh.
type Fallback struct{ Reason string }

func fallbackf(format string, a ...any) *Fallback {
	return &Fallback{Reason: fmt.Sprintf(format, a...)}
}

var (
	prURLRe = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+/pull/([0-9]+)/?$`)

	longFlags = map[string]string{
		"repo": "repo", "json": "json", "jq": "jq", "max-age": "max-age",
		"head": "head", "state": "state", "limit": "limit",
	}
	shortFlags = map[string]string{
		"R": "repo", "q": "jq", "H": "head", "s": "state", "L": "limit",
	}
	listOnlyFlags = map[string]bool{"head": true, "state": true, "limit": true}
)

// Parse maps gh's argv (after the program name) onto a Request, or reports why
// the real gh has to handle it.
func Parse(args []string) (Request, *Fallback) {
	if len(args) < 2 || args[0] != "pr" || (args[1] != "view" && args[1] != "list") {
		return Request{}, fallbackf("not pr view/list")
	}
	req := Request{Cmd: args[1]}
	flags, positional, fb := splitArgs(args[2:])
	if fb != nil {
		return Request{}, fb
	}
	if req.Cmd == "view" {
		for name := range listOnlyFlags {
			if _, ok := flags[name]; ok {
				return Request{}, fallbackf("--%s is not supported by pr view", name)
			}
		}
	}

	req.Repo = flags["repo"]
	req.JQ = flags["jq"]
	if v, ok := flags["max-age"]; ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Request{}, fallbackf("invalid --max-age %q", v)
		}
		req.MaxAge = &d
	}
	if fb := parseFields(&req, flags["json"]); fb != nil {
		return Request{}, fb
	}

	if req.Cmd == "view" {
		return req, parseViewTarget(&req, positional)
	}
	if len(positional) > 0 {
		return Request{}, fallbackf("unexpected argument %q", positional[0])
	}
	return req, parseListOptions(&req, flags)
}

// splitArgs separates positionals from flags. Value flags take `=value`, an
// attached short value (-L5) or the next argument.
func splitArgs(args []string) (flags map[string]string, positional []string, fb *Fallback) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var name, val string
		var hasVal bool
		switch {
		case strings.HasPrefix(arg, "--"):
			var ok bool
			name, val, hasVal = strings.Cut(arg[2:], "=")
			if name, ok = longFlags[name]; !ok {
				return nil, nil, fallbackf("unsupported flag %s", arg)
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			name, ok := shortFlags[arg[1:2]]
			if !ok {
				return nil, nil, fallbackf("unsupported flag %s", arg)
			}
			if len(arg) > 2 {
				flags[name] = arg[2:]
				continue
			}
			i++
			if i == len(args) {
				return nil, nil, fallbackf("flag %s needs a value", arg)
			}
			flags[name] = args[i]
			continue
		default:
			positional = append(positional, arg)
			continue
		}
		if !hasVal {
			i++
			if i == len(args) {
				return nil, nil, fallbackf("flag %s needs a value", arg)
			}
			val = args[i]
		}
		flags[name] = val
	}
	return flags, positional, nil
}

func parseFields(req *Request, json string) *Fallback {
	if json == "" {
		return fallbackf("--json with fields is required")
	}
	req.Fields = strings.Split(json, ",")
	for _, f := range req.Fields {
		if _, ok := fieldShapers[f]; !ok {
			return fallbackf("unsupported field %q", f)
		}
	}
	return nil
}

func parseViewTarget(req *Request, positional []string) *Fallback {
	if len(positional) != 1 {
		return fallbackf("pr view needs exactly one number or URL")
	}
	target := positional[0]
	if n, err := strconv.Atoi(target); err == nil && n > 0 {
		req.Number = n
		return nil
	}
	m := prURLRe.FindStringSubmatch(target)
	if m == nil {
		return fallbackf("unsupported pr view target %q", target)
	}
	req.Number, _ = strconv.Atoi(m[1])
	req.URL = target
	return nil
}

func parseListOptions(req *Request, flags map[string]string) *Fallback {
	req.Head = flags["head"]
	req.State = "open"
	if s, ok := flags["state"]; ok {
		req.State = s
	}
	switch req.State {
	case "open", "closed", "merged", "all":
	default:
		return fallbackf("unsupported state %q", req.State)
	}
	if req.Head == "" && req.State != "open" {
		return fallbackf("--state %s needs --head", req.State)
	}

	req.Limit = defaultListLimit
	if l, ok := flags["limit"]; ok {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			return fallbackf("invalid --limit %q", l)
		}
		req.Limit = n
	}
	if req.Head != "" && req.Limit > maxHeadLimit {
		return fallbackf("--head with --limit above %d", maxHeadLimit)
	}
	return nil
}
