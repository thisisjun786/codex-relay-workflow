// Package quote is how a relay message names a value it shows a person: a string as Go quotes
// it, any other value as its compact JSON (Value), and a word of a command line as a shell reads
// it back (Shell). It stands where the Python relay's messages used repr() and shlex.quote.
// Nothing stored or hashed is spelled with Value: a message whose text a store, a journal or a
// hash keeps quotes with pyvalue.Repr, the stored spelling.
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
