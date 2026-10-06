//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The state target is registered and its oracle is a Node worker.
func TestStateTargetIsRegistered(t *testing.T) {
	if !slices.Contains(Names(), "state") {
		t.Fatalf("state is not registered: %v", Names())
	}
	target, ok := Lookup("state")
	if !ok {
		t.Fatal("state is not registered")
	}
	if target.Oracle.Command != "node" {
		t.Fatalf("oracle %+v is not a Node worker", target.Oracle)
	}
}

// stateGo reads a session file the case wrote, rewrites it, and reports the rewritten bytes with the
// write timestamp masked, all with no Node and no python3.
func TestStateGoReadsAndRewrites(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, ".crw/sessions/s.json", `{"phase": "P", "slug": "my-slug"}`)
	answer, err := stateGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if unreadable, _ := field(answer, "unreadable"); unreadable != false {
		t.Fatalf("a valid state read as unreadable: %v", unreadable)
	}
	stateText, _ := field(answer, "state")
	if text, _ := stateText.(string); !strings.Contains(text, `"phase": "P"`) {
		t.Fatalf("the rebuilt state %q lost its phase", stateText)
	}
	written, _ := field(answer, "written")
	text, _ := written.(string)
	if !strings.Contains(text, `"phase": "P"`) || !strings.Contains(text, "@TS@") {
		t.Fatalf("the rewritten bytes %q are not the masked rewrite", written)
	}
}

// A file that is not a valid state reads as unreadable, so a gate that must fail closed can tell.
func TestStateGoMarksAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, ".crw/sessions/s.json", `{`)
	answer, err := stateGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if unreadable, _ := field(answer, "unreadable"); unreadable != true {
		t.Fatalf("a broken file read as readable: %v", unreadable)
	}
}

// writeStateFile puts one case file under a root.
func writeStateFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stateDoc is one state document text holding an updatedAt beside a persisted recordedAt.
func stateDoc(updatedAt, recordedAt string) string {
	return "{\n  \"phase\": \"P\",\n  \"updatedAt\": \"" + updatedAt + "\",\n  \"unverifiedSubagents\": [\n    {\n      \"agentId\": \"a\",\n      \"recordedAt\": \"" + recordedAt + "\"\n    }\n  ]\n}"
}

// stateAnswerText is one state answer in the real shape: "state" is the rebuilt form, whose updatedAt
// is the persisted value (readUpdatedAt), and "written" is the published form, whose updatedAt the
// write restamped from its own clock (writeStamp). Both are masked the way stateGo masks them, with
// maskRead saying whether the reader defaulted the read form's updatedAt.
func stateAnswerText(readUpdatedAt, recordedAt, writeStamp string, maskRead bool) any {
	return maskTimestamps(pyjson.Object{
		{Key: "unreadable", Value: false},
		{Key: "state", Value: stateDoc(readUpdatedAt, recordedAt)},
		{Key: "written", Value: stateDoc(writeStamp, recordedAt)},
	}, maskRead)
}

// A persisted timestamp is compared as stored: the mask covers only updatedAt, the key a write
// restamps from the wall clock, so two answers that differ only in a persisted recordedAt are a
// difference rather than a Same (generation 3, c8: the old text-wide mask hid exactly this).
func TestStateCompareSeesAPersistedTimestampChange(t *testing.T) {
	verdict := stateCompare(
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z", false),
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-02-03T04:05:06.789Z", "2026-06-06T06:06:06.000Z", false),
	)
	if verdict.Kind != Differ {
		t.Fatalf("a moved persisted recordedAt compared %v (%s), want Differ", verdict.Kind, verdict.Detail)
	}
}

// Two answers that differ only in the write's stamped updatedAt stay Same: the write restamps it from
// the wall clock on both sides, so its two values can never match.
func TestStateCompareIgnoresTheStampedUpdatedAt(t *testing.T) {
	verdict := stateCompare(
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z", false),
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-09-09T09:09:09.999Z", false),
	)
	if verdict.Kind != Same {
		t.Fatalf("two wall-clock updatedAt stamps compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
}

// A persisted read updatedAt is compared as stored: when the source file carried one, both sides keep
// it, so a port that rewrites it to another instant is a difference rather than a Same.
func TestStateCompareSeesAPersistedReadUpdatedAtChange(t *testing.T) {
	verdict := stateCompare(
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z", false),
		stateAnswerText("2026-05-05T05:05:05.001Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z", false),
	)
	if verdict.Kind != Differ {
		t.Fatalf("a moved persisted read updatedAt compared %v (%s), want Differ", verdict.Kind, verdict.Detail)
	}
}

// When the source file carried no updatedAt, the reader creates it from the clock, so the two sides'
// read forms hold two different instants: that read value is masked and the answers stay Same.
func TestStateCompareIgnoresTheDefaultedReadUpdatedAt(t *testing.T) {
	verdict := stateCompare(
		stateAnswerText("2026-05-05T05:05:05.000Z", "2026-01-01T00:00:00.000Z", "2026-06-06T06:06:06.000Z", true),
		stateAnswerText("2026-07-07T07:07:07.777Z", "2026-01-01T00:00:00.000Z", "2026-09-09T09:09:09.999Z", true),
	)
	if verdict.Kind != Same {
		t.Fatalf("two defaulted read updatedAt stamps compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
}

// The reader defaults updatedAt whenever it does not keep the file's value, which is not only when the
// key is absent: an unreadable file, broken JSON, an invalid phase and a non-text updatedAt all take
// the default state, whose updatedAt each side stamps from its own clock. readDefaultedUpdatedAt
// mirrors that rule, so a file that holds a persisted updatedAt but no valid phase still masks the
// read form and the two sides' wall-clock defaults compare Same.
func TestStateReadDefaultedUpdatedAtMirrorsTheReader(t *testing.T) {
	kept := "{\"phase\": \"P\", \"updatedAt\": \"2026-01-01T00:00:00.000Z\"}"
	if readDefaultedUpdatedAt([]byte(kept)) {
		t.Fatal("a readable state with a persisted updatedAt read as defaulted")
	}
	for _, raw := range []string{
		"{\"updatedAt\": \"2026-01-01T00:00:00.000Z\"}",
		"{\"phase\": \"NOPE\", \"updatedAt\": \"2026-01-01T00:00:00.000Z\"}",
		"{\"phase\": 5, \"updatedAt\": \"2026-01-01T00:00:00.000Z\"}",
		"{\"phase\": \"P\", \"updatedAt\": 5}",
		"{\"phase\": \"P\"}",
		"{",
		"",
	} {
		if !readDefaultedUpdatedAt([]byte(raw)) {
			t.Errorf("%q did not read as a defaulted updatedAt", raw)
		}
	}
}

// The predicate decodes with the reader's own decoder: pyjson accepts bare NaN, which encoding/json
// refuses, so a source holding one takes the default state in the reader and must read as defaulted
// here too (generation 3, c8; audit round 2 P2).
func TestStateReadDefaultedUpdatedAtUsesTheReaderDecoder(t *testing.T) {
	raw := "{\"phase\": \"P\", \"updatedAt\": NaN}"
	if !readDefaultedUpdatedAt([]byte(raw)) {
		t.Fatal("a source encoding/json refuses read as a kept updatedAt")
	}
}

// The mask splices only the top-level updatedAt's value. A document whose nested object carries an
// updatedAt before the top-level one must keep the nested value and every other byte (generation 3,
// c8; audit round 2 P2: the shim matched the first textual occurrence, which a nested-first document
// would have cut).
func TestMaskTopLevelTimestampLeavesANestedUpdatedAt(t *testing.T) {
	text := "{\n  \"finalGate\": {\n    \"updatedAt\": \"2020-01-01T00:00:00.000Z\"\n  },\n  \"updatedAt\": \"2026-01-01T00:00:00.000Z\",\n  \"objective\": \"o\"\n}"
	want := "{\n  \"finalGate\": {\n    \"updatedAt\": \"2020-01-01T00:00:00.000Z\"\n  },\n  \"updatedAt\": \"" + timestampPlaceholder + "\",\n  \"objective\": \"o\"\n}"
	if got := maskTopLevelTimestamp(text); got != want {
		t.Fatalf("the mask changed more than the top-level updatedAt:\n got %q\nwant %q", got, want)
	}
}

// A value that only looks like a stamp, a non-string updatedAt, and a document that is not an object
// are all left as stored.
func TestMaskTopLevelTimestampLeavesEverythingElse(t *testing.T) {
	for _, text := range []string{
		"{\"updatedAt\": \"not a stamp\"}",
		"{\"updatedAt\": 5}",
		"[\"updatedAt\"]",
		"{",
		"",
		"{\"other\": \"2026-01-01T00:00:00.000Z\"}",
	} {
		if got := maskTopLevelTimestamp(text); got != text {
			t.Errorf("maskTopLevelTimestamp(%q) = %q, want it unchanged", text, got)
		}
	}
}
