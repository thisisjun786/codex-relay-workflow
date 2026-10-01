package capacity

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// repr is repr() of a str, for caller-visible text; a float's repr is pyjson.Float.
func repr(text string) string { return store.PythonRepr(text) }
