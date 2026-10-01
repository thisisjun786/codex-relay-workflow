package pyvalue

import "github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"

// StrRepr is repr() of a str: settings.Repr, the one implementation, beside the table of what
// CPython 3.14 prints (settings.Printable) and that table's recorded check.
func StrRepr(s string) string { return settings.Repr(s) }
