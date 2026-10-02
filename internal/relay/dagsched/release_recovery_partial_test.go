package dagsched

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// c1: a write of a new copy that fails part way (a full disk) leaves a file that does not hash to its name. The call that created it took it back, whatever it holds of the bytes it meant to
// write, because a strict prefix of those bytes cannot be a manifest copy anything relies on.
func TestAFailedWriteOfANewCopyIsTakenBack(t *testing.T) {
	for name, written := range map[string]func(canonical []byte) []byte{
		"half of the bytes":    func(c []byte) []byte { return c[:len(c)/2] },
		"all but the last":     func(c []byte) []byte { return c[:len(c)-1] },
		"nothing at all":       func([]byte) []byte { return nil },
		"the first few bytes":  func(c []byte) []byte { return c[:7] },
		"a prefix of one byte": func(c []byte) []byte { return c[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			k := newReleaseKit(t)
			k.largeB()
			var wrote int
			testFreezeWrite = func(file *os.File, canonical []byte) error {
				part := written(canonical)
				wrote = len(part)
				if _, err := file.Write(part); err != nil {
					return err
				}
				return errors.New("no space left on device")
			}
			t.Cleanup(func() { testFreezeWrite = nil })
			before := k.rows()
			_, err := k.release("rp", "B")
			if err == nil || !strings.Contains(err.Error(), "no space left on device") {
				t.Fatalf("release = %v, want the write failure", err)
			}
			if got := k.copies(); len(got) != 0 {
				t.Fatalf("a failed write left %v", got)
			}
			removed := k.journal("dag_manifest_copy_removed")
			if len(removed) != 1 || removed[0]["why"] != "incomplete_write" || removed[0]["cause"] != "error" || int(removed[0]["bytes"].(float64)) != wrote {
				t.Fatalf("journal = %v, want one removal of %d bytes as incomplete_write", removed, wrote)
			}
			if k.rows() != before || k.heldSlots() != 0 || k.children() != 0 {
				t.Fatalf("a failed write left rows %+v -> %+v, held %d", before, k.rows(), k.heldSlots())
			}
			// the disk has room again: the same release binds, over exactly one copy
			testFreezeWrite = nil
			res := k.mustRelease("rp", "B")
			if !res.Bound {
				t.Fatalf("retry = %+v", res)
			}
			if copies := k.copies(); len(copies) != 1 || !strings.Contains(k.host.sent[0], copies[0]) {
				t.Fatalf("copies after the retry = %v, the prompt names another file", copies)
			}
		})
	}
}

// c1: only what its own failed write could have left is taken back. A file at the path that holds other bytes (planted, or swapped in between the write and the cleanup) stays, with its evidence.
func TestAFailedWriteLeavesAFileThatIsNotAPrefixOfTheBytes(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	testFreezeWrite = func(file *os.File, canonical []byte) error {
		if _, err := file.Write([]byte("not the start of the manifest")); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { testFreezeWrite = nil })
	if _, err := k.release("rp", "B"); err == nil {
		t.Fatal("the release was expected to fail")
	}
	if got := k.copies(); len(got) != 1 {
		t.Fatalf("copies = %v, the file that is no prefix of the manifest must stay", got)
	}
	if kept := k.journal("dag_manifest_copy_kept"); len(kept) != 1 || kept[0]["why"] != "bytes_differ" {
		t.Fatalf("journal kept = %v", kept)
	}
	if removed := k.journal("dag_manifest_copy_removed"); len(removed) != 0 {
		t.Fatalf("journal removed = %v", removed)
	}
}

// c1: a call that only reused a file never takes it back, even when the file holds a prefix of its bytes (the creator's write is still going, or failed and is its creator's to clean).
func TestAReusedPartialFileIsNotTheCallsToTakeBack(t *testing.T) {
	k := newReleaseKit(t)
	k.largeB()
	canonical := []byte(strings.Repeat("reused ", 100))
	if err := os.MkdirAll(k.root+"/dag-input-manifests", 0o700); err != nil {
		t.Fatal(err)
	}
	path := frozenManifestPath(k.root, canonical)
	if err := os.WriteFile(path, canonical[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := freezeManifestCopy(k.root, canonical); err == nil || created {
		t.Fatalf("freeze over a partial file = created %v, %v: it must refuse and claim nothing", created, err)
	}
	reused := &frozenCopy{Root: k.root, Path: path, SHA: shaOf(canonical), Canonical: canonical, Created: false}
	if removed, err := k.sched.discardFrozenCopy(t.Context(), reused, "rp", "B", "test"); err != nil || removed {
		t.Fatalf("discard of a reused partial file = %v %v", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a file the call did not create was removed")
	}
}
