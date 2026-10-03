package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The tests are the B-class tests of CXC v0.2.40 pabcd-state/test/state.test.ts that cover the write path, named by the oracle
// test they port and run through the real EnsureState, WriteState and appenders (state_test.go ports the read side with
// hand-written files). The recorded oracle cases are replayed by oracle_writes_test.go and the process cases by
// concurrency_test.go.

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sessionFiles lists the names under .crw/sessions, nil when the directory is missing.
func sessionFiles(cwd string) (names []string) {
	entries, _ := os.ReadDir(filepath.Join(cwd, crwdir.DirName, SessionsSubdir))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func linkFails(errno syscall.Errno) func(string, string) error {
	return func(old, created string) error { return &os.LinkError{Op: "link", Old: old, New: created, Err: errno} }
}

func TestEnsureStateFreshSessionCreatesTheExactDefaultStateWithoutTempFiles(t *testing.T) { // oracle 137
	cwd := t.TempDir()
	if created, err := EnsureState(cwd, "session-start-fresh"); err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	file := fileText(t, StatePath(cwd, "session-start-fresh"))
	got, unreadable := ReadStateStrict(cwd, "session-start-fresh")
	want := DefaultState("session-start-fresh", "")
	if _, err := time.Parse(time.RFC3339Nano, got.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	want.UpdatedAt = got.UpdatedAt
	if unreadable || mustEncode(t, got) != file || file != mustEncode(t, want) || !slices.Equal(sessionFiles(cwd), []string{"session-start-fresh.json"}) {
		t.Fatalf("unreadable %v, files %v\n%s", unreadable, sessionFiles(cwd), file)
	}
}

func TestEnsureStateExistingValidStateIsResumeSafeAndUnchanged(t *testing.T) { // oracle 245
	cwd, resumed := t.TempDir(), DefaultState("session-start-valid", "resume-me")
	resumed.Phase, resumed.OrchestrationActive, resumed.StopBlockPhase, resumed.StopBlockCount = PhaseB, true, &resumed.Phase, 2
	if err := WriteState(cwd, resumed); err != nil {
		t.Fatal(err)
	}
	before := fileText(t, StatePath(cwd, "session-start-valid"))
	if created, err := EnsureState(cwd, "session-start-valid"); created || err != nil || fileText(t, StatePath(cwd, "session-start-valid")) != before {
		t.Fatalf("created %v, %v", created, err)
	}
}

func TestEnsureStateKeepsCorruptBytesAndRejectsNoncanonicalIDs(t *testing.T) { // oracle 266
	cwd, corrupt := t.TempDir(), "{ not valid json \x00"
	putIn(t, cwd, "session-start-corrupt", corrupt)
	if created, err := EnsureState(cwd, "session-start-corrupt"); created || err != nil || fileText(t, StatePath(cwd, "session-start-corrupt")) != corrupt {
		t.Fatalf("created %v, %v", created, err)
	}
	for _, id := range []string{"  padded  ", "../unsafe/session", "세션"} {
		if created, err := EnsureState(cwd, id); created || !errors.Is(err, ErrNonCanonicalSessionID) || err.Error() != "sessionId must be a canonical state key" {
			t.Errorf("%q: created %v, %v", id, created, err)
		}
	}
	if !slices.Equal(sessionFiles(cwd), []string{"session-start-corrupt.json"}) {
		t.Fatalf("files %v", sessionFiles(cwd))
	}
}

func TestEnsureStateWhenTheHardLinkFails(t *testing.T) { // oracle 651, 666, 680, 696
	for _, c := range []struct {
		name    string
		errno   syscall.Errno
		created bool
		err     error
		file    bool
	}{
		{"falls back when linkSync answers EPERM", syscall.EPERM, true, nil, true},
		{"EEXIST from linkSync returns false without touching the fallback", syscall.EEXIST, false, nil, false},
		{"a non-link error still propagates", syscall.EIO, false, syscall.EIO, false},
	} {
		cwd := t.TempDir()
		created, err := ensureState(cwd, "link-case", at(), linkFails(c.errno))
		if created != c.created || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) || len(sessionFiles(cwd)) != map[bool]int{true: 1}[c.file] {
			t.Errorf("%s: created %v, %v, files %v", c.name, created, err, sessionFiles(cwd))
		}
	}
	cwd := t.TempDir() // the fallback maps EEXIST to false: someone else won the race
	if created, err := EnsureState(cwd, "fat32-race"); !created || err != nil {
		t.Fatal(created, err)
	}
	if created, err := ensureState(cwd, "fat32-race", at(), linkFails(syscall.ENOTSUP)); created || err != nil {
		t.Fatalf("second: created %v, %v", created, err)
	}
	if _, err := ensureState(t.TempDir(), "x", at(), os.Link); err != nil || (func() bool {
		f := filepath.Join(t.TempDir(), "f")
		_ = os.WriteFile(f, nil, 0o644)
		_, e := ensureState(f, "x", at(), os.Link)
		return !errors.Is(e, syscall.ENOTDIR)
	})() {
		t.Fatal("a .crw that is a file must fail with ENOTDIR")
	}
}

func TestEnsureStateLeavesAnExistingStateDirectoryAndIgnoreFileAlone(t *testing.T) { // oracle 111
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, crwdir.DirName), 0o777); err != nil {
		t.Fatal(err)
	}
	ignore := filepath.Join(cwd, crwdir.DirName, ".gitignore")
	if _, err := EnsureState(cwd, "existing-empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ignore); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an existing state directory got an ignore file: %v", err)
	}
	if err := os.WriteFile(ignore, []byte("user rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureState(cwd, "existing-ignore"); err != nil || fileText(t, ignore) != "user rules\n" {
		t.Fatalf("%v", err)
	}
}

func TestWriteStateThenReadStateRoundTrips(t *testing.T) {
	marker := func(session, next string) *DcloseRecoveryMarker {
		m := &DcloseRecoveryMarker{SessionID: session, CheckEpoch: "c-1", ClosedWorkPhaseID: "wp-1"}
		if next != "" {
			m.NextWorkPhaseID = &next
		}
		return m
	}
	phaseB := PhaseB
	for _, c := range []struct {
		name  string // the oracle test
		set   func(*State)
		check func(State) bool
	}{
		{"write -> read roundtrip + flags merge (328): the interview flag is derived", func(s *State) { s.Phase, s.Flags.Interview = PhaseP, true },
			func(s State) bool { return s.Phase == PhaseP && !s.Flags.Interview && !s.Flags.AuditPassed }},
		{"stopBlockTurnId round trips (182)", func(s *State) { s.StopBlockTurnID = str("turn-new") }, func(s State) bool { return s.StopBlockTurnID != nil && *s.StopBlockTurnID == "turn-new" }},
		{"stopBlockCapNotified round-trips (194)", func(s *State) { s.StopBlockCapNotified = true }, func(s State) bool { return s.StopBlockCapNotified }},
		{"loopArmSeen/idleEditNudges valid values roundtrip (407)", func(s *State) { s.LoopArmSeen, s.IdleEditNudges = true, 7 }, func(s State) bool { return s.LoopArmSeen && s.IdleEditNudges == 7 }},
		{"injectedTurns roundtrips (454)", func(s *State) { s.InjectedTurns = []string{"t1", "t2"} }, func(s State) bool { return slices.Equal(s.InjectedTurns, []string{"t1", "t2"}) }},
		{"lastInjectedPhase + orchestrationActive roundtrip (489)", func(s *State) { s.Phase, s.LastInjectedPhase, s.OrchestrationActive = PhaseB, &phaseB, true },
			func(s State) bool {
				return s.LastInjectedPhase != nil && *s.LastInjectedPhase == PhaseB && s.OrchestrationActive
			}},
		{"IDLE phase forces orchestrationActive false (523)", func(s *State) { idle := PhaseIdle; s.LastInjectedPhase, s.OrchestrationActive = &idle, true },
			func(s State) bool {
				return s.Phase == PhaseIdle && s.LastInjectedPhase == nil && !s.OrchestrationActive
			}},
		{"a valid D-close marker restores with its IDLE check epoch (707)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker(s.SessionID, "wp-2") },
			func(s State) bool {
				return s.CheckEpoch != nil && *s.CheckEpoch == "c-1" && s.DcloseRecovery != nil && *s.DcloseRecovery.NextWorkPhaseID == "wp-2" && !s.DcloseRecovery.Legacy
			}},
		{"an explicit null successor restores without the legacy flag (786)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker(s.SessionID, "") },
			func(s State) bool {
				return s.DcloseRecovery != nil && s.DcloseRecovery.NextWorkPhaseID == nil && !s.DcloseRecovery.Legacy
			}},
		{"a foreign D-close marker is dropped and cannot retain an IDLE epoch (808)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker("other-session", "wp-2") },
			func(s State) bool { return s.DcloseRecovery == nil && s.CheckEpoch == nil }},
	} {
		cwd, s := t.TempDir(), DefaultState("rt", "")
		c.set(&s)
		if err := WriteState(cwd, s); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, unreadable := ReadStateStrict(cwd, "rt"); unreadable || !c.check(got) {
			t.Errorf("%s: %+v unreadable=%v", c.name, got, unreadable)
		}
		if names := sessionFiles(cwd); !slices.Equal(names, []string{"rt.json"}) { // writeState: no orphan .tmp (432)
			t.Errorf("%s: files %v", c.name, names)
		}
	}
	// the hand-edited halves of 182 and 194: a malformed value reads as the default
	cwd, s := t.TempDir(), DefaultState("edit", "")
	s.StopBlockTurnID, s.StopBlockCapNotified = str("turn-new"), true
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	edited := strings.NewReplacer("\"turn-new\"", "42", "\"stopBlockCapNotified\": true", "\"stopBlockCapNotified\": \"true\"").Replace(fileText(t, StatePath(cwd, "edit")))
	putIn(t, cwd, "edit", edited)
	if got := ReadState(cwd, "edit"); got.StopBlockTurnID != nil || got.StopBlockCapNotified {
		t.Fatalf("%+v", got)
	}
}

func TestSessionIsolationTwoSessionIDsInOneCwdDoNotClobber(t *testing.T) { // oracle 344
	cwd, alpha, beta := t.TempDir(), DefaultState("alpha", ""), DefaultState("beta", "")
	alpha.Phase, beta.Phase = PhaseB, PhaseP
	for _, s := range []State{alpha, beta} {
		if err := WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := ReadState(cwd, "alpha"), ReadState(cwd, "beta"); a.Phase != PhaseB || b.Phase != PhaseP {
		t.Fatalf("alpha %s beta %s", a.Phase, b.Phase)
	}
}
