package ghcompat

import (
	"bytes"
	"errors"
	"testing"
)

func TestApplyJQ(t *testing.T) {
	obj := map[string]any{"number": 5, "title": "fix <b> & co", "draft": false}
	list := []map[string]any{{"number": 3}, {"number": 1}}
	tests := []struct {
		name string
		v    any
		expr string
		want string
	}{
		{"number", obj, ".number", "5\n"},
		{"string is raw", obj, ".title", "fix <b> & co\n"},
		{"bool", obj, ".draft", "false\n"},
		{"iterate list", list, ".[] | .number", "3\n1\n"},
		{"object compact", obj, ".", `{"draft":false,"number":5,"title":"fix <b> & co"}` + "\n"},
		{"list compact", list, ".", `[{"number":3},{"number":1}]` + "\n"},
		{"null", obj, ".missing", "null\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := ApplyJQ(&buf, tc.v, tc.expr); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyJQCompileErrorIsJQError(t *testing.T) {
	var buf bytes.Buffer
	err := ApplyJQ(&buf, map[string]any{"number": 5}, ".[")
	var jerr *JQError
	if !errors.As(err, &jerr) {
		t.Fatalf("err = %v, want *JQError", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q on compile error", buf.String())
	}
}

func TestApplyJQRuntimeErrorIsNotJQError(t *testing.T) {
	var buf bytes.Buffer
	err := ApplyJQ(&buf, map[string]any{"number": 5}, ".number | keys")
	if err == nil {
		t.Fatal("want error")
	}
	var jerr *JQError
	if errors.As(err, &jerr) {
		t.Errorf("runtime error must not be a *JQError: %v", err)
	}
}
