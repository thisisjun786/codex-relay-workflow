package testsupport

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FrozenManifest is a frozen MANIFEST.json that no freeze writes: a record whose bytes, path or
// digest is another JSON type or value than the one a freeze writes, a constant (NaN, Infinity)
// only Python's JSON allows, a value nested as deep as json.loads can descend or deeper, or a
// corrupt document with CRLF or CR line ends. The fence reads each field as the value json.loads
// made of it, so every reader of a frozen copy answers these the way
// manifest.verify_frozen_detailed does, whether that is a problem, a clean copy or an exception.
type FrozenManifest struct {
	Name     string
	template string
}

// Document is the manifest's text for a frozen copy of one artifact, given that artifact's
// declared path and digest: %[1]s is the path and %[2]s the digest as JSON strings, and %[3]s
// the digest's bare hex.
func (m FrozenManifest) Document(path, digest string) string {
	quotedPath, _ := json.Marshal(path)
	quotedDigest, _ := json.Marshal(digest)
	return fmt.Sprintf(m.template, quotedPath, quotedDigest, digest)
}

// FrozenManifests are the crafted frozen documents the parity tests stage for every reader of
// a frozen copy: the receipt intake, the omission reader and the Stop hook. The artifact is the
// 19 bytes "the delivered bytes".
func FrozenManifests() []FrozenManifest {
	record := func(name, bytes string) FrozenManifest {
		return FrozenManifest{name, `{"entries": [{"path": %[1]s, "sha256": %[2]s, "bytes": ` + bytes + `}]}`}
	}
	// A good record beside a value nested levels deep. json.loads's C scanner spends one level
	// of the recursion budget on each container, and the installed console script calls it with
	// 9998 left: the outer object and 9997 nested containers are read, one more is RecursionError.
	nested := func(name string, levels int, open, close string) FrozenManifest {
		return FrozenManifest{name, `{"entries": [{"path": %[1]s, "sha256": %[2]s, "bytes": 19}], "x": ` + strings.Repeat(open, levels) + "1" + strings.Repeat(close, levels) + "}"}
	}
	return []FrozenManifest{
		record("frozen-bytes-numeric-string", `"19"`),
		record("frozen-bytes-list", `[19]`),
		record("frozen-bytes-empty-object", `{}`),
		record("frozen-bytes-nested-object", `{"n": [19, "x", null, true, 1.5, NaN, "it's"]}`),
		record("frozen-bytes-text", `"x"`),
		record("frozen-bytes-nan", `NaN`),
		record("frozen-bytes-negative-infinity", `-Infinity`),
		record("frozen-bytes-rounded-float", `19.000000000000001`),
		record("frozen-bytes-float", `19.0`),
		record("frozen-bytes-overflowing-float", `1e400`),
		record("frozen-bytes-true", `true`),
		record("frozen-bytes-big-integer", `100000000000000000000000000000019`),
		{"frozen-nan-elsewhere", `{"entries": [{"path": %[1]s, "sha256": %[2]s, "bytes": 19}], "checked": NaN}`},
		{"frozen-duplicate-keys", `{"entries": [{"path": 5, "path": %[1]s, "sha256": %[2]s, "bytes": "19", "bytes": 19}]}`},
		{"frozen-path-number", `{"entries": [{"path": 5, "sha256": %[2]s}]}`},
		{"frozen-path-list", `{"entries": [{"path": [1], "sha256": %[2]s}]}`},
		{"frozen-path-surrogates", `{"entries": [{"path": "/frozen/\ud800\udbff", "sha256": %[2]s}]}`},
		{"frozen-second-path-null", `{"entries": [{"path": %[1]s, "sha256": %[2]s, "bytes": 19}, {"path": null, "sha256": %[2]s}]}`},
		{"frozen-digest-null", `{"entries": [{"path": %[1]s, "sha256": null}]}`},
		{"frozen-digest-number", `{"entries": [{"path": %[1]s, "sha256": 5}]}`},
		{"frozen-digest-false", `{"entries": [{"path": %[1]s, "sha256": false}]}`},
		{"frozen-digest-object", `{"entries": [{"path": %[1]s, "sha256": {}}]}`},
		// A digest is 64 lowercase hex characters and nothing after them, a newline included.
		{"frozen-digest-newline", `{"entries": [{"path": %[1]s, "sha256": "%[3]s\n", "bytes": 19}]}`},
		nested("frozen-nested-at-the-limit", 9997, "[", "]"),
		nested("frozen-nested-past-the-limit", 9998, "[", "]"),
		nested("frozen-nested-objects-past-the-limit", 9998, `{"k": `, "}"),
		nested("frozen-nested-far-past-the-limit", 100000, "[", "]"),
		// Path.read_text() reads with universal newlines, so json.loads counts a CRLF or a CR as
		// the one line feed it became when it names where a document stops being JSON.
		{"frozen-corrupt-crlf", "{\"entries\": [\r\n{\"path\": %[1]s, \"sha256\": %[2]s},\r\n]}"},
		{"frozen-corrupt-cr", "{\"entries\": [\r{\"path\": %[1]s, \"sha256\": %[2]s},\r]}"},
	}
}
