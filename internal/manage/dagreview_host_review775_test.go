package manage

// Tests for the four P1 defects the post-merge evaluation of PR #775 (CRW-688) named in the
// dag-review host reading: a refusal the review already reported is not reported again, a rollout
// seen for the first time keeps the relay call that is still waiting for its answer, only a real
// relay dag- invocation is judged a relay call, and a terminal turn whose end cannot be measured is
// left unmeasured rather than silent. Every test uses temporary rollout and state files only.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// dagHostReview775Refusal is a rollout holding one tool call and the relay's refusal envelope.
func dagHostReview775Refusal(t *testing.T, callID, arguments string) []string {
	t.Helper()
	return []string{
		dagHostToolCall(t, callID, arguments),
		dagHostToolOutput(t, callID, `{"error":"refused","reason":"stale_coordinator_epoch"}`),
	}
}

// dagHostReview775SeedOffsets writes a state file that already knows path at offset, so the rollout
// is read as a known one from that byte rather than as first-seen.
func dagHostReview775SeedOffsets(t *testing.T, stateDir, path string, offset int64) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"offsets": map[string]int64{path: offset}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, dagHostStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// dagHostReview775Parent answers thread/read with one parent rollout and points a review at it.
func dagHostReview775Parent(t *testing.T, f *dagReviewFixture, rollout, stateDir string) *Config {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	return dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)
}

// dagHostReview775TurnPage answers thread/turns/list with the one turn the test spells, or with an
// empty page when turn is nil.
func dagHostReview775TurnPage(host *fakehost.Server, turn map[string]any) {
	data := []any{}
	if turn != nil {
		data = append(data, turn)
	}
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}})
}

// C1 red: a refusal whose call the review already reported is not reported again while an earlier
// relay call of the same rollout is still waiting for its answer. dev reports it on every check.
func TestDagHostReview775PendingCallDoesNotRepeatLaterRefusal(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	head := dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"})
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", append([]string{head},
		dagHostToolCall(t, "call-A", "crw relay dag-ready --plan p1"),
		dagHostToolCall(t, "call-B", "crw relay dag-release --plan p1"),
		dagHostToolOutput(t, "call-B", `{"ok": false, "reason": "stale_coordinator_epoch"}`),
	)...)
	f.close()
	// The saved offset sits at call-A's line, so the check resumes there.
	dagHostReview775SeedOffsets(t, stateDir, rollout, int64(len(head)+1))
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	first := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
	if len(first) != 1 || !strings.Contains(first[0].Detail, "stale_coordinator_epoch") {
		t.Fatalf("the first check reported %+v, want the one refusal of call-B", first)
	}
	second := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
	if len(second) != 0 {
		t.Fatalf("the second check reported a refusal it had already reported: %+v", second)
	}
}

// C2 red: a rollout seen for the first time remembers the earliest relay call whose output has not
// arrived, so the refusal that arrives after the first check is reported. dev starts at EOF and
// never sees it.
func TestDagHostReview775FirstSightKeepsPendingCall(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostToolCall(t, "call-A", "crw relay dag-ready --plan p1"))
	f.close()
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("a first-seen rollout with no output raised an anomaly: %+v", found)
	}
	dagHostAppendRollout(t, rollout, dagHostToolOutput(t, "call-A", `{"ok": false, "reason": "stale_coordinator_epoch"}`))

	found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
	if len(found) != 1 || !strings.Contains(found[0].Detail, "stale_coordinator_epoch") {
		t.Fatalf("the pending call's later refusal was not reported: %+v", found)
	}
}

// C3 red: reading a relay document is not a relay invocation. dev matches the words relay and dag-
// anywhere in the arguments, so the document read is reported.
func TestDagHostReview775DocsReadIsNotRelayCall(t *testing.T) {
	cases := []struct {
		name       string
		arguments  string
		wantRaised bool
	}{
		{"a relay document read", "cat docs/relay/dag-release-example.json", false},
		{"a variable-expansion program", "$RELAY --state S dag-release --plan p1", true},
		{"the relay program name", "codex-session-relay dag-ready --plan p1", true},
		{"a function_call cmd member", `{"cmd":"codex-session-relay dag-ready --plan p1"}`, true},
		{"a value-less option before the subcommand", "codex-session-relay --json dag-release --plan p1", true},
		{"an option with a separate value", "codex-session-relay --state S dag-ready --plan p1", true},
		{"a program whose argument holds the words", "rg -n 'relay dag-release' docs", false},
		{"an unrelated program after exec", "exec cat docs/relay/dag-plan.md", false},
		// The subcommand position follows the relay's own root parser: a dag- word given as an
		// option's value is that value, and a word after an unknown or value-less option is not
		// the subcommand either. A line the parser refuses is not a dag call.
		{"a dag- word taken as an option value", "codex-session-relay --state dag-release status", false},
		{"a dag- word after an unknown option", "codex-session-relay --json --relationship dag-release status", false},
		{"a dag- word after an unknown option, no flag first", "codex-session-relay --nope dag-release --plan p1", false},
		{"an option missing its value", "codex-session-relay --state", false},
		{"a variable program with a value-less option", "$RELAY --json dag-ready --plan p", true},
		{"a variable program with an inline option value", "$RELAY --state=$ST dag-release --plan p1", true},
		{"the relay program with an inline option value", "codex-session-relay --state=$ST dag-release --plan p1", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostReview775Refusal(t, "call-1", test.arguments)...)
			f.close()
			dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
			cfg := dagHostReview775Parent(t, f, rollout, stateDir)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
			if test.wantRaised {
				if len(found) != 1 {
					t.Fatalf("%q was not reported as a relay call: %+v", test.arguments, found)
				}
			} else if len(found) != 0 {
				t.Fatalf("%q was reported as a relay call: %+v", test.arguments, found)
			}
		})
	}
}

// C4 red: a terminal turn whose end cannot be measured leaves the child_turn check unmeasured.
func TestDagHostReview775InterruptedTurnWithoutEndIsUnmeasured(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
	f.close()
	host := fakehost.Start(t)
	dagHostReview775TurnPage(host, map[string]any{
		"id": "turn-1", "status": "interrupted", "startedAt": nil, "completedAt": nil, "durationMs": nil,
	})
	cfg := dagHostConfig(t, f, host.SocketPath, nil, "", 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	if found := dagReviewFind(review, dagHostKindChildTurnWithoutReceipt); len(found) != 0 {
		t.Fatalf("an unmeasurable turn raised an anomaly: %+v", found)
	}
	if !dagHostUnmeasured(review, "child_turn:child-thread-1") {
		t.Fatalf("a terminal turn without a measurable end was not unmeasured: %+v", review.Checks)
	}
}

// An empty thread and a turn still in progress stay silent: neither an anomaly nor an unmeasured
// check, which is what keeps a healthy reading quiet.
func TestDagHostReview775SilentTurnsStayQuiet(t *testing.T) {
	cases := []struct {
		name string
		turn map[string]any
	}{
		{"an empty thread", nil},
		{"a turn in progress", map[string]any{
			"id": "turn-1", "status": "inProgress", "startedAt": float64(dagReviewNow().Add(-2 * time.Hour).Unix()),
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
			f.close()
			host := fakehost.Start(t)
			dagHostReview775TurnPage(host, test.turn)
			cfg := dagHostConfig(t, f, host.SocketPath, nil, "", 0)

			review := dagHostRun(t, context.Background(), f, cfg)
			if found := dagReviewFind(review, dagHostKindChildTurnWithoutReceipt); len(found) != 0 {
				t.Fatalf("a silent reading raised an anomaly: %+v", found)
			}
			if dagHostUnmeasured(review, "child_turn:child-thread-1") {
				t.Fatalf("a silent reading was left unmeasured: %+v", review.Checks)
			}
		})
	}
}

// C1's migration claim: a state file that carries only offsets (no reported member) reads as an
// empty list, so the refusal after its offset is reported once, and the rewrite adds the member.
func TestDagHostReview775OldStateFileReadsEmptyReported(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostReview775Refusal(t, "call-1", "crw relay dag-release --plan p1")...)
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("an old state file without a reported member hid the refusal: %+v", found)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, dagHostStateFile))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("the state file is not a JSON object: %v, %s", err, data)
	}
	if _, ok := document["reported"]; !ok {
		t.Fatalf("the rewrite did not add the reported member: %s", data)
	}
}

// A relay call whose command follows another command on an earlier line is still a relay call:
// a newline starts a command unit, so the program word is the relay's, not the first line's.
func TestDagHostReview775NewlineSeparatedRelayCall(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview775Refusal(t, "call-1", "set -e\ncrw relay dag-release --plan p1")...)
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("a relay call after a newline was not reported: %+v", found)
	}
}

// The relay's own recovery command is the resolved executable followed by relay, so an absolute
// program word names the relay exactly as the bare name does.
func TestDagHostReview775AbsoluteRelayProgram(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
	}{
		{"an absolute crw followed by relay", "/opt/crw/bin/crw relay dag-release --plan p1"},
		{"an absolute codex-session-relay", "/opt/crw/bin/codex-session-relay dag-ready --plan p1"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostReview775Refusal(t, "call-1", test.arguments)...)
			f.close()
			dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
			cfg := dagHostReview775Parent(t, f, rollout, stateDir)

			if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
				t.Fatalf("%q was not reported as a relay call: %+v", test.arguments, found)
			}
		})
	}
}

// A rollout line over the limit is an unmeasured reading, not the end of the reading: a refusal
// written after it is still reported and the offset still advances.
func TestDagHostReview775OversizeLineDoesNotBlockLaterRefusals(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	lines := append([]string{dagHostToolOutput(t, "call-big", strings.Repeat("z", dagHostLineLimit+1024))},
		dagHostReview775Refusal(t, "call-1", "crw relay dag-release --plan p1")...)
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", lines...)
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	review := dagHostRun(t, context.Background(), f, cfg)
	if found := dagReviewFind(review, dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("the refusal after an oversize line was not reported: %+v", found)
	}
	if !dagHostUnmeasured(review, "parent_refusals:parent-1") {
		t.Fatalf("the oversize line was not left unmeasured: %+v", review.Checks)
	}
}

// On a rollout seen for the first time only the refusals whose answer was written after the reading
// began are reported: a refusal already in the file is history even when its call line is inside the
// reading, and the answer of a call that already existed is new when it arrives during the scan. The
// call's own position decides nothing here; the resume list still bounds itself by the call line.
func TestDagHostReview775FirstSightReportsOnlyRefusalsWrittenAfterTheReading(t *testing.T) {
	reading := dagHostRolloutReading{
		boundary: 100,
		resume:   250,
		refusals: []dagHostRefusal{
			{callID: "history", callStart: 10, outputStart: 20, commands: []string{"dag-release"}, reason: "old"},
			{callID: "appended", callStart: 10, outputStart: 120, commands: []string{"dag-ready"}, reason: "new"},
		},
	}

	first := dagHostNewRefusals(reading, map[string]bool{}, true)
	if len(first) != 1 || first[0].callID != "appended" {
		t.Fatalf("a first-sight reading reported %+v, want only the refusal answered after it began", first)
	}
	known := dagHostNewRefusals(reading, map[string]bool{}, false)
	if len(known) != 2 {
		t.Fatalf("a known rollout reported %+v, want both refusals", known)
	}
	suppressed := dagHostNewRefusals(reading, map[string]bool{"appended": true}, false)
	if len(suppressed) != 1 || suppressed[0].callID != "history" {
		t.Fatalf("an already reported refusal was reported again: %+v", suppressed)
	}
}

// d1 (C2): a rollout seen for the first time decides history by where a refusal's ANSWER starts, not
// by where its call starts. The seam appends the answer of a call that already existed after the
// reading has taken the file's size as its boundary and before it scans, so the real reading is
// driven: the answer is new and is reported once, and the next check does not report it again. The
// control's call and answer both precede the reading, so neither is reported.
func TestDagHostReview775FirstSightReportsTheAnswerAppendedDuringTheScan(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		append     []string
		wantRaised bool
	}{
		{
			name:       "the answer of a call that already existed arrives during the scan",
			lines:      []string{dagHostToolCall(t, "call-A", "crw relay dag-release --plan p1")},
			append:     []string{dagHostToolOutput(t, "call-A", `{"error":"refused","reason":"stale_coordinator_epoch"}`)},
			wantRaised: true,
		},
		{
			name: "a call and its answer both precede the scan",
			lines: []string{
				dagHostToolCall(t, "call-A", "crw relay dag-release --plan p1"),
				dagHostToolOutput(t, "call-A", `{"error":"refused","reason":"stale_coordinator_epoch"}`),
			},
			append:     []string{dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"})},
			wantRaised: false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", test.lines...)
			f.close()
			cfg := dagHostReview775Parent(t, f, rollout, stateDir)

			appended := false
			dagHostAfterBoundary = func(path string) {
				if path != rollout || appended {
					return
				}
				appended = true
				dagHostAppendRollout(t, rollout, test.append...)
			}
			t.Cleanup(func() { dagHostAfterBoundary = nil })

			first := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
			if !test.wantRaised {
				if len(first) != 0 {
					t.Fatalf("a refusal that preceded the reading was reported: %+v", first)
				}
				return
			}
			if len(first) != 1 || !strings.Contains(first[0].Detail, "stale_coordinator_epoch") {
				t.Fatalf("the answer appended during the scan was not reported once: %+v", first)
			}
			second := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
			if len(second) != 0 {
				t.Fatalf("the next check reported the same answer again: %+v", second)
			}
		})
	}
}
