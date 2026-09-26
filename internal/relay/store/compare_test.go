package store

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// identityFixture is test_store.py Identity: a store in a/ that can be copied the way a person
// copying a state directory would, sidecars included.
type identityFixture struct {
	t     *testing.T
	tmp   string
	a     string
	store *Store
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	a := filepath.Join(root, "a")
	s, err := Open(context.Background(), filepath.Join(a, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	f := &identityFixture{t: t, tmp: root, a: a, store: s}
	t.Cleanup(f.close)
	return f
}

func (f *identityFixture) close() {
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			f.t.Error(err)
		}
		f.store = nil
	}
}

func (f *identityFixture) copyStore(into string) string {
	f.t.Helper()
	if err := os.MkdirAll(into, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(f.a, "relay.sqlite3"+suffix))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(into, "relay.sqlite3"+suffix), data, 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return into
}

func (f *identityFixture) probe(dir string) Location {
	f.t.Helper()
	return Probe(context.Background(), StateSelection{Path: dir}).Store
}

func (f *identityFixture) nonce(dir, nonce string) NonceReading {
	return NonceLookup(context.Background(), StateSelection{Path: dir}, nonce)
}

func (f *identityFixture) locate() Location {
	f.t.Helper()
	loc, err := f.store.Locate(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return loc
}

func (f *identityFixture) challenge() string {
	f.t.Helper()
	written, err := f.store.WriteChallengeFor(context.Background(), "parent")
	if err != nil {
		f.t.Fatal(err)
	}
	return written.Nonce
}

func (f *identityFixture) hardlink(dir string) string {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Link(filepath.Join(f.a, "relay.sqlite3"), filepath.Join(dir, "relay.sqlite3")); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func requireVerdict(t *testing.T, got StoreComparison, want StoreVerdict, detail ...string) {
	t.Helper()
	if got.SameStore != want {
		t.Fatalf("verdict %+v, want %s", got, want)
	}
	for _, fragment := range detail {
		if !strings.Contains(got.Detail, fragment) {
			t.Fatalf("detail %q lacks %q", got.Detail, fragment)
		}
	}
}

func logLocation(l Location, inode uint64) string {
	return strconv.FormatUint(l.LogDevice, 10) + ":" + strconv.FormatUint(inode, 10) + ":" + l.LogName
}

func TestCompareStore_python_identity_grades(t *testing.T) {
	t.Run("test_a_copy_keeps_the_identifier_and_is_not_the_same_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		mine := f.locate()
		theirs := f.probe(f.copyStore(filepath.Join(f.tmp, "b")))
		if theirs.StoreID != mine.StoreID || theirs.Inode == mine.Inode {
			t.Fatalf("copy %+v mine %+v", theirs, mine)
		}
		requireVerdict(t, CompareStore(theirs, CompareExpectations{StoreID: mine.StoreID}), Unproven)
		requireVerdict(t, CompareStore(theirs, CompareExpectations{StoreID: mine.StoreID, Inode: mine.PhysicalIdentity()}), Mismatch)
	})
	t.Run("test_a_nonce_written_after_the_copy_separates_them", func(t *testing.T) {
		f := newIdentityFixture(t)
		b := f.copyStore(filepath.Join(f.tmp, "b"))
		if f.probe(b).StoreID != f.locate().StoreID {
			t.Fatal("the copy must be a real copy")
		}
		written := f.challenge()
		here, there := f.nonce(f.a, written), f.nonce(b, written)
		if !here.Found || there.Found || !there.Readable {
			t.Fatalf("here %+v there %+v", here, there)
		}
		mine := f.locate()
		requireVerdict(t, CompareStore(mine, CompareExpectations{Inode: mine.PhysicalIdentity(), Log: mine.LogLocation(), Nonce: &here}), Proven)
		requireVerdict(t, CompareStore(mine, CompareExpectations{Nonce: &here}), Unproven)
		requireVerdict(t, CompareStore(f.probe(b), CompareExpectations{Nonce: &there}), Mismatch)
	})
	t.Run("test_a_nonce_copied_with_the_bytes_is_not_proof_of_a_shared_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		mine := f.probe(f.a)
		copied := f.copyStore(filepath.Join(f.tmp, "copied-after"))
		theirs := f.probe(copied)
		here := f.nonce(copied, written)
		graded := CompareStore(theirs, CompareExpectations{Nonce: &here})
		if graded.SameStore == Proven || !strings.Contains(graded.Detail, "copy") {
			t.Fatalf("copied nonce %+v", graded)
		}
		if !here.Found || theirs.StoreID != mine.StoreID || theirs.Inode == mine.Inode {
			t.Fatalf("not the described copy: %+v %+v %+v", here, theirs, mine)
		}
		requireVerdict(t, CompareStore(theirs, CompareExpectations{Inode: mine.PhysicalIdentity(), Nonce: &here}), Mismatch)
	})
	t.Run("test_a_nonce_query_that_fails_is_unreadable_rather_than_absent", func(t *testing.T) {
		f := newIdentityFixture(t)
		broken := filepath.Join(f.tmp, "broken")
		if err := os.MkdirAll(broken, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(broken, "relay.sqlite3"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		answer := f.nonce(broken, "any-nonce")
		if answer.Found || answer.Readable || answer.Detail == "" {
			t.Fatalf("broken %+v", answer)
		}
		if got := CompareStore(f.probe(broken), CompareExpectations{Nonce: &answer}); got.SameStore == Mismatch {
			t.Fatalf("unreadable graded as mismatch %+v", got)
		}
	})
	t.Run("test_a_symlinked_directory_is_the_same_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		alias := filepath.Join(f.tmp, "alias")
		if err := os.Symlink(f.a, alias); err != nil {
			t.Fatal(err)
		}
		mine := f.locate()
		through := f.probe(alias)
		if through.DBPath == mine.DBPath || through.RealPath != mine.RealPath || through.Links != 1 {
			t.Fatalf("alias %+v mine %+v", through, mine)
		}
		requireVerdict(t, CompareStore(through, CompareExpectations{StoreID: mine.StoreID, Inode: mine.PhysicalIdentity()}), Unproven)
		found := f.nonce(alias, f.challenge())
		if !found.Found || through.LogLocation() != mine.LogLocation() {
			t.Fatalf("found %+v logs %s %s", found, through.LogLocation(), mine.LogLocation())
		}
		requireVerdict(t, CompareStore(through, CompareExpectations{Inode: mine.PhysicalIdentity(), Log: mine.LogLocation(), Nonce: &found}), Proven)
	})
	t.Run("test_an_agreeing_device_and_inode_is_never_proof_on_its_own", func(t *testing.T) {
		f := newIdentityFixture(t)
		mine := f.locate()
		if mine.Links != 1 {
			t.Fatalf("links %d", mine.Links)
		}
		requireVerdict(t, CompareStore(f.probe(f.a), CompareExpectations{StoreID: mine.StoreID, Inode: mine.PhysicalIdentity()}), Unproven, "pathname")
		elsewhere := f.copyStore(filepath.Join(f.tmp, "elsewhere"))
		requireVerdict(t, CompareStore(f.probe(elsewhere), CompareExpectations{Inode: mine.PhysicalIdentity()}), Mismatch)
	})
	t.Run("test_a_second_name_for_one_inode_is_not_proof_of_a_shared_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		mine := f.locate()
		f.close()
		theirs := f.probe(f.hardlink(filepath.Join(f.tmp, "hardlink")))
		requireVerdict(t, CompareStore(theirs, CompareExpectations{StoreID: mine.StoreID, Inode: mine.PhysicalIdentity()}), Unproven, "names")
		if theirs.StoreID != mine.StoreID || theirs.Device != mine.Device || theirs.Inode != mine.Inode || theirs.RealPath == mine.RealPath || theirs.Links != 2 {
			t.Fatalf("not the described hardlink: %+v %+v", theirs, mine)
		}
	})
	t.Run("test_a_nonce_does_not_talk_the_second_name_up_into_proof", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		other := f.hardlink(filepath.Join(f.tmp, "hardlink"))
		found := f.nonce(other, written)
		if !found.Found {
			t.Fatalf("nonce not readable through the second name: %+v", found)
		}
		requireVerdict(t, CompareStore(f.probe(other), CompareExpectations{Nonce: &found}), Unproven, "names")
	})
	t.Run("test_a_second_pathname_no_name_count_can_see_is_still_refused", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		found := f.nonce(f.a, written)
		if measured.Links != 1 || !found.Found {
			t.Fatalf("measured %+v found %+v", measured, found)
		}
		peer := logLocation(measured, measured.LogInode+1)
		requireVerdict(t, CompareStore(measured, CompareExpectations{StoreID: measured.StoreID, Inode: measured.PhysicalIdentity(), Log: peer, Nonce: &found}), Unproven, peer, strconv.FormatUint(measured.LogInode, 10))
	})
	t.Run("test_two_pathnames_that_share_one_log_are_still_one_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		found := f.nonce(f.a, written)
		elsewhere := measured
		elsewhere.RealPath = "/mounted/somewhere/else/relay.sqlite3"
		expect := CompareExpectations{Inode: measured.PhysicalIdentity(), Log: measured.LogLocation(), Nonce: &found}
		requireVerdict(t, CompareStore(elsewhere, expect), Proven)
		expect.Log = logLocation(measured, measured.LogInode+1)
		requireVerdict(t, CompareStore(elsewhere, expect), Unproven)
	})
	t.Run("test_a_nonce_is_not_proof_until_the_log_location_has_been_compared", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		found := f.nonce(f.a, written)
		graded := CompareStore(measured, CompareExpectations{Inode: measured.PhysicalIdentity(), Nonce: &found})
		requireVerdict(t, graded, Unproven, "--expect-log")
		if strings.Contains(graded.Detail, "--expect-inode") {
			t.Fatalf("asked again for a supplied expectation: %q", graded.Detail)
		}
		requireVerdict(t, CompareStore(measured, CompareExpectations{Nonce: &found}), Unproven, "--expect-inode", "--expect-log")
	})
	t.Run("test_a_log_location_that_cannot_be_used_is_not_comparable_rather_than_different", func(t *testing.T) {
		f := newIdentityFixture(t)
		measured := f.probe(f.a)
		// "" is Python's expect_log="", which is supplied; Go's empty string means absent,
		// so the malformed spellings carry the case here.
		for _, unusable := range []string{"1:2", "nonsense"} {
			requireVerdict(t, CompareStore(measured, CompareExpectations{Log: unusable}), Unproven, "not comparable here")
		}
		unmeasured := measured
		unmeasured.LogDevice, unmeasured.LogInode, unmeasured.LogName = 0, 0, ""
		requireVerdict(t, CompareStore(unmeasured, CompareExpectations{Log: measured.LogLocation()}), Unproven, "not comparable here")
	})
	t.Run("test_a_nonce_read_through_another_log_is_not_attributed_to_this_one", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		found := f.nonce(f.a, written)
		elsewhere := found
		elsewhere.LogInode++
		requireVerdict(t, CompareStore(measured, CompareExpectations{Inode: measured.PhysicalIdentity(), Log: measured.LogLocation(), Nonce: &elsewhere}), Unproven, "read through a pathname")
		unmeasured := found
		unmeasured.LogDevice, unmeasured.LogInode, unmeasured.LogName = 0, 0, ""
		requireVerdict(t, CompareStore(measured, CompareExpectations{Inode: measured.PhysicalIdentity(), Log: measured.LogLocation(), Nonce: &unmeasured}), Unproven, "could not be measured")
	})
	t.Run("test_a_nonce_read_from_a_replacement_does_not_prove_the_measured_store", func(t *testing.T) {
		f := newIdentityFixture(t)
		mine := f.locate()
		written := f.challenge()
		f.close()
		replacement := f.copyStore(filepath.Join(f.tmp, "replacement"))
		if err := os.Rename(filepath.Join(replacement, "relay.sqlite3"), filepath.Join(f.a, "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		answer := f.nonce(f.a, written)
		graded := CompareStore(mine, CompareExpectations{Nonce: &answer})
		if graded.SameStore == Proven || !strings.Contains(graded.Detail, "read from") {
			t.Fatalf("replacement %+v", graded)
		}
		if !answer.Found || (answer.Device == mine.Device && answer.Inode == mine.Inode) {
			t.Fatalf("not the described replacement: %+v", answer)
		}
	})
	t.Run("test_a_name_added_after_the_probe_still_vetoes_a_found_nonce", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		if measured.Links != 1 {
			t.Fatalf("links %d", measured.Links)
		}
		f.hardlink(filepath.Join(f.tmp, "late-hardlink"))
		answer := f.nonce(f.a, written)
		requireVerdict(t, CompareStore(measured, CompareExpectations{Nonce: &answer}), Unproven, "names")
		if !answer.Found || answer.Links != 2 || answer.Device != measured.Device || answer.Inode != measured.Inode {
			t.Fatalf("answer %+v", answer)
		}
	})
	t.Run("test_a_name_that_goes_away_during_the_read_is_still_counted", func(t *testing.T) {
		f := newIdentityFixture(t)
		written := f.challenge()
		f.close()
		measured := f.probe(f.a)
		alias := filepath.Join(f.tmp, "vanishing.sqlite3")
		if err := os.Link(filepath.Join(f.a, "relay.sqlite3"), alias); err != nil {
			t.Fatal(err)
		}
		// The unlink is injected at the seam between the read's opening and closing
		// observations rather than raced: both counts are real counts of the real file.
		var seen []uint64
		ctx := context.WithValue(context.Background(), diagnosticSeamsKey{}, diagnosticSeams{identity: func(fd int) (heldIdentity, bool) {
			answer, ok := fstatIdentity(fd)
			seen = append(seen, answer.links)
			if len(seen) == 1 {
				if err := os.Remove(alias); err != nil {
					t.Error(err)
				}
			}
			return answer, ok
		}})
		answer := NonceLookup(ctx, StateSelection{Path: f.a}, written)
		requireVerdict(t, CompareStore(measured, CompareExpectations{Nonce: &answer}), Unproven, "names")
		if len(seen) != 2 || seen[0] != 2 || seen[1] != 1 {
			t.Fatalf("observations %v", seen)
		}
		if measured.Links != 1 || !answer.Found || answer.Links != 2 {
			t.Fatalf("measured %+v answer %+v", measured, answer)
		}
	})
	t.Run("test_a_store_with_no_identity_is_never_proven_equal", func(t *testing.T) {
		requireVerdict(t, CompareStore(Location{}, CompareExpectations{StoreID: "whatever"}), Unproven)
	})
	t.Run("test_an_unreadable_nonce_is_unproven_rather_than_a_mismatch", func(t *testing.T) {
		unreadable := NonceReading{Nonce: "abc", Detail: "OperationalError: unable to open database file"}
		requireVerdict(t, CompareStore(Location{StoreID: "x"}, CompareExpectations{Nonce: &unreadable}), Unproven, "could not be read")
		absent := NonceReading{Nonce: "abc", Readable: true}
		requireVerdict(t, CompareStore(Location{StoreID: "x"}, CompareExpectations{Nonce: &absent}), Mismatch)
	})
}
