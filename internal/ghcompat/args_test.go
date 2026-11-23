package ghcompat

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func fields(s string) []string { return strings.Split(s, ",") }

func TestParseSupported(t *testing.T) {
	const prURL = "https://github.com/o/r/pull/42"
	tests := []struct {
		name string
		args string
		want Request
	}{
		{"view number", "pr view 5 --json number,title", Request{Cmd: "view", Number: 5, Fields: fields("number,title")}},
		{"view url", "pr view " + prURL + " --json number", Request{Cmd: "view", Number: 42, URL: prURL, Fields: fields("number")}},
		{"view -R", "pr view 5 -R o/r --json number", Request{Cmd: "view", Number: 5, Repo: "o/r", Fields: fields("number")}},
		{"view -Rowner/repo", "pr view 5 -Ro/r --json number", Request{Cmd: "view", Number: 5, Repo: "o/r", Fields: fields("number")}},
		{"view --repo=", "pr view 5 --repo=o/r --json number", Request{Cmd: "view", Number: 5, Repo: "o/r", Fields: fields("number")}},
		{"view --repo sep", "pr view 5 --repo o/r --json number", Request{Cmd: "view", Number: 5, Repo: "o/r", Fields: fields("number")}},
		{"view --json=", "pr view 5 --json=number,state", Request{Cmd: "view", Number: 5, Fields: fields("number,state")}},
		{"view flags before arg", "pr view -R o/r --json number 5", Request{Cmd: "view", Number: 5, Repo: "o/r", Fields: fields("number")}},
		{"view -q", "pr view 5 --json number -q .number", Request{Cmd: "view", Number: 5, Fields: fields("number"), JQ: ".number"}},
		{"view --jq=", "pr view 5 --json number --jq=.number", Request{Cmd: "view", Number: 5, Fields: fields("number"), JQ: ".number"}},
		{"view max-age 0", "pr view 5 --json number --max-age 0", Request{Cmd: "view", Number: 5, Fields: fields("number"), MaxAge: new(time.Duration(0))}},
		{"view max-age=", "pr view 5 --json number --max-age=30s", Request{Cmd: "view", Number: 5, Fields: fields("number"), MaxAge: new(30 * time.Second)}},
		{"view all fields", "pr view 5 --json " + strings.Join(allFields, ","), Request{Cmd: "view", Number: 5, Fields: allFields}},
		{"list defaults", "pr list --json number", Request{Cmd: "list", State: "open", Limit: 30, Fields: fields("number")}},
		{"list -R -L5", "pr list -R o/r -L5 --json number", Request{Cmd: "list", Repo: "o/r", State: "open", Limit: 5, Fields: fields("number")}},
		{"list --limit", "pr list --limit 7 --json number", Request{Cmd: "list", State: "open", Limit: 7, Fields: fields("number")}},
		{"list --limit=", "pr list --limit=7 --json number", Request{Cmd: "list", State: "open", Limit: 7, Fields: fields("number")}},
		{"list explicit open", "pr list --state open --json number", Request{Cmd: "list", State: "open", Limit: 30, Fields: fields("number")}},
		{"list --head", "pr list --head feat/x --json number", Request{Cmd: "list", Head: "feat/x", State: "open", Limit: 30, Fields: fields("number")}},
		{"list -H", "pr list -H feat/x --json number", Request{Cmd: "list", Head: "feat/x", State: "open", Limit: 30, Fields: fields("number")}},
		{"list --head=", "pr list --head=feat/x --json number", Request{Cmd: "list", Head: "feat/x", State: "open", Limit: 30, Fields: fields("number")}},
		{"list head -s all", "pr list -H feat/x -s all --json number", Request{Cmd: "list", Head: "feat/x", State: "all", Limit: 30, Fields: fields("number")}},
		{"list head --state closed", "pr list --head feat/x --state closed --json number", Request{Cmd: "list", Head: "feat/x", State: "closed", Limit: 30, Fields: fields("number")}},
		{"list head merged", "pr list -H feat/x -s merged --json number", Request{Cmd: "list", Head: "feat/x", State: "merged", Limit: 30, Fields: fields("number")}},
		{"list head limit 100", "pr list -H feat/x -L 100 --json number", Request{Cmd: "list", Head: "feat/x", State: "open", Limit: 100, Fields: fields("number")}},
		{"list limit 200 no head", "pr list -L 200 --json number", Request{Cmd: "list", State: "open", Limit: 200, Fields: fields("number")}},
		{"list jq", "pr list --json number -q .[].number", Request{Cmd: "list", State: "open", Limit: 30, Fields: fields("number"), JQ: ".[].number"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fb := Parse(strings.Fields(tt.args))
			if fb != nil {
				t.Fatalf("unexpected fallback: %s", fb.Reason)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseFallback(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{"no args", ""},
		{"pr alone", "pr"},
		{"view without json", "pr view 5"},
		{"list without json", "pr list"},
		{"unknown field", "pr view 5 --json number,comments"},
		{"unknown field list", "pr list --json comments"},
		{"empty json", "pr view 5 --json="},
		{"template", "pr view 5 --json number --template {{.number}}"},
		{"template short", "pr view 5 --json number -t x"},
		{"web", "pr view 5 --web --json number"},
		{"comments", "pr view 5 --json number -c"},
		{"comments long", "pr view 5 --json number --comments"},
		{"unknown flag", "pr view 5 --json number --bogus"},
		{"unknown short flag", "pr list --json number -x"},
		{"list closed without head", "pr list --state closed --json number"},
		{"list merged without head", "pr list -s merged --json number"},
		{"list all without head", "pr list -s all --json number"},
		{"list bad state", "pr list -H b -s weird --json number"},
		{"head with limit 101", "pr list --head b --limit 101 --json number"},
		{"list non-numeric limit", "pr list -L x --json number"},
		{"list zero limit", "pr list -L 0 --json number"},
		{"view no arg", "pr view --json number"},
		{"view branch", "pr view feat/branch --json number"},
		{"view non-github url", "pr view https://gitlab.com/o/r/pull/3 --json number"},
		{"view ghe url", "pr view https://ghe.example.com/o/r/pull/3 --json number"},
		{"view issue url", "pr view https://github.com/o/r/issues/3 --json number"},
		{"view extra arg", "pr view 5 6 --json number"},
		{"list positional", "pr list extra --json number"},
		{"pr checkout", "pr checkout 5"},
		{"pr create", "pr create --json number"},
		{"issue list", "issue list --json number"},
		{"repo view", "repo view --json name"},
		{"missing flag value", "pr view 5 --json"},
		{"bad max-age", "pr view 5 --json number --max-age soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fb := Parse(strings.Fields(tt.args))
			if fb == nil || fb.Reason == "" {
				t.Fatalf("want fallback with a reason, got %+v", fb)
			}
		})
	}
}
