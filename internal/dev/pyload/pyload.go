//go:build dev

// Package pyload reads the records the development judges grade (crw-dev stop-events and
// crw-dev trial-ledger): journal rows, host ledgers and trial ledgers that Python and Go writers
// left on the host. A reader of stored data keeps accepting what any earlier writer wrote, so a
// record reads as Python's json.dumps could have written it: NaN and the infinities, a lone
// surrogate escape (held in the WTF-8 bytes a Go string holds it in), objects in their key order
// with a repeated key's last value, and containers as deep as MaxNesting. A refusal is in
// encoding/json's words.
package pyload

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// MaxNesting is how many containers deep a record may nest. The Python writers read, and so
// wrote, records as deep as CPython 3.14's json nests on the default 8 MiB stack, which stopped
// between 57,929 and 57,955 containers; the cap stays just below every edge measured, past
// encoding/json's 10000.
const MaxNesting = 57900

// errNotUTF8 refuses a record whose bytes are not UTF-8 text.
var errNotUTF8 = errors.New("the record is not UTF-8 text")

// stored is how a judge reads a record's values: what hook.Decode reads a record into, at any
// depth MaxNesting allows.
var stored = pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers, Deep: true}

// Loads is the value one record holds.
func Loads(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errNotUTF8
	}
	if nesting(raw) > MaxNesting {
		return nil, fmt.Errorf("the record nests deeper than %d containers", MaxNesting)
	}
	return pyjson.Loads(string(raw), stored)
}

// nesting is how deep raw's brackets nest outside its strings: the depth of a well-formed
// document, and a bound the reader then refuses a malformed one within.
func nesting(raw []byte) int {
	deepest, open, inString, escaped := 0, 0, false, false
	for _, c := range raw {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			open++
			deepest = max(deepest, open)
		case c == ']' || c == '}':
			open--
		}
	}
	return deepest
}
