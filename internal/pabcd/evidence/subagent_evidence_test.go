package evidence

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Ports of the tests of CXC v0.2.40 pabcd-state/test/subagent-evidence.test.ts that exercise the units of lines 326-470. The
// oracle drives them through the SubagentStop gate, the PreToolUse goal gate and the evidence CLI, which other issues port, so
// each test here makes the calls those entry points make: the gate's order (gate in corpus_test.go, whose resolution of a late
// receipt is ClearAttempts and ResolveTombstone), the goal gate's probes (UnrecordableVerdictStatus and HasSpentBudget) and the
// CLI's resolution (ClearAttempts, and a filter of its own over the tombstones, which the tests make with ResolveTombstone).

func agent(id, turn string) Payload { return Payload{AgentType: "executor", AgentID: id, TurnID: turn} }

// spend writes the counter an agent leaves behind after its blocks.
func spend(t *testing.T, cwd, sessionID, agentID, turnID string) {
	t.Helper()
	for i := 1; i <= MaxAttempts; i++ {
		if !WriteAttempts(cwd, sessionID, agentID, i, turnID) {
			t.Fatal("the counter was not written")
		}
	}
}

func tombstones(cwd, sessionID string) (turns []string) {
	for _, e := range state.ReadState(cwd, sessionID).UnverifiedSubagents {
		turns = append(turns, e.AgentID+"/"+e.TurnID)
	}
	return turns
}

// :489, :504 and :551: a verdict that could not be recorded, an unreadable marker directory and an unwritable one each deny.
func TestUnrecordableMarkerDeniesCompletion(t *testing.T) {
	t.Run("a marker", func(t *testing.T) {
		cwd := t.TempDir()
		must(t, WriteUnrecordableMarker(cwd, "s1", "a1"))
		if got := UnrecordableVerdictStatus(cwd, "s1"); got != (VerdictStatus{Present: true}) {
			t.Errorf("%+v", got)
		}
		if got := UnrecordableVerdictStatus(cwd, "s2"); got != (VerdictStatus{}) {
			t.Errorf("another session sees it: %+v", got)
		}
	})
	t.Run("an unreadable marker directory", func(t *testing.T) {
		cwd := t.TempDir()
		put(t, filepath.Join(cwd, ".crw", UnrecordableSubdir), []byte("not a directory"))
		if got := UnrecordableVerdictStatus(cwd, "s1"); got != (VerdictStatus{Unreadable: true}) {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a readable but unwritable marker directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permissions are not enforced")
		}
		cwd := t.TempDir()
		dir := filepath.Join(cwd, ".crw", UnrecordableSubdir)
		must(t, os.MkdirAll(dir, 0o777))
		must(t, os.Chmod(dir, 0o500))
		defer func() { must(t, os.Chmod(dir, 0o700)) }()
		if got := UnrecordableVerdictStatus(cwd, "s1"); got != (VerdictStatus{Unreadable: true}) {
			t.Errorf("%+v", got)
		}
	})
}

// :586: a tombstone is never committed over unreadable state; the verdict goes to the marker, which the gate passes in as the
// writer of RecordTombstone.
func TestUnreadableStateGetsAMarkerNotATombstone(t *testing.T) {
	cwd := t.TempDir()
	spend(t, cwd, "s1", "a1", "")
	path := filepath.Join(cwd, ".crw", "sessions", "s1.json")
	put(t, path, []byte("{ corrupt"))
	var writer MarkerWriter = WriteUnrecordableMarker
	if RecordTombstone(cwd, "s1", agent("a1", ""), MaxAttempts, writer) {
		t.Error("a verdict was reported recorded over an unreadable file")
	}
	if raw, _ := os.ReadFile(path); !bytes.Equal(raw, []byte("{ corrupt")) {
		t.Errorf("the unreadable file was rewritten: %q", raw)
	}
	if got := UnrecordableVerdictStatus(cwd, "s1"); !got.Present {
		t.Errorf("no marker: %+v", got)
	}
}

// :616: the spent counter outlives every other record of a verdict.
func TestSpentBudgetOutlivesTheOtherRecords(t *testing.T) {
	cwd := t.TempDir()
	for i := 0; i <= MaxAttempts; i++ {
		gate(cwd, "s1", agent("a1", ""))
	}
	if !HasTombstone(cwd, "s1", agent("a1", "")) || !HasSpentBudget(cwd, "s1") {
		t.Fatal("the gate left no verdict")
	}
	must(t, os.RemoveAll(filepath.Join(cwd, ".crw", "sessions")))
	must(t, os.RemoveAll(filepath.Join(cwd, ".crw", UnrecordableSubdir)))
	if !HasSpentBudget(cwd, "s1") {
		t.Error("the budget did not outlive the loss of the other records")
	}
}

// :283 and :636: a late valid receipt resolves the tombstone and clears the spent counter, so the parent is not left blocked.
func TestAReceiptResolvesTheVerdict(t *testing.T) {
	cwd := t.TempDir()
	for i := 0; i <= MaxAttempts; i++ {
		gate(cwd, "s1", agent("a1", ""))
	}
	if got := tombstones(cwd, "s1"); len(got) != 1 {
		t.Fatalf("tombstones: %v", got)
	}
	ClearAttempts(cwd, "s1", "a1", "")
	if !ResolveTombstone(cwd, "s1", agent("a1", "")) {
		t.Error("the tombstone was not resolved")
	}
	if got := tombstones(cwd, "s1"); len(got) != 0 || HasTombstone(cwd, "s1", agent("a1", "")) || HasSpentBudget(cwd, "s1") {
		t.Errorf("the verdict stands: tombstones %v, spent %v", got, HasSpentBudget(cwd, "s1"))
	}
}

// :519, :664 and :718: resolving one turn leaves the verdicts and the counters of the agent's other turns, the turn-less one
// included.
func TestResolvingOneTurnLeavesTheOthers(t *testing.T) {
	cwd := t.TempDir()
	for _, turn := range []string{"t1", "t2", ""} {
		for i := 0; i <= MaxAttempts; i++ {
			gate(cwd, "s1", agent("a1", turn))
		}
	}
	ClearAttempts(cwd, "s1", "a1", "t1")
	if !ResolveTombstone(cwd, "s1", agent("a1", "t1")) || ResolveTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("the turn was not resolved exactly once")
	}
	if got := tombstones(cwd, "s1"); len(got) != 2 || got[0] != "a1/t2" || got[1] != "a1/" {
		t.Errorf("tombstones left: %v", got)
	}
	must(t, os.RemoveAll(filepath.Join(cwd, ".crw", "sessions"))) // only the counters speak now
	if !HasSpentBudget(cwd, "s1") {
		t.Error("the counters of the other turns did not survive")
	}
	ClearAttempts(cwd, "s1", "a1", "t2")
	if !HasSpentBudget(cwd, "s1") {
		t.Error("the turn-less counter did not survive the resolution of another turn")
	}
	ClearAttempts(cwd, "s1", "a1", "")
	if HasSpentBudget(cwd, "s1") {
		t.Error("the budget is spent with every counter cleared")
	}
}

// :237 and :700: each block names its attempt of the budget.
func TestVerifierDirectiveNamesTheAttempt(t *testing.T) {
	for i := 1; i <= MaxAttempts; i++ {
		if got := VerifierDirective(i); !strings.Contains(got, fmt.Sprintf("This is attempt %d of %d.", i, MaxAttempts)) {
			t.Errorf("attempt %d: %q", i, got)
		}
	}
}

// A verdict that is found but cannot be saved is not resolved: the call reports false and the file stays as it was. The lock is
// replaced by one that does not need the directory, which is what the permission takes away.
func TestResolveTombstoneReportsAFailedSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions are not enforced")
	}
	cwd, p := t.TempDir(), agent("a1", "t1")
	if !RecordTombstone(cwd, "s1", p, MaxAttempts, nil) {
		t.Fatal("no tombstone")
	}
	sessions := filepath.Join(cwd, ".crw", "sessions")
	before, err := os.ReadFile(state.StatePath(cwd, "s1"))
	must(t, err)
	must(t, os.Chmod(sessions, 0o500))
	defer func() { must(t, os.Chmod(sessions, 0o700)) }()
	if resolveTombstone(cwd, "s1", p, func(_, _ string, fn func() error) error { return fn() }) {
		t.Error("a verdict that could not be saved was reported resolved")
	}
	if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); !bytes.Equal(after, before) {
		t.Errorf("the file changed: %q", after)
	}
}

// A link at the marker directory is refused whole: the write fails, the status denies, and nothing appears where it leads.
func TestMarkerDirectoryLinkIsRefused(t *testing.T) {
	cwd, out := caseDirs(t)
	must(t, os.MkdirAll(filepath.Join(cwd, ".crw"), 0o777))
	must(t, os.Symlink(out, filepath.Join(cwd, ".crw", UnrecordableSubdir)))
	if WriteUnrecordableMarker(cwd, "s1", "a1") == nil {
		t.Error("a marker was written through a link")
	}
	if got := UnrecordableVerdictStatus(cwd, "s1"); got != (VerdictStatus{Unreadable: true}) {
		t.Errorf("%+v", got)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("files appeared outside the workspace: %v", entries)
	}
}

// A resolve without an agent id clears nothing: the tombstones of agents without ids stay separate verdicts.
func TestIdlessResolveClearsNothing(t *testing.T) {
	cwd := t.TempDir()
	for _, turn := range []string{"t1", "t2"} {
		if !RecordTombstone(cwd, "s1", agent("", turn), MaxAttempts, nil) {
			t.Fatal("no tombstone")
		}
	}
	if ResolveTombstone(cwd, "s1", agent("", "t1")) {
		t.Error("an identity-less resolve reported success")
	}
	if got := tombstones(cwd, "s1"); len(got) != 2 {
		t.Errorf("tombstones left: %v", got)
	}
	// The refusal comes before the lock, which would create the sessions directory and a lock file in a workspace that has none.
	empty, locked := t.TempDir(), false
	if resolveTombstone(empty, "s1", agent("", "t1"), func(_, _ string, fn func() error) error { locked = true; return fn() }) || locked {
		t.Errorf("an identity-less resolve took the lock (%v) or succeeded", locked)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("an identity-less resolve left %v in an empty workspace", entries)
	}
}
