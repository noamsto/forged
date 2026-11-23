package ghcompat

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/itchyny/gojq"
)

// JQError is a --jq expression that does not compile.
type JQError struct{ Err error }

func (e *JQError) Error() string { return "invalid jq expression: " + e.Err.Error() }
func (e *JQError) Unwrap() error { return e.Err }

// ApplyJQ runs expr over v and writes each result on its own line the way gh
// does: strings raw, everything else compact JSON with HTML left unescaped.
func ApplyJQ(w io.Writer, v any, expr string) error {
	query, err := gojq.Parse(expr)
	if err != nil {
		return &JQError{Err: err}
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return &JQError{Err: err}
	}

	// gojq only understands []any / map[string]any, not typed Go values.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return err
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	iter := code.Run(normalized)
	for {
		out, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := out.(error); isErr {
			return fmt.Errorf("jq: %w", err)
		}
		if s, isStr := out.(string); isStr {
			if _, err := io.WriteString(w, s+"\n"); err != nil {
				return err
			}
			continue
		}
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
}
