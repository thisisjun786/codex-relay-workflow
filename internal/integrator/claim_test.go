package integrator

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// noSpaceLink is a link that fails the way a full or read-only disk fails it.
func noSpaceLink(oldname, newname string) error {
	return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOSPC}
}

// claimHeads is the head each claim left in the queue holds.
func (w *world) claimHeads() []string {
	w.t.Helper()
	var heads []string
	for _, c := range w.r.listClaims() {
		h, err := readHead(c.Path)
		if err != nil {
			w.t.Fatalf("claim %s: %v", c.Path, err)
		}
		heads = append(heads, h)
	}
	return heads
}

// regularHead is the head a regular queue file holds ("" when there is none).
func regularHead(path string) string {
	if !isRegularHead(path) {
		return ""
	}
	h, _ := readHead(path)
	return h
}

// A refreshed file that the pass cannot put back into pending (the link fails with ENOSPC)
// is kept under its claim, never removed, and the next pass puts it back and runs it.
func TestMoveEntryKeepsARefreshedFileItCannotPutBack(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	newer := w.head("a2", map[string]string{"a2.txt": "a2\n"}, true)
	w.pend("1", a)
	pending := filepath.Join(w.cfg.QueueDir, "pending", "1")
	done := filepath.Join(w.cfg.QueueDir, "done", "1")
	fired := false
	w.r.hookMove = func(src string) {
		if !fired {
			fired = true
			if err := os.WriteFile(src, []byte(newer+"\n"), 0o664); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.r.hookLink = noSpaceLink
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if !fired {
		t.Fatal("the seam did not run")
	}
	if got := w.claimHeads(); len(got) != 1 || got[0] != newer {
		t.Fatalf("claims hold %v, want the refreshed head %s kept (pending %q, done %q)", got, newer, regularHead(pending), regularHead(done))
	}
	if regularHead(done) != "" {
		t.Fatal("the refreshed head was carried into done/")
	}
	dry, err := w.r.DryRun(context.Background(), false)
	if err != nil || len(dry.Claims) != 1 {
		t.Fatalf("dry run claims = %v (%v), want the kept claim", dry.Claims, err)
	}

	// The disk recovers: the next pass puts the file back before it reads the queue (pending is
	// empty, so the pass must not stop at idle) and runs the refreshed head.
	w.r.hookMove, w.r.hookLink = nil, nil
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if got := regularHead(done); got != newer {
		t.Fatalf("done/1 holds %q, want the refreshed head %s", got, newer)
	}
	if got := w.claimHeads(); len(got) != 0 {
		t.Fatalf("claims left: %v", got)
	}
}

// A processed file whose final move fails (done/1 is in the way) and that cannot be put back
// either is kept under its claim, and the next pass puts it back into pending.
func TestMoveEntryKeepsAProcessedFileWhenMoveAndPutBackFail(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	w.pend("1", a)
	pending := filepath.Join(w.cfg.QueueDir, "pending", "1")
	done := filepath.Join(w.cfg.QueueDir, "done", "1")
	w.write(filepath.Join(done, "in-the-way"), "x\n")
	w.r.hookLink = noSpaceLink
	rep := w.pass()
	if got := w.claimHeads(); len(got) != 1 || got[0] != a {
		t.Fatalf("claims hold %v, want the processed head %s kept (pass %s, pending %q)", got, a, rep.Result, regularHead(pending))
	}

	if err := os.RemoveAll(done); err != nil {
		t.Fatal(err)
	}
	w.r.hookLink = nil
	rep = w.pass()
	if got := w.claimHeads(); len(got) != 0 {
		t.Fatalf("claims left: %v", got)
	}
	if regularHead(pending) != a && regularHead(done) != a {
		t.Fatalf("queue file 1 lost after the recovery pass %s (pending %q, done %q)", rep.Result, regularHead(pending), regularHead(done))
	}
}

// A put-back refused because the coordinator wrote a newer pending file meanwhile lets that
// newer file win: the claim goes, the newer file stays.
func TestMoveEntryPutBackLetsANewerPendingFileWin(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	newer := w.head("a2", map[string]string{"a2.txt": "a2\n"}, true)
	w.pend("1", a)
	pending := filepath.Join(w.cfg.QueueDir, "pending", "1")
	w.write(filepath.Join(w.cfg.QueueDir, "done", "1", "in-the-way"), "x\n")
	w.r.hookLink = func(oldname, newname string) error {
		w.write(newname, newer+"\n")
		return os.Link(oldname, newname)
	}
	w.pass()
	if got := regularHead(pending); got != newer {
		t.Fatalf("pending holds %q, want the newer head %s", got, newer)
	}
	if got := w.claimHeads(); len(got) != 0 {
		t.Fatalf("claims left: %v", got)
	}
}

// A put-back refused because something that is not a queue file sits at pending/<name> keeps
// the claim.
func TestRestoreClaimKeepsTheClaimWhenPendingHoldsNoFile(t *testing.T) {
	w := newWorld(t)
	if err := w.r.ensureQueueDirs(); err != nil {
		t.Fatal(err)
	}
	claim := filepath.Join(w.cfg.QueueDir, "done", claimPrefix+"1-7")
	w.write(claim, "0123456789012345678901234567890123456789\n")
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "1", "x"), "x\n")
	if w.r.restoreClaim(claim, "1") {
		t.Fatal("restoreClaim reported the claim gone")
	}
	if _, err := os.Stat(claim); err != nil {
		t.Fatalf("claim removed: %v", err)
	}
}
