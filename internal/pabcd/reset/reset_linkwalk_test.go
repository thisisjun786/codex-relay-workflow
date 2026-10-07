package reset

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// resetLinkWalkPinned opens dir as the pinned directory the link judgement reads through — the
// os.Root the removals act with, plus the descriptor resetPin takes — and closes it when the test
// ends.
func resetLinkWalkPinned(t *testing.T, dir string) *resetLinkWalkPin {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetLinkWalkPinOf(root)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })
	return pinned
}

// resetLinkWalkWorkspace makes a directory holding keep/ (with inner.txt), which the links of
// these cases point at.
func resetLinkWalkWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResetLinkWalkRemovesDotEndingLinkInARenamedPinnedDirectory: the judgement walks the target's
// components through the pinned descriptor and never uses the root's path name, so renaming the
// pinned directory between resetPin and the removal no longer changes the verdict. The dot-ending
// link is removed and reset continues with the remaining candidates, as it did before CRW-745.
func TestResetLinkWalkRemovesDotEndingLinkInARenamedPinnedDirectory(t *testing.T) {
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/.", filepath.Join(sessions, "a.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(sessions, "b.json")); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	observed, err := parent.Lstat("sessions")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetPin(parent, "sessions", observed)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	moved := filepath.Join(base, "moved")
	if err := os.Rename(sessions, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	result := ResetResult{Removed: []string{}, Absent: []string{}}
	if err := resetRmIfExists(pinned, "a.json", "a.json", &result); err != nil {
		t.Fatalf("a.json: %v", err)
	}
	if err := resetRmIfExists(pinned, "b.json", "b.json", &result); err != nil {
		t.Fatalf("b.json: %v (reset must continue with the remaining candidates)", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "a.json" {
		t.Errorf("removed = %v, want [a.json]", result.Removed)
	}
	if len(result.Absent) != 1 || result.Absent[0] != "b.json" {
		t.Errorf("absent = %v, want [b.json]", result.Absent)
	}
	if _, err := os.Lstat(filepath.Join(moved, "a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pinned link must be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "keep", "inner.txt")); err != nil {
		t.Errorf("target directory lost: %v", err)
	}
}

// TestResetLinkWalkChainDoesNotOpenTheTargetDirectory: a link whose immediate target is another
// link ending in a dot component (interviews -> alias, alias -> keep/.) is judged by walking the
// chain, so the target directory is never opened. The root stat seam counts the descriptor-path
// calls; a walk that never opens the target makes none, where CRW-745's dot-ending check on the
// link's own Readlink text only does not see through the chain.
func TestResetLinkWalkChainDoesNotOpenTheTargetDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, link, target string
		wantExists         bool
	}{
		{"chain_ending_dot", "interviews", "alias", true},
		{"chain_ending_dot_missing", "gone", "alias_missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := resetLinkWalkWorkspace(t)
			if err := os.Symlink("keep/.", filepath.Join(dir, "alias")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing/.", filepath.Join(dir, "alias_missing")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(dir, tc.link)); err != nil {
				t.Fatal(err)
			}
			pinned := resetLinkWalkPinned(t, dir)
			calls := 0
			stat := func(name string) (os.FileInfo, error) {
				calls++
				return pinned.Stat(name)
			}
			got, err := resetLinkTargetExistsWith(pinned, tc.link, stat)
			if err != nil {
				t.Fatalf("resetLinkTargetExistsWith: %v", err)
			}
			if got != tc.wantExists {
				t.Errorf("exists = %v, want %v", got, tc.wantExists)
			}
			if calls != 0 {
				t.Errorf("root stat calls = %d, want 0: the target directory must not be opened", calls)
			}
		})
	}
}

// TestResetLinkWalkJudgesLikeToday pins the verdicts the walk must not change: an absent target, a
// link loop, a chain past 40 links, an out-of-root absolute target, and a component that is not a
// directory where a directory is needed. Every one of these judges as it did before the walk.
func TestResetLinkWalkJudgesLikeToday(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, dir, outside string) string
		wantExists bool
	}{
		{"absent_target", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "missing", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"link_loop", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "loop.json", filepath.Join(dir, "loop.json"))
			return "loop.json"
		}, false},
		{"chain_past_40", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "keep/inner.txt", filepath.Join(dir, "c41"))
			for i := 40; i >= 1; i-- {
				resetLinksLink(t, "c"+strconv.Itoa(i+1), filepath.Join(dir, "c"+strconv.Itoa(i)))
			}
			resetLinksLink(t, "c1", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"out_of_root_absolute_present", func(t *testing.T, dir, outside string) string {
			resetLinksWrite(t, filepath.Join(outside, "keep.json"), "outside")
			resetLinksLink(t, filepath.Join(outside, "keep.json"), filepath.Join(dir, "a.json"))
			return "a.json"
		}, true},
		{"out_of_root_absolute_dangling", func(t *testing.T, dir, outside string) string {
			resetLinksLink(t, filepath.Join(outside, "gone.json"), filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"not_a_directory_component", func(t *testing.T, dir, _ string) string {
			resetLinksWrite(t, filepath.Join(dir, "file"), "regular")
			resetLinksLink(t, "file/inner", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"trailing_slash_on_a_regular_file", func(t *testing.T, dir, _ string) string {
			resetLinksWrite(t, filepath.Join(dir, "file"), "regular")
			resetLinksLink(t, "file/", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"empty_component_inside_the_target", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "keep//inner.txt", filepath.Join(dir, "a.json"))
			return "a.json"
		}, true},
		{"dot_ending_at_the_root", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "keep/..", filepath.Join(dir, "a.json"))
			return "a.json"
		}, true},
		{"dot_ending_below_two_components", func(t *testing.T, dir, _ string) string {
			if err := os.Mkdir(filepath.Join(dir, "keep", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			resetLinksLink(t, "keep/sub/..", filepath.Join(dir, "a.json"))
			return "a.json"
		}, true},
		{"dot_ending_below_two_components_absent", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "keep/missing/..", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
		{"dotdot_above_the_root", func(t *testing.T, dir, _ string) string {
			resetLinksLink(t, "../../outside.json", filepath.Join(dir, "a.json"))
			return "a.json"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, outside := resetLinkWalkWorkspace(t), t.TempDir()
			link := tc.setup(t, dir, outside)
			pinned := resetLinkWalkPinned(t, dir)
			got, err := resetLinkTargetExists(pinned, link)
			if err != nil {
				t.Fatalf("resetLinkTargetExists: %v", err)
			}
			if got != tc.wantExists {
				t.Errorf("exists = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

// TestResetLinkWalkKeepsTheDescriptorPathWhenReadlinkFails: resetLinkTargetExists is only reached
// for a name that exists, and Readlink fails for anything that is not a link, so that case keeps
// today's flow, the descriptor path. The judgement therefore never uses the root's path name for
// it, and a renamed pinned directory does not turn a present entry into a refusal.
func TestResetLinkWalkKeepsTheDescriptorPathWhenReadlinkFails(t *testing.T) {
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "regular.txt"), []byte("regular"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	observed, err := parent.Lstat("sessions")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetPin(parent, "sessions", observed)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := os.Rename(sessions, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resetLinkTargetExists(pinned, "regular.txt")
	if err != nil {
		t.Fatalf("resetLinkTargetExists: %v", err)
	}
	if !got {
		t.Error("exists = false, want true: the descriptor path still names the entry")
	}
}

// TestResetLinkWalkChainEscapingThroughASplicedLink: the links before a ".." are expanded before it
// is popped, so a link that resolves to ".." is spliced first and the pop that follows can leave
// the root. A spliced target that stays inside resolves by the walk; one that walks above the root
// keeps the OS-path judgement, and the verdict is the one the OS gives the link itself.
func TestResetLinkWalkChainEscapingThroughASplicedLink(t *testing.T) {
	t.Run("spliced_dotdot_stays_inside", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// sub resolves to keep/.., i.e. the root itself, so "sub/x" is "x" once spliced and popped.
		if err := os.Symlink(filepath.Join("keep", ".."), filepath.Join(dir, "sub")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("sub/x", filepath.Join(dir, "a.json")); err != nil {
			t.Fatal(err)
		}
		got, err := resetLinkTargetExists(resetLinkWalkPinned(t, dir), "a.json")
		if err != nil {
			t.Fatalf("resetLinkTargetExists: %v", err)
		}
		if !got {
			t.Error("exists = false, want true: the spliced \"..\" pops back to the root")
		}
	})
	t.Run("spliced_dotdot_leaves_the_root", func(t *testing.T) {
		dir := t.TempDir()
		// sub resolves to "..", so "sub/../.." is above the root and the walk hands the target to the
		// OS path. Its verdict is what the OS answers for the link, which is the fallback contract.
		if err := os.Symlink("..", filepath.Join(dir, "sub")); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "a.json")
		if err := os.Symlink("sub/../..", link); err != nil {
			t.Fatal(err)
		}
		_, statErr := os.Stat(link)
		got, err := resetLinkTargetExists(resetLinkWalkPinned(t, dir), "a.json")
		if err != nil {
			t.Fatalf("resetLinkTargetExists: %v", err)
		}
		if got != (statErr == nil) {
			t.Errorf("exists = %v, want the OS verdict %v", got, statErr == nil)
		}
	})
}

// TestResetLinkWalkCountsTheCandidateLinkInTheCeiling: the caller already read the candidate
// link's target, and the kernel counts that link as the first of the 40 traversals it allows for
// the whole resolution. A chain of 39 links inside the target is 40 and resolves; one of 40 is 41
// and the OS reports a loop, so the walk must not call the target present either. Each case also
// compares the walk with os.Stat on the same chain.
func TestResetLinkWalkCountsTheCandidateLinkInTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		innerLinks int
		wantExists bool
	}{
		{38, true},
		{39, true},
		{40, false},
		{41, false},
	} {
		t.Run(strconv.Itoa(tc.innerLinks)+"_inner_links", func(t *testing.T) {
			dir := resetLinkWalkWorkspace(t)
			// a.json -> c1 -> ... -> c<inner> -> keep/. makes inner+1 traversals in all.
			resetLinksLink(t, "keep/.", filepath.Join(dir, "c"+strconv.Itoa(tc.innerLinks)))
			for i := tc.innerLinks - 1; i >= 1; i-- {
				resetLinksLink(t, "c"+strconv.Itoa(i+1), filepath.Join(dir, "c"+strconv.Itoa(i)))
			}
			link := filepath.Join(dir, "a.json")
			resetLinksLink(t, "c1", link)
			_, statErr := os.Stat(link)
			got, err := resetLinkTargetExists(resetLinkWalkPinned(t, dir), "a.json")
			if err != nil {
				t.Fatalf("resetLinkTargetExists: %v", err)
			}
			if got != (statErr == nil) {
				t.Errorf("exists = %v, but the OS says %v: the walk must agree with the kernel", got, statErr == nil)
			}
			if got != tc.wantExists {
				t.Errorf("exists = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

// TestResetLinkWalkChainThroughRunReset: the chained dot-ending link is judged the same way
// through the real entry point, so the public behaviour of a reset is pinned and not only the
// helper's. interviews -> alias, alias -> keep/. is a state candidate whose target exists, so
// RunReset removes the link itself and leaves the directory it names.
func TestResetLinkWalkChainThroughRunReset(t *testing.T) {
	root := t.TempDir()
	crw := filepath.Join(root, ".crw")
	if err := os.MkdirAll(filepath.Join(crw, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crw, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/.", filepath.Join(crw, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alias", filepath.Join(crw, "interviews")); err != nil {
		t.Fatal(err)
	}
	got, err := RunReset(root, State)
	if err != nil {
		t.Fatalf("RunReset: %v", err)
	}
	want := filepath.Join(crw, "interviews")
	found := false
	for _, removed := range got.Removed {
		if removed == want {
			found = true
		}
	}
	if !found {
		t.Errorf("removed = %v, want %s among them", got.Removed, want)
	}
	if _, err := os.Lstat(filepath.Join(crw, "interviews")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link must be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(crw, "keep", "inner.txt")); err != nil {
		t.Errorf("the target directory must survive: %v", err)
	}
}

// TestResetLinkWalkKeepsADotEndingLinkWhenSearchIsDenied: the kernel resolves a final "." or
// ".." by traversing into the directory the walk reached, which needs search permission on it.
// The walk reaches that directory with Lstat, which needs none, so without asking the kernel it
// would call the target present where the oracle's existsSync answers false and rmIfExists keeps
// the link. The link must stay absent, and reset must continue with the remaining candidates.
func TestResetLinkWalkKeepsADotEndingLinkWhenSearchIsDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	for _, tc := range []struct {
		name, target, dir string
		mode              os.FileMode
	}{
		{"keep_dot_search_denied", "keep/.", "keep", 0o000},
		{"keep_dotdot_search_denied", "keep/..", "keep", 0o000},
		{"keep_sub_dot_search_denied", "keep/sub/.", filepath.Join("keep", "sub"), 0o000},
		{"keep_sub_dotdot_search_denied", "keep/sub/..", filepath.Join("keep", "sub"), 0o000},
		{"keep_dot_search_only_still_removed", "keep/.", "keep", 0o311},
		{"keep_dotdot_search_only_still_removed", "keep/..", "keep", 0o311},
		{"keep_dot_writable_still_removed", "keep/.", "keep", 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "keep", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(dir, "a.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(dir, tc.dir), tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chmod(filepath.Join(dir, "keep", "sub"), 0o755)
				_ = os.Chmod(filepath.Join(dir, "keep"), 0o755)
			})
			// The oracle's verdict: an OS stat of the link, which the kernel resolves by traversing
			// into the directory the target names. Concatenated, not joined, so the "." survives.
			_, oracleErr := os.Stat(dir + "/a.json")
			wantExists := oracleErr == nil
			got, err := resetLinkTargetExists(resetLinkWalkPinned(t, dir), "a.json")
			if err != nil {
				t.Fatalf("resetLinkTargetExists: %v", err)
			}
			if got != wantExists {
				t.Errorf("exists = %v, but the oracle's stat answers %v", got, oracleErr)
			}
			// The removal itself, through the entry point, must follow the same verdict.
			crw := filepath.Join(dir, ".crw")
			if err := os.MkdirAll(filepath.Join(crw, "sessions"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(crw, "sessions", "b.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := RunReset(dir, State); err != nil {
				t.Fatalf("RunReset: %v", err)
			}
			_, linkErr := os.Lstat(filepath.Join(crw, "sessions", "b.json"))
			if wantExists && linkErr != nil {
				t.Errorf("the link must be removed: %v", linkErr)
			}
			if !wantExists && linkErr != nil {
				t.Errorf("the link must be kept, and reset must continue: %v", linkErr)
			}
		})
	}
}

// TestResetLinkWalkKeepsASearchDeniedCandidateAtTheCRWRoot: the candidates judged directly under
// .crw (the ledger, interviews and the recovery markers) take the same judgement, so a link there
// whose target ends in a dot component and names a directory that cannot be searched is kept and
// the reset continues with the candidates after it.
func TestResetLinkWalkKeepsASearchDeniedCandidateAtTheCRWRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	for _, tc := range []struct {
		name, target string
		mode         os.FileMode
		wantRemoved  bool
	}{
		{"interviews_dot_search_denied", "keep/.", 0o000, false},
		{"interviews_dotdot_search_denied", "keep/..", 0o000, false},
		{"interviews_dot_search_only", "keep/.", 0o311, true},
		{"interviews_dot_writable", "keep/.", 0o755, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			crw := filepath.Join(root, ".crw")
			if err := os.MkdirAll(filepath.Join(crw, "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(crw, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(crw, "ledger.jsonl"), []byte("ledger"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(crw, "interviews")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(crw, "keep"), tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(crw, "keep"), 0o755) })
			got, err := RunReset(root, State)
			if err != nil {
				t.Fatalf("RunReset: %v", err)
			}
			link := filepath.Join(crw, "interviews")
			_, linkErr := os.Lstat(link)
			if tc.wantRemoved && !errors.Is(linkErr, os.ErrNotExist) {
				t.Errorf("the link must be removed: %v (removed %v)", linkErr, got.Removed)
			}
			if !tc.wantRemoved && linkErr != nil {
				t.Errorf("the link must be kept: %v (removed %v)", linkErr, got.Removed)
			}
			// The reset must reach the candidates after interviews whatever the verdict was.
			if _, err := os.Lstat(filepath.Join(crw, "ledger.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the reset must continue past the candidate: %v", err)
			}
		})
	}
}
