// Package quote is how a relay message names a value it shows a person: a string as Go quotes
// it, any other value as its compact JSON. It stands where the Python relay's messages used
// repr(). Nothing stored or hashed is spelled with it: a message whose text a store, a journal
// or a hash keeps quotes with pyvalue.Repr, the stored spelling.
package quote

import (
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Value is v as a message shows it: a string as %q (an invisible character escaped, "x\u00a0y"),
// any other value as its compact JSON with the text left unescaped (null, true, [1,2],
// {"a":"é"}).
func Value(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return pyjson.Dumps(v, pyjson.Options{Compact: true, Unicode: true})
}
