package testsupport

import (
	"encoding/json"
	"fmt"
)

// FrozenManifest is a frozen MANIFEST.json that no freeze writes and that json.loads reads all
// the same: a record whose bytes, path or digest is another JSON type than the one a freeze
// writes, or a constant (NaN, Infinity) only Python's JSON allows. The fence reads each field as
// the value json.loads made of it, so every reader of a frozen copy answers these the way
// manifest.verify_frozen_detailed does, whether that is a problem, a clean copy or an exception.
type FrozenManifest struct {
	Name     string
	template string
}

// Document is the manifest's text for a frozen copy of one artifact, given that artifact's
// declared path and digest.
func (m FrozenManifest) Document(path, digest string) string {
	quotedPath, _ := json.Marshal(path)
	quotedDigest, _ := json.Marshal(digest)
	return fmt.Sprintf(m.template, quotedPath, quotedDigest)
}

// FrozenManifests are the crafted frozen documents the parity tests stage for every reader of
// a frozen copy: the receipt intake, the omission reader and the Stop hook. The artifact is the
// 19 bytes "the delivered bytes".
func FrozenManifests() []FrozenManifest {
	record := func(name, bytes string) FrozenManifest {
		return FrozenManifest{name, `{"entries": [{"path": %[1]s, "sha256": %[2]s, "bytes": ` + bytes + `}]}`}
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
	}
}
