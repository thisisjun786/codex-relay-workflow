package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// CRW-1570 D2: an answer that resumes the run checks the TMPDIR the run's spec records before it touches the run: a removed directory
// is refused (run_env_unusable) before the drain, the lock and the decision_applied row, so the same answer applies once the
// directory is made again. An answer with --no-resume launches nothing and keeps its behaviour; the later relaunch refuses.

// lastSeq is the seq of the last row of run-q.
func lastSeq(t *testing.T, w *world) int {
	t.Helper()
	rows := mustLoad(t, w)
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].Seq
}

func TestAnswerRefusesBeforeTheRowWhenTheRecordedTMPDIRIsGone(t *testing.T) {
	a := filepath.Join(t.TempDir(), "gone")
	w := newWorld(t)
	q := envAsking(t, w, map[string]string{"TMPDIR": a})
	td := w.deps()
	signalled := 0
	td.Signal = func(int, syscall.Signal) error { signalled++; return nil }
	id := q.answered(t, "ud-gone", nil)
	before := lastSeq(t, w)
	code, reply, errOut := q.answer(td, id)
	if code != 1 || reply.Applied || reply.Refusal == nil || reply.Refusal.Code != RefusedRunEnv || !strings.Contains(reply.Refusal.Detail, "recorded TMPDIR "+a) {
		t.Fatalf("the answer with the recorded TMPDIR gone: exit %d %+v %s", code, reply, errOut)
	}
	if after := lastSeq(t, w); after != before || len(q.appliedRows(t)) != 0 {
		t.Errorf("the refused answer appended rows (last seq %d -> %d, %d decision_applied)", before, after, len(q.appliedRows(t)))
	}
	if len(td.spawned) != 0 || signalled != 0 || reply.DrainAsked {
		t.Errorf("the refused answer started %d processes, sent %d signals, drain asked %v", len(td.spawned), signalled, reply.DrainAsked)
	}
	// the text answer says the same
	code, out, errOut := td.do("answer", "--state", w.state, "--decision", id, "--run", "run-q", "--node", "n1", "--question", q.q, "--revision", "1", "--thread", "thread-owner")
	if code != 1 || !strings.Contains(errOut, "refused ("+RefusedRunEnv+")") || !strings.Contains(errOut, a) || len(td.spawned) != 0 {
		t.Errorf("the text answer: exit %d %q %q", code, out, errOut)
	}
}

func TestAnswerAppliesOnceAfterTheRecordedTMPDIRIsBack(t *testing.T) {
	a := filepath.Join(t.TempDir(), "back")
	w := newWorld(t)
	q := envAsking(t, w, map[string]string{"TMPDIR": a})
	td := w.deps()
	id := q.answered(t, "ud-back", nil)
	if code, reply, errOut := q.answer(td, id); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedRunEnv {
		t.Fatalf("the answer with the recorded TMPDIR gone: exit %d %+v %s", code, reply, errOut)
	}
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	code, reply, errOut := q.answer(td, id)
	if code != 0 || !reply.Applied || reply.Replayed || reply.Refusal != nil || reply.Resumed == 0 {
		t.Fatalf("the same answer once the directory is back: exit %d %+v %s", code, reply, errOut)
	}
	if rows := q.appliedRows(t); len(rows) != 1 || rows[0].Seq != reply.Seq {
		t.Errorf("decision_applied rows %+v", rows)
	}
	if len(td.spawned) != 1 || !td.spawned[0].Resume || td.spawned[0].Spec.Env["TMPDIR"] != a {
		t.Errorf("the resume: %+v", td.spawned)
	}
}

func TestAnswerNoResumeKeepsItsBehaviour(t *testing.T) {
	a := filepath.Join(t.TempDir(), "gone")
	w := newWorld(t)
	q := envAsking(t, w, map[string]string{"TMPDIR": a})
	td := w.deps()
	id := q.answered(t, "ud-noresume", nil)
	code, reply, errOut := q.answer(td, id, "--no-resume")
	if code != 0 || !reply.Applied || reply.Refusal != nil || reply.Resumed != 0 {
		t.Fatalf("the answer with --no-resume: exit %d %+v %s", code, reply, errOut)
	}
	if rows := q.appliedRows(t); len(rows) != 1 || len(td.spawned) != 0 {
		t.Errorf("--no-resume: %d decision_applied rows, %d processes", len(rows), len(td.spawned))
	}
	before := lastSeq(t, w)
	code, out, errOut := td.do("resume", "run-q", "--detach", "--state", w.state)
	if code != 1 || !strings.Contains(errOut, "recorded TMPDIR "+a) || len(td.spawned) != 0 {
		t.Errorf("the later resume --detach: exit %d %q %q, %d processes", code, out, errOut, len(td.spawned))
	}
	if after := lastSeq(t, w); after != before {
		t.Errorf("the refused resume appended rows (%d -> %d)", before, after)
	}
}
