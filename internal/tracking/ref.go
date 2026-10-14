package tracking

import "github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"

// Ref names an issue or milestone as "<repo>#N". It is the type of internal/tracking/ref, re-exported
// so the tracker's callers keep one spelling.
type Ref = ref.Ref

// ParseRef reads "<owner>/<name>#N".
func ParseRef(s string) (Ref, error) { return ref.Parse(s) }

// validRepo reports whether s is "owner/name" with neither part "." or "..".
func validRepo(s string) bool { return ref.ValidRepo(s) }
