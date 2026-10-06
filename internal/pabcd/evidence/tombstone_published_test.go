package evidence

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These are the CRW-778 cases for the evidence writers: a state write that published the new state at the final path and
// then failed the directory sync (state.Published) is a write that happened. The seam is the last argument of the two
// unexported writers, a function the tests supply, so no package-level variable carries test state.

// publishedEvidenceWrite publishes through state.WriteState and then reports the post-rename failure WriteState returns as
// *state.PublishedError, which is the shape a directory sync failure reaches these callers with.
func publishedEvidenceWrite(cwd string, s state.State) error {
	if err := state.WriteState(cwd, s); err != nil {
		return err
	}
	return &state.PublishedError{Err: syscall.EIO}
}

// evidenceLockFree is the session lock without the filesystem, so the case is about the write and not the lock.
func evidenceLockFree(_, _ string, fn func() error) error { return fn() }

// publishedEvidenceHome pins the real-state roots into temporary directories and returns their listings, so a case can show
// that the run touched none of them. The caller compares the listings before and after.
func publishedEvidenceHome(t *testing.T) (codexHome, crwHome string, list func() string) {
	t.Helper()
	home := t.TempDir()
	codexHome, crwHome = filepath.Join(home, "codex"), filepath.Join(home, "crw")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CRW_HOME", crwHome)
	list = func() string {
		var names []string
		for _, root := range []string{codexHome, crwHome} {
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					names = append(names, p)
				}
				return nil
			})
		}
		sort.Strings(names)
		return strings.Join(names, "\n")
	}
	return codexHome, crwHome, list
}

// publishedEvidenceRootsUnchanged fails the case when the temporary roots gained or lost a file, which is how a run that
// reached a real root would show.
func publishedEvidenceRootsUnchanged(t *testing.T, before, after string) {
	t.Helper()
	if before != after {
		t.Fatalf("the pinned roots changed: %q -> %q", before, after)
	}
}

// A tombstone whose commit published is recorded: the call reports true and the sentinel tier never runs, so a healthy
// session does not end up marked unverifiedCorrupt and refused by the goal-complete gate as unreadable.
func TestRecordTombstoneTreatsAPublishedCommitAsWritten(t *testing.T) {
	_, _, list := publishedEvidenceHome(t)
	before := list()
	cwd := t.TempDir()
	if !recordTombstone(cwd, "s1", agent("a1", "t1"), 3, time.Now(), evidenceLockFree, nil, publishedEvidenceWrite) {
		t.Error("a commit whose state was published was reported as failed")
	}
	if s := state.ReadState(cwd, "s1"); s.UnverifiedCorrupt {
		t.Error("the corruption sentinel was written over a session whose tombstone was published")
	}
	if !HasTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("the tombstone is not in the published state")
	}
	publishedEvidenceRootsUnchanged(t, before, list())
}

// A removal whose write published is a removal: the resolve reports true and the tombstone is gone from the visible state.
func TestResolveTombstoneTreatsAPublishedWriteAsRemoved(t *testing.T) {
	_, _, list := publishedEvidenceHome(t)
	before := list()
	cwd := t.TempDir()
	if !RecordTombstone(cwd, "s1", agent("a1", "t1"), MaxAttempts, nil) {
		t.Fatal("seed tombstone failed")
	}
	if !resolveTombstone(cwd, "s1", agent("a1", "t1"), evidenceLockFree, publishedEvidenceWrite) {
		t.Error("a removal whose state was published was reported as not removed")
	}
	if HasTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("the tombstone is still in the published state")
	}
	publishedEvidenceRootsUnchanged(t, before, list())
}

// A failure before publication keeps today's behaviour: nothing was written, so the commit failed, the sentinel tier runs and
// no tombstone is recorded.
func TestRecordTombstonePrePublicationFailureKeepsTheSentinel(t *testing.T) {
	_, _, list := publishedEvidenceHome(t)
	before := list()
	cwd := t.TempDir()
	failing := func(string, state.State) error { return syscall.EIO }
	if recordTombstone(cwd, "s1", agent("a1", "t1"), 3, time.Now(), evidenceLockFree, nil, failing) {
		t.Error("a write that never published was reported as written")
	}
	if !state.ReadState(cwd, "s1").UnverifiedCorrupt {
		t.Error("a pre-publication failure must still raise the sentinel")
	}
	if HasTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("a pre-publication failure recorded a tombstone")
	}
	publishedEvidenceRootsUnchanged(t, before, list())
}

// The resolve keeps today's behaviour for a failure before publication: the tombstone is still there and the call says so.
func TestResolveTombstonePrePublicationFailureReportsNothingRemoved(t *testing.T) {
	_, _, list := publishedEvidenceHome(t)
	before := list()
	cwd := t.TempDir()
	if !RecordTombstone(cwd, "s1", agent("a1", "t1"), MaxAttempts, nil) {
		t.Fatal("seed tombstone failed")
	}
	failing := func(string, state.State) error { return syscall.EIO }
	if resolveTombstone(cwd, "s1", agent("a1", "t1"), evidenceLockFree, failing) {
		t.Error("a removal that never published was reported as removed")
	}
	if !HasTombstone(cwd, "s1", agent("a1", "t1")) {
		t.Error("the tombstone was cleared by a pre-publication failure")
	}
	publishedEvidenceRootsUnchanged(t, before, list())
}
