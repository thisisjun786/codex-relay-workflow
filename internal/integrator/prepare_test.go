package integrator

import (
	"errors"
	"strings"
	"testing"
)

// CRW-958: Prepare makes in place what the tool commands of a real run need on disk. It runs in a
// pass that starts a gate and never in DryRun, which promises that no directory is made.
func TestPreparerRunsInARealPassAndNeverInADryRun(t *testing.T) {
	w := newWorld(t)
	prepared := 0
	w.r.Prepare = func() error { prepared++; return nil }
	h := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	w.pend("1", h)
	if _, err := w.r.DryRun(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if prepared != 0 {
		t.Fatalf("a dry run (with the gate) called Prepare %d times", prepared)
	}
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if prepared == 0 {
		t.Fatal("a real pass never called Prepare")
	}
}

func TestPassStopsWhenPrepareFailsBeforeAnyToolStarts(t *testing.T) {
	w := newWorld(t)
	w.r.Prepare = func() error { return errors.New("build_work: no space") }
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	if !strings.Contains(rep.Error, "build_work: no space") || w.where("1") != "pending" {
		t.Fatalf("%+v, 1 is %s", rep, w.where("1"))
	}
	if got := w.ctlRead("gate-calls"); got != "" {
		t.Fatalf("the gate ran: %q", got)
	}
}
