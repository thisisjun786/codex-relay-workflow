package settings

import "unicode"

//go:generate go run ./generate

// Printable is str.isprintable() of one character as CPython 3.14 answers it: under that
// interpreter's Unicode database (unicodedata 16.0.0), not Go's own tables, which follow a later
// Unicode version and would print a character assigned since then (U+0C5C, U+1ACF) that
// CPython 3.14 escapes as unassigned. A lone surrogate is not printable.
func Printable(r rune) bool { return unicode.Is(pythonPrintable, r) }
