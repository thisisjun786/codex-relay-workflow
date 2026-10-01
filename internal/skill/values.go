package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// How the skill commands read their JSON inputs (fixtures, observation and capability records,
// requests) and name a value or a failure in a message.

var errNotUTF8 = errors.New("not UTF-8 text")

// recordValues reads a record as the relay's own records are read (hook.Decode's values):
// objects in key order, an integer an int64 (a json.Number past it), NaN and the infinities
// floats, and a lone surrogate escape kept, which a recorded claim can hold.
var recordValues = pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers}

// decodeJSON is the one JSON document raw holds, which must be UTF-8 text.
func decodeJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errNotUTF8
	}
	return pyjson.Loads(string(raw), recordValues)
}

// readFailure is a file a command could not read; the command reports it and exits 3.
type readFailure struct{ err error }

func (e *readFailure) Error() string { return e.err.Error() }

// fileError is err, a failed read, naming the file as label.
func fileError(err error, label string) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return &fs.PathError{Op: pathErr.Op, Path: label, Err: pathErr.Err}
	}
	return err
}

// readJSON is the document the file name in fsys holds, named label in a failure: a read failure
// is a *readFailure, a decode failure its own error.
func readJSON(fsys fs.FS, name, label string) (any, error) {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, &readFailure{fileError(err, label)}
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return value, nil
}

// readJSONFile is readJSON for a path the operator named.
func readJSONFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &readFailure{err}
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

// reportProbeFailure prints what stopped hook-probe: a file it could not read exits 3, anything
// else 1.
func reportProbeFailure(stderr io.Writer, err error) int {
	var failure *readFailure
	if errors.As(err, &failure) {
		fmt.Fprintf(stderr, "Probe failed: %s. Nothing was written.\n", failure)
		return 3
	}
	fmt.Fprintln(stderr, err)
	return 1
}

// kindOf is a JSON value's kind, as a message names it.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case contract.OrderedObject, map[string]any:
		return "an object"
	}
	return "a number"
}

// notObject refuses a value read where an object belongs.
func notObject(v any) error { return fmt.Errorf("expected a JSON object, found %s", kindOf(v)) }

// notList refuses a value read where a list of values belongs.
func notList(v any) error { return fmt.Errorf("expected a JSON array, found %s", kindOf(v)) }

// show is a value as a message names it: its JSON text.
func show(v any) string { return pyjson.Dumps(v, pyjson.Options{}) }

// sortValues sorts values that can be put in order: numbers (and booleans, as 0 and 1) by
// value, strings by code point, arrays element by element. A pair that cannot be ordered, such
// as a string and a number or two objects, is refused.
func sortValues(items []any) ([]any, error) {
	out := append([]any(nil), items...)
	var refused error
	sort.SliceStable(out, func(i, j int) bool {
		less, err := lessValue(out[i], out[j])
		if err != nil && refused == nil {
			refused = err
		}
		return less
	})
	if refused != nil {
		return nil, refused
	}
	return out, nil
}

func lessValue(v, w any) (bool, error) {
	if x, ok := numberValue(v); ok {
		if y, ok := numberValue(w); ok {
			return x < y, nil
		}
	}
	switch x := v.(type) {
	case string:
		if y, ok := w.(string); ok {
			return x < y, nil
		}
	case []any:
		if y, ok := w.([]any); ok {
			for i := 0; i < len(x) && i < len(y); i++ {
				if !pyvalue.ItemEqual(x[i], y[i]) {
					return lessValue(x[i], y[i])
				}
			}
			return len(x) < len(y), nil
		}
	}
	return false, fmt.Errorf("cannot order %s and %s", kindOf(v), kindOf(w))
}

func numberValue(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	}
	return 0, false
}
