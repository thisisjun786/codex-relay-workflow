//go:build dev

// Package pyload is json.loads for the development judges (crw-dev stop-events and
// crw-dev trial-ledger): the value hook.Decode builds from a record (ordered objects, int64 or
// json.Number integers, float64, WTF-8 strings), read by pyjson.Loads, which has no nesting limit
// of its own.
//
// CPython 3.14, the interpreter the judges answer for, has no nesting limit of its own: its C
// scanner recurses until the thread's stack runs out and raises RecursionError there. Loads
// models that edge as Nesting and returns the RecursionError past it, so a judge reads what
// Python reads and fails where Python fails rather than at encoding/json's 10000. The real edge
// moves with the stack and the container kind, which no model reproduces; docs/port/known-defects.md
// names the difference.
package pyload

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Nesting is how many containers deep a judge reads a record. CPython 3.14.4 on the default
// 8 MiB main-thread stack stopped at 57,930 (stop_events.py, a journal row), 57,929 and 57,955
// (trial_startup.py ledger, a ledger line and the start record): where exactly moves by tens with
// whatever else is on the stack. The model stays just below every edge measured, so a judge
// never reads a record the interpreter could not; between Nesting and the interpreter's edge it
// raises where Python still reads.
const Nesting = 57900

// Loads is json.loads(raw.decode("utf-8")): the decoded value, the ValueError Python raises
// (its message), or past Nesting the RecursionError, as an *evidence.PythonError a judge raises
// on, because neither reader it ports catches one.
func Loads(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	text := string(raw)
	if message, recursion := pyjson.ErrorWithLimit(text, Nesting); recursion {
		return nil, &evidence.PythonError{Class: "RecursionError", Detail: message}
	} else if message != "" {
		return nil, errors.New(message)
	}
	return pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers, Deep: true})
}

// Recursion is the RecursionError Loads returned, if it returned one.
func Recursion(err error) (*evidence.PythonError, bool) {
	var python *evidence.PythonError
	if errors.As(err, &python) && python.Class == "RecursionError" {
		return python, true
	}
	return nil, false
}
