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
