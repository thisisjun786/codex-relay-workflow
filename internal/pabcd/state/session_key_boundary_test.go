package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-1108 (known-defects.md:78 and :109): the public state, lock and interview-scan boundaries take a
// canonical session id only. The oracle's writeState and withSessionLock sanitise, so a/b and a-b
// shared one file and one lock and the later write replaced the earlier; an empty id became "missing".
// ensureState already refused such an id; the writers and the lock now refuse it the same way, before
// anything is created.
func TestStateBoundariesRefuseNonCanonicalSessionIDs(t *testing.T) {
	for _, id := range []string{"a/b", "", "x/", " a-b", "missing/"} {
		cwd := t.TempDir()
		s := DefaultState(id, "")
		if err := WriteState(cwd, s); !errors.Is(err, ErrNonCanonicalSessionID) {
			t.Fatalf("WriteState(%q) = %v; want ErrNonCanonicalSessionID", id, err)
		}
		ran := false
		if err := WithSessionLock(cwd, id, func() error { ran = true; return nil }); !errors.Is(err, ErrNonCanonicalSessionID) || ran {
			t.Fatalf("WithSessionLock(%q) = %v ran=%v; want ErrNonCanonicalSessionID and fn not run", id, err, ran)
		}
		if err := AppendInterviewEvent(cwd, InterviewEvent{TS: "t", SessionID: id, Event: ScanStarted, RoundID: 1}); !errors.Is(err, ErrNonCanonicalSessionID) {
			t.Fatalf("AppendInterviewEvent(%q) = %v; want ErrNonCanonicalSessionID", id, err)
		}
		if err := AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: id, To: PhaseP, Reason: "x"}); !errors.Is(err, ErrNonCanonicalSessionID) {
			t.Fatalf("AppendLedger(%q) = %v; want ErrNonCanonicalSessionID", id, err)
		}
		if _, err := os.Lstat(filepath.Join(cwd, crwdir.DirName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%q: a refused id created %s (%v)", id, crwdir.DirName, err)
		}
	}
	// a-b is canonical and keeps its own file; a/b can no longer alias it
	cwd := t.TempDir()
	s := DefaultState("a-b", "")
	s.Phase = PhaseB
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	alias := DefaultState("a/b", "")
	alias.Phase = PhaseP
	_ = WriteState(cwd, alias)
	if got := ReadState(cwd, "a-b").Phase; got != PhaseB {
		t.Fatalf("a/b replaced a-b: phase %s", got)
	}
}

// The scan reader counts a row as this session's evidence only when it names this session and carries
// every field InterviewEvent declares; a non-canonical id reads nothing, so a/b never reads a-b's file.
func TestReadInterviewEventsCountsOnlyThisSessionsCompleteRows(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, crwdir.DirName, InterviewsSubdir)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	rows := `{"ts":"t1","sessionId":"iv","event":"scan_completed","roundId":1,"contradictionCount":0,"highContradictionCount":0}
{"ts":"t2","sessionId":"other","event":"scan_completed","roundId":2,"contradictionCount":0,"highContradictionCount":0}
{"sessionId":"iv","event":"scan_completed","roundId":3,"contradictionCount":0,"highContradictionCount":0}
{"ts":"t4","event":"scan_completed","roundId":4,"contradictionCount":0,"highContradictionCount":0}
{"ts":"t5","sessionId":"iv","event":"scan_completed","roundId":5,"contradictionCount":0}
{"ts":"t6","sessionId":"iv","event":"rescan_completed","roundId":6,"contradictionCount":1,"highContradictionCount":"1"}
`
	for _, name := range []string{"iv.jsonl", "a-b.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(rows), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ReadInterviewEvents(cwd, "iv")
	if len(got) != 1 || got[0].RoundID != 1 || got[0].SessionID != "iv" {
		t.Fatalf("counted %+v; want only round 1", got)
	}
	for _, id := range []string{"a/b", ""} {
		if got := ReadInterviewEvents(cwd, id); got == nil || len(got) != 0 {
			t.Fatalf("ReadInterviewEvents(%q) = %+v; want an empty, non-nil list", id, got)
		}
	}
}

// CRW-1108 read side: a/b and "" sanitise to a-b and missing, so reading them used to return those sessions' state,
// grants included. ReadStateFile refuses a non-canonical id before any file access, and ReadStateStrict marks it
// unreadable (a default, never the alias's state); canonical ids still read.
func TestStateReadsRefuseNonCanonicalSessionIDs(t *testing.T) {
	cwd := t.TempDir()
	for _, id := range []string{"a-b", "missing"} {
		s := DefaultState(id, "")
		s.Phase = PhaseB
		s.MemoryWriteGrant = true
		if err := WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a/b", "", "missing/", " a-b"} {
		raw, err := ReadStateFile(cwd, id)
		s, unreadable := ReadStateStrict(cwd, id)
		if !errors.Is(err, ErrNonCanonicalSessionID) || raw != nil || !unreadable || s.Phase == PhaseB || s.MemoryWriteGrant {
			t.Errorf("id=%q ReadStateFile err=%v raw=%d; ReadStateStrict phase=%s grant=%v unreadable=%v; a non-canonical read must not expose an alias's state", id, err, len(raw), s.Phase, s.MemoryWriteGrant, unreadable)
		}
	}
	for _, id := range []string{"a-b", "missing"} {
		if s, unreadable := ReadStateStrict(cwd, id); unreadable || s.Phase != PhaseB || !s.MemoryWriteGrant {
			t.Errorf("canonical %q read changed: phase=%s grant=%v unreadable=%v", id, s.Phase, s.MemoryWriteGrant, unreadable)
		}
	}
}
