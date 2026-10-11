package integrator

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// snapshot is every path below the roots with its size and mode, plus the refs of the repositories.
func (w *world) snapshot() string {
	var lines []string
	for _, root := range []string{w.cfg.QueueDir, w.cfg.RunsDir, w.cfg.WorkRoot, w.cfg.LockFile, w.ctl} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, _ := d.Info()
			lines = append(lines, p+" "+info.Mode().String()+" "+info.ModTime().String()+" "+string(rune(info.Size())))
			return nil
		})
	}
	lines = append(lines, w.git(w.checkout, "for-each-ref"), w.git(w.remote, "for-each-ref"), w.git(w.checkout, "worktree", "list"))
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestDryRunReportsTheQueueAndChangesNothing(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	merged := w.head("m", map[string]string{"m.txt": "m\n"}, true)
	w.git(w.other, "push", "-q", "origin", "m:refs/heads/dev")
	w.git(w.checkout, "fetch", "-q", "origin")
	lost := w.head("lost", map[string]string{"l.txt": "l\n"}, false)
	w.pend("3", a)
	w.pend("1", merged)
	w.pend("2", lost)
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "bad"), "x")
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "4"), "zz\n")
	w.write(filepath.Join(w.cfg.QueueDir, "retried", "3"), a+"\n")
	w.write(w.cfg.LockFile, "")
	before := w.snapshot()
	callsBefore := len(w.argvCalls())

	rep, err := w.r.DryRun(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if after := w.snapshot(); after != before {
		t.Fatalf("the dry run changed something:\n--- before\n%s\n--- after\n%s", before, after)
	}
	for _, c := range w.argvCalls()[callsBefore:] {
		for _, banned := range []string{" fetch ", " merge ", " push ", " worktree ", " commit ", " add ", "gate.sh", "verify.sh", "record.sh"} {
			if strings.Contains(c, banned) {
				t.Fatalf("the dry run ran %q", c)
			}
		}
	}
	if rep.Lock != "free" || rep.Stop || rep.Base != w.remoteDev() || len(rep.Entries) != 4 || strings.Join(rep.Ignored, ",") != "bad" {
		t.Fatalf("%+v", rep)
	}
	byName := map[string]DryEntry{}
	for _, e := range rep.Entries {
		byName[e.Name] = e
	}
	if !byName["1"].AlreadyInDev || !byName["1"].InCheckout || byName["2"].InCheckout || byName["3"].AlreadyInDev || !byName["3"].InCheckout || !byName["3"].Retried || byName["4"].HeadOK {
		t.Fatalf("%+v", byName)
	}
	if got := strings.Join(rep.Plan, " "); got != "crw-1="+merged+" crw-2="+lost+" crw-3="+a {
		t.Fatalf("plan %q", got)
	}
}

func TestDryRunWithGateRunsOnlyTheGate(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	w.ctlWrite("block-CRW-2", "")
	rep, err := w.r.DryRun(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Plan, " ") != "crw-1="+a || !strings.HasPrefix(rep.Entries[1].Gate, "BLOCK") || w.where("2") != "pending" {
		t.Fatalf("%+v", rep)
	}
}

func TestDryRunSeesALockAnotherRunHoldsAndAStopFile(t *testing.T) {
	w := newWorld(t)
	if rep, _ := w.r.DryRun(t.Context(), false); rep.Lock != "absent" {
		t.Fatalf("lock %q", rep.Lock)
	}
	if _, err := os.Stat(w.cfg.LockFile); err == nil {
		t.Fatal("the dry run created the lock file")
	}
	unlock, err := acquireLock(w.cfg.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	w.write(filepath.Join(w.cfg.QueueDir, "stop"), "")
	rep, _ := w.r.DryRun(t.Context(), false)
	if rep.Lock != "held" || !rep.Stop {
		t.Fatalf("%+v", rep)
	}
}

func TestDryRunSoloModePlansOneHead(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	w.r.setSoloOwed([]string{"crw-1=" + a, "crw-2=" + b})
	rep, err := w.r.DryRun(t.Context(), false)
	if err != nil || len(rep.Plan) != 1 || !strings.HasPrefix(rep.Plan[0], "crw-1=") || len(rep.SoloHeads) != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
}
