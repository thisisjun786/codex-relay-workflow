package pyvalue

import (
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Quote is how a message names a value it shows a person: a string as Go quotes it (%q), any
// other value as its compact JSON with the text left unescaped. It stands where a message quoted
// with repr(); nothing stored or hashed is spelled with it.
func Quote(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return pyjson.Dumps(v, pyjson.Options{Compact: true, Unicode: true})
}
