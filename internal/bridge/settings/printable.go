package settings

import "unicode"

// Printable is str.isprintable() of one character as CPython 3.14 answers it: under that
// interpreter's Unicode database (unicodedata 16.0.0), not Go's own tables, which follow a later
// Unicode version and would print a character assigned since then (U+0C5C, U+1ACF) that
// CPython 3.14 escapes as unassigned. A lone surrogate is not printable.
//
// The table, printable_generated.go, is frozen. Its generator (internal/bridge/settings/generate,
// run by go generate) read it from a CPython 3.14 on PATH, and was deleted with the Python
// execution path in todo 44 (decision 49): the bridge settings it serves are held to what the
// Python bridge answered, and TestPrintableIsTheFrozenTable holds the table to its golden,
// which began as that interpreter's answer.
func Printable(r rune) bool { return unicode.Is(pythonPrintable, r) }
