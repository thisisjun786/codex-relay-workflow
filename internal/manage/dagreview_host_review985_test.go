package manage

// CRW-985 tests: the dag-review reading judges a child's relay calls with the mvdan.cc/sh/v3 syntax
// parser. Each case is one shell command line in a parent rollout that the relay refused; the
// reading reports the refusal only when the parser finds a real relay dag- invocation in it. Every
// test uses temporary rollout and state files only.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// dagHostReview985Call is a function_call whose arguments carry the command line as a JSON cmd member,
// the shape a real exec_command call has in a rollout.
func dagHostReview985Call(t *testing.T, callID, cmd string) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]string{"cmd": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return dagHostToolCall(t, callID, string(arguments))
}

// dagHostReview985Refused is the relay's refusal envelope as the output of call callID.
func dagHostReview985Refused(t *testing.T, callID string) string {
	t.Helper()
	return dagHostToolOutput(t, callID, `{"error":"refused","reason":"unregistered_scope"}`)
}

// dagHostReview985Unparsed counts the unmeasured checks that name a command the parser refused.
func dagHostReview985Unparsed(review Review) int {
	count := 0
	for _, check := range review.Checks {
		if check.State == dagReviewUnmeasured && strings.Contains(check.Detail, "command_unparsed") {
			count++
		}
	}
	return count
}

// TestDagHostReview985CommandLineJudgedByGrammar runs one refused call per case and checks whether a
// refusal is reported: the relay call is judged by the parser, so a call inside a substitution, a
// pipeline, a list or a compound command counts, and a document read, a comment or a quoted string
// does not.
func TestDagHostReview985CommandLineJudgedByGrammar(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"a multi-line substitution in an expanded here-document",
			"cat <<EOF\n$(\n  crw relay --state S dag-ready --plan missing-plan\n)\nEOF", true},
		{"a quoted delimiter with a dollar sign keeps its body as data",
			"cat <<'END$EXAMPLE'\ncrw relay --state S dag-ready --plan p\nEND$EXAMPLE", false},
		{"a relay call after a pipe", "echo go | crw relay --state S dag-ready --plan p", true},
		{"a relay call after and-and", "true && crw relay --state S dag-ready --plan p", true},
		{"a relay call in an if body", "if true; then crw relay --state S dag-ready --plan p; fi", true},
		{"a relay call in a for body", "for i in 1; do crw relay --state S dag-ready --plan p; done", true},
		{"a relay call through a variable program", "$RELAY --state S dag-ready --plan p", true},
		{"a dag- word given as an option value", "crw relay --state dag-ready status", false},
		{"a relay document read", "cat docs/relay.md | rg dag-ready", false},
		{"a relay call inside a comment", "# crw relay dag-ready --plan p", false},
		{"a relay call inside a double-quoted string", "echo \"crw relay dag-ready --plan p\"", false},
		{"an ANSI-C quoted program name", `$'codex-session-relay' --state S dag-ready --plan p`, true},
		{"an ANSI-C quoted program name with an escape", `$'codex\x2dsession-relay' --state S dag-ready --plan p`, true},
		{"a substituted program name", `$(echo crw) relay --state S dag-ready --plan p`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			stateDir := filepath.Join(t.TempDir(), "state")
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
				dagHostReview985Call(t, "call-1", test.cmd), dagHostReview985Refused(t, "call-1"))
			f.close()
			dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
			cfg := dagHostReview775Parent(t, f, rollout, stateDir)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
			if got := len(found) == 1; got != test.want {
				t.Fatalf("refusal reported = %v (%+v), want %v for %q", got, found, test.want, test.cmd)
			}
		})
	}
}

// TestDagHostReview985UnparsedCommandIsUnmeasuredOnce: a command line the parser refuses is not
// guessed at. It is left unmeasured once, with the reason command_unparsed, and a later check does not
// repeat it.
func TestDagHostReview985UnparsedCommandIsUnmeasuredOnce(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview985Call(t, "call-1", `crw relay --state S dag-release --plan "unclosed`),
		dagHostReview985Refused(t, "call-1"))
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	first := dagHostRun(t, context.Background(), f, cfg)
	if got := dagHostReview985Unparsed(first); got != 1 {
		t.Fatalf("command_unparsed checks = %d, want 1: %+v", got, first.Checks)
	}
	if found := dagReviewFind(first, dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("an unparsed command was judged a relay call: %+v", found)
	}
	if again := dagHostReview985Unparsed(dagHostRun(t, context.Background(), f, cfg)); again != 0 {
		t.Fatalf("the unparsed command was reported again: %d", again)
	}
}

// TestDagHostReview985UnparsedLineBehindPendingCallIsNotRepeated: a call still waiting for its answer
// makes the next check read the lines after it again, so a refused command line behind it is read a
// second time. It is reported once, and the refusal that arrives for the pending call is reported
// when it comes.
func TestDagHostReview985UnparsedLineBehindPendingCallIsNotRepeated(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview985Call(t, "call-A", "crw relay --state S dag-ready --plan p"),
		dagHostReview985Call(t, "call-B", `crw relay --state S dag-release --plan "unclosed`),
		dagHostReview985Refused(t, "call-B"))
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	if got := dagHostReview985Unparsed(dagHostRun(t, context.Background(), f, cfg)); got != 1 {
		t.Fatalf("the first check reported %d unparsed lines, want 1", got)
	}
	dagHostAppendRollout(t, rollout, dagHostReview985Refused(t, "call-A"))
	second := dagHostRun(t, context.Background(), f, cfg)
	if got := dagHostReview985Unparsed(second); got != 0 {
		t.Fatalf("the second check repeated the unparsed line: %d", got)
	}
	if found := dagReviewFind(second, dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("the refusal of the pending call = %+v, want one", found)
	}
}

// TestDagHostReview985FirstSightReportsUnparsedHistory: a refused command line is unmeasured wherever it
// stands, so a rollout seen for the first time reports the one in its history, once.
func TestDagHostReview985FirstSightReportsUnparsedHistory(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview985Call(t, "call-1", "crw relay --state S dag-release --plan \"unclosed"),
		dagHostReview985Refused(t, "call-1"))
	f.close()
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	first := dagHostRun(t, context.Background(), f, cfg)
	if got := dagHostReview985Unparsed(first); got != 1 {
		t.Fatalf("command_unparsed checks = %d, want 1: %+v", got, first.Checks)
	}
	if again := dagHostReview985Unparsed(dagHostRun(t, context.Background(), f, cfg)); again != 0 {
		t.Fatalf("the history line was reported again: %d", again)
	}
}

// TestDagHostReview985FirstSightKeepsAnsiCPendingCall: a call whose program word is ANSI-C quoted and
// static is a relay call, so a rollout seen for the first time keeps it pending until its refusal
// arrives.
func TestDagHostReview985FirstSightKeepsAnsiCPendingCall(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview985Call(t, "call-1", "$'codex-session-relay' --state S dag-ready --plan p"))
	f.close()
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("the first check reported %+v before the answer arrived", found)
	}
	dagHostAppendRollout(t, rollout, dagHostReview985Refused(t, "call-1"))
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("the refusal of the ANSI-C call = %+v, want one", found)
	}
}

// TestDagHostReview985UnparsedLineAndRefusalKeepSeparateReports: a refusal and a refused command line
// are two reports even when a call id spells the other's prefix. Each is reported once, and neither is
// reported again by the next check.
func TestDagHostReview985UnparsedLineAndRefusalKeepSeparateReports(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostReview985Call(t, "unparsed:call-1", "crw relay --state S dag-release --plan p"),
		dagHostReview985Refused(t, "unparsed:call-1"),
		dagHostReview985Call(t, "call-1", "crw relay --state S dag-release --plan \"unclosed"),
		dagHostReview985Refused(t, "call-1"))
	f.close()
	dagHostReview775SeedOffsets(t, stateDir, rollout, 0)
	cfg := dagHostReview775Parent(t, f, rollout, stateDir)

	first := dagHostRun(t, context.Background(), f, cfg)
	if got := len(dagReviewFind(first, dagHostKindParentDagRefusals)); got != 1 {
		t.Fatalf("refusals = %d, want 1", got)
	}
	if got := dagHostReview985Unparsed(first); got != 1 {
		t.Fatalf("command_unparsed checks = %d, want 1", got)
	}
	second := dagHostRun(t, context.Background(), f, cfg)
	if got := len(dagReviewFind(second, dagHostKindParentDagRefusals)) + dagHostReview985Unparsed(second); got != 0 {
		t.Fatalf("the second check repeated %d reports", got)
	}
}
