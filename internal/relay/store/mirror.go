package store

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// MirrorRefusal is ownership.mirror's refusal for takeover.json bytes that are present:
// "" when json.loads reads them as an object, otherwise the OwnershipRefused detail. It is
// the one reading of the mirror's bytes behind doctor's ownership block and its probe.
func MirrorRefusal(raw []byte) string {
	_, why := MirrorDocument(raw)
	return why
}

// MirrorDocument is ownership.mirror's json.loads(bytes) of takeover.json: the text the bytes
// decode to (json.detect_encoding's UTF-8, behind its byte order mark or not, UTF-16 or UTF-32,
// pyjson.DecodeBytes), which json.loads reads as an object, or why it refuses them.
func MirrorDocument(raw []byte) (text, why string) {
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return "", "takeover record unreadable: UnicodeDecodeError: " + err.Error()
	}
	if message := pyjson.DecodedError(text); message != "" {
		return "", "takeover record unreadable: JSONDecodeError: " + message
	}
	// The document is valid JSON, so its first non-whitespace character names its type.
	if !strings.HasPrefix(strings.TrimLeft(text, " \t\n\r"), "{") {
		return "", "takeover record is not an object"
	}
	return text, ""
}
