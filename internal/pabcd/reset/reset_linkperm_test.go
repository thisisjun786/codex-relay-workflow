package reset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file is the red-first evidence of CRW-927. It reads the package through its existing seam —
// RunReset, resetPin, resetRmIfExists and resetLinkTargetExists — and never names the pinned
// directory's type, so the same source compiles against the baseline and against the change: the
// red run is the change's absence rather than a compile error.

// resetLinkPermCRW makes a workspace holding .crw/keep/sub/file and .crw/a/b, and returns the
// workspace root and the .crw path.
func resetLinkPermCRW(t *testing.T) (root, crw string) {
	t.Helper()
	root = t.TempDir()
	crw = filepath.Join(root, ".crw")
	if err := os.MkdirAll(filepath.Join(crw, "keep", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(crw, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crw, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crw, "keep", "sub", "file"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, crw
}

// TestResetLinkPermCeilingFollowsThePlatform: the hop ceiling is the kernel's own, so the walk
// cannot call a chain present that the kernel answers ELOOP for. Linux allows 40 traversals in one
// resolution and XNU allows 32, and a single ceiling would lose a link on Darwin.
func TestResetLinkPermCeilingFollowsThePlatform(t *testing.T) {
	want := 40
	if runtime.GOOS == "darwin" {
		want = 32
	}
	if got := resetLinkWalkLimit(); got != want {
		t.Errorf("resetLinkWalkLimit() = %d, want %d on %s", got, want, runtime.GOOS)
	}
}

// TestResetLinkPermKeepsASearchDeniedMiddleDot: the search question is asked at every "." and ".."
// component the walk passes, not only at the last one. A directory the kernel cannot traverse makes
// the target absent, which is the oracle's answer (existsSync stats the link, the kernel answers
// EACCES, rmIfExists keeps it). On dev only the final dot component asked the question, so a target
// whose last dot component sits in a searchable directory while an earlier one does not was judged
// present and removed; the first three cases were red on dev, the last two are the shape the
// last-component check already answered, kept as controls.
func TestResetLinkPermKeepsASearchDeniedMiddleDot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	for _, tc := range []struct{ name, target, denied string }{
		{"middle_dotdot_then_dot", "keep/../.", "keep"},
		{"middle_dot_then_dotdot_then_dot", "keep/./../.", "keep"},
		{"dotdot_below_two_components", "a/b/../..", "a/b"},
		{"last_dot_only", "keep/.", "keep"},
		{"last_dotdot_only", "keep/./..", "keep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, crw := resetLinkPermCRW(t)
			link := filepath.Join(crw, "interviews")
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			denied := filepath.Join(crw, tc.denied)
			if err := os.Chmod(denied, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })
			// The oracle's verdict: existsSync follows the link with a stat, which the kernel answers
			// EACCES for, so the link is kept.
			if _, err := os.Stat(link); err == nil {
				t.Fatalf("the oracle stat must fail for %s", tc.target)
			}
			got, err := RunReset(root, State)
			if err != nil {
				t.Fatalf("RunReset: %v", err)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("the link must be kept: %v (removed %v)", err, got.Removed)
			}
			if !slices.Contains(got.Absent, link) {
				t.Errorf("absent = %v, want %s among them", got.Absent, link)
			}
		})
	}
}

// TestResetLinkPermJudgesLikeStatInSearchOnlyDirectories: the judgement opens no file and no
// directory. The pinned directory and the intermediate directories are set to 0111 (search only)
// after the pin, where an open for reading would fail with EACCES and change the answer, so a
// verdict that equals os.Stat on the same link is the evidence that nothing is opened.
func TestResetLinkPermJudgesLikeStatInSearchOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny read to root")
	}
	for _, tc := range []struct{ name, target string }{
		{"dot", "."},
		{"keep_dot", "keep/."},
		{"keep_sub_dotdot", "keep/sub/.."},
		{"keep_sub_file", "keep/sub/file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, crw := resetLinkPermCRW(t)
			link := filepath.Join(crw, "alias")
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			// Pin first: the descriptor is taken when the directory is pinned, which is what makes a
			// search-only directory answerable at all.
			parent, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			observed, err := parent.Lstat(".crw")
			if err != nil {
				t.Fatal(err)
			}
			pinned, err := resetPin(parent, ".crw", observed)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			dirs := []string{filepath.Join(crw, "keep", "sub"), filepath.Join(crw, "keep"), crw}
			for _, dir := range dirs {
				if err := os.Chmod(dir, 0o111); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, dir := range dirs {
					_ = os.Chmod(dir, 0o755)
				}
			})
			_, oracleErr := os.Stat(link)
			if oracleErr != nil {
				t.Fatalf("the control itself must resolve for %s: %v", tc.target, oracleErr)
			}
			got, err := resetLinkTargetExists(pinned, "alias")
			if err != nil {
				t.Fatalf("resetLinkTargetExists: %v", err)
			}
			if got != (oracleErr == nil) {
				t.Errorf("exists = %v, but os.Stat on the same link answers %v", got, oracleErr)
			}
		})
	}
}

// TestResetLinkPermInRootFailuresAreAbsentInARenamedPin: a link loop, a chain past the 40-hop
// ceiling and a target under a directory without search permission all stay inside the root, so the
// walk decides them absent with no error and the reset continues to the remaining candidate. On dev
// each of them was handed to the root-path judgement, which refused with "reset directory changed
// while judging a link" once the pinned sessions directory had been renamed, stopping the reset
// before b.json.
func TestResetLinkPermInRootFailuresAreAbsentInARenamedPin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a.json -> loop -> loop is a link loop; d.json -> c1 -> ... -> c41 is a chain past the ceiling;
	// e.json names a file under keep, which is left without search permission. b.json is a plain file,
	// so it is removed without a judgement: the candidate after the three.
	if err := os.Symlink("loop", filepath.Join(sessions, "a.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(sessions, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/inner.txt", filepath.Join(sessions, "c41")); err != nil {
		t.Fatal(err)
	}
	for i := 40; i >= 1; i-- {
		if err := os.Symlink("c"+strconv.Itoa(i+1), filepath.Join(sessions, "c"+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("c1", filepath.Join(sessions, "d.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/inner.txt", filepath.Join(sessions, "e.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "b.json"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(sessions, "keep")
	if err := os.Chmod(keep, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(keep, 0o755)
		_ = os.Chmod(filepath.Join(base, "moved", "keep"), 0o755)
	})

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
	for _, name := range []string{"a.json", "d.json", "e.json"} {
		if err := resetRmIfExists(pinned, name, name, &result); err != nil {
			t.Fatalf("%s: %v (the reset must not stop)", name, err)
		}
	}
	if err := resetRmIfExists(pinned, "b.json", "b.json", &result); err != nil {
		t.Fatalf("b.json: %v (the reset must continue to the remaining candidates)", err)
	}
	wantAbsent := []string{"a.json", "d.json", "e.json"}
	if !slices.Equal(result.Absent, wantAbsent) {
		t.Errorf("absent = %v, want %v", result.Absent, wantAbsent)
	}
	if !slices.Equal(result.Removed, []string{"b.json"}) {
		t.Errorf("removed = %v, want [b.json]", result.Removed)
	}
	for _, name := range wantAbsent {
		if _, err := os.Lstat(filepath.Join(moved, name)); err != nil {
			t.Errorf("%s must be kept: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(moved, "b.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("b.json must be removed: %v", err)
	}
	// The denied directory moved with the pinned tree; restore it so the assertions below and
	// t.TempDir's own cleanup can walk through it.
	if err := os.Chmod(filepath.Join(moved, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "keep", "inner.txt")); err != nil {
		t.Errorf("the target must survive: %v", err)
	}
}

// TestResetLinkPermTenLinkChainIsRemovedWithAndWithoutARename: a chain of ten links that stays inside
// the root and ends at an existing file is a present target, so the link itself is removed — whether
// or not the pinned directory was renamed after it was pinned.
func TestResetLinkPermTenLinkChainIsRemovedWithAndWithoutARename(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("renamed_%v", renamed), func(t *testing.T) {
			base := t.TempDir()
			sessions := filepath.Join(base, "sessions")
			if err := os.MkdirAll(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessions, "keep.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("keep.txt", filepath.Join(sessions, "c10")); err != nil {
				t.Fatal(err)
			}
			for i := 9; i >= 1; i-- {
				if err := os.Symlink("c"+strconv.Itoa(i+1), filepath.Join(sessions, "c"+strconv.Itoa(i))); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("c1", filepath.Join(sessions, "a.json")); err != nil {
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
			target := sessions
			if renamed {
				target = filepath.Join(base, "moved")
				if err := os.Rename(sessions, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(sessions, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Control: with no rename the verdict equals os.Stat on the same link.
			if !renamed {
				_, statErr := os.Stat(filepath.Join(target, "a.json"))
				got, err := resetLinkTargetExists(pinned, "a.json")
				if err != nil {
					t.Fatalf("resetLinkTargetExists: %v", err)
				}
				if got != (statErr == nil) {
					t.Errorf("exists = %v, want the os.Stat verdict %v", got, statErr == nil)
				}
			}
			result := ResetResult{Removed: []string{}, Absent: []string{}}
			if err := resetRmIfExists(pinned, "a.json", "a.json", &result); err != nil {
				t.Fatalf("resetRmIfExists: %v", err)
			}
			if !slices.Equal(result.Removed, []string{"a.json"}) {
				t.Errorf("removed = %v, absent = %v, want [a.json]", result.Removed, result.Absent)
			}
			if _, err := os.Lstat(filepath.Join(target, "a.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the link must be removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
				t.Errorf("the target file must survive: %v", err)
			}
		})
	}
}

// TestResetLinkPermOutOfRootTargetStillRefusesARenamedPin: a target that really leaves the root keeps
// the root-path judgement, so a renamed pinned directory still refuses instead of guessing which
// directory the removal acts on. This is a control: it holds on dev too.
func TestResetLinkPermOutOfRootTargetStillRefusesARenamedPin(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute", filepath.Join("/tmp", "crw-927-outside.json")},
		{"dotdot_above_the_root", "../../outside.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			sessions := filepath.Join(base, "sessions")
			if err := os.MkdirAll(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(sessions, "a.json")); err != nil {
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
			result := ResetResult{Removed: []string{}, Absent: []string{}}
			err = resetRmIfExists(pinned, "a.json", "a.json", &result)
			if err == nil {
				t.Fatalf("the root-path judgement must refuse a renamed pinned directory: removed %v absent %v", result.Removed, result.Absent)
			}
			if !strings.Contains(err.Error(), "reset directory changed") {
				t.Errorf("err = %v, want the renamed-directory refusal", err)
			}
		})
	}
}

// TestResetLinkPermVerdictsMatchStatWithoutARename is the control for the cases above: with the
// pinned directory left where it was, every verdict equals os.Stat on the same link.
func TestResetLinkPermVerdictsMatchStatWithoutARename(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	root, crw := resetLinkPermCRW(t)
	denied := filepath.Join(crw, "keep", "denied")
	if err := os.Mkdir(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "file"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(crw, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	observed, err := parent.Lstat(".crw")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetPin(parent, ".crw", observed)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	targets := []string{
		".", "keep", "keep/.", "keep/sub/..", "keep/sub/file", "missing",
		"loop", "keep/denied/file", "keep/denied/..", "keep/../.", "keep/./..",
		filepath.Join(crw, "keep", "inner.txt"), "../../outside.json",
	}
	for _, target := range targets {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			link := filepath.Join(crw, "alias")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(link) }()
			_, oracleErr := os.Stat(link)
			got, err := resetLinkTargetExists(pinned, "alias")
			if err != nil {
				t.Fatalf("resetLinkTargetExists(%s): %v", target, err)
			}
			if got != (oracleErr == nil) {
				t.Errorf("exists = %v, but os.Stat on the same link answers %v", got, oracleErr)
			}
		})
	}
}

// TestResetLinkPermAnUnreadableTargetIsAbsentNotARefusal: a candidate link whose target cannot be
// read through the pinned descriptor is absent, not a refusal, and the root-path judgement is not
// reached for it — a renamed pinned directory would turn that judgement into an error and stop the
// reset, and it opens the directories the judgement must not open. The kernel cannot resolve such a
// link either, so absent is the oracle's answer and the link is kept.
func TestResetLinkPermAnUnreadableCandidateIsAbsentAndTheRemovalContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not deny search to root")
	}
	// The reset pins the sessions directory first and judges its candidates after, so a directory
	// that loses search permission in between reaches a candidate the descriptor cannot lstat. Such a
	// candidate must be recorded absent — the kernel cannot resolve the name either, which is the
	// oracle's answer, and the link is kept — and the removal must carry on to the candidates after
	// it instead of ending the whole reset. The removal caller is exercised directly with the pin
	// taken first, which is the order the production code uses.
	root := t.TempDir()
	crw := filepath.Join(root, ".crw")
	sessions := filepath.Join(crw, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "a.json"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "b.json"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	observed, err := parent.Lstat(".crw")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetPin(parent, ".crw", observed)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	info, err := pinned.Lstat("sessions")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := resetPin(pinned.Root, "sessions", info)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	if err := os.Chmod(sessions, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessions, 0o755) })
	result := ResetResult{Removed: []string{}, Absent: []string{}}
	if err := resetRmIfExists(inner, "a.json", "a.json", &result); err != nil {
		t.Fatalf("resetRmIfExists: %v (an unreadable candidate must be absent, not a refusal)", err)
	}
	if !slices.Equal(result.Absent, []string{"a.json"}) {
		t.Errorf("absent = %v, want [a.json]", result.Absent)
	}
	// The candidate must still be there: restore search so the check can read the directory again.
	if err := os.Chmod(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(sessions, "a.json")); err != nil {
		t.Errorf("the candidate must be kept: %v", err)
	}
	// The candidates after it must still be judged and removed.
	if err := resetRmIfExists(inner, "b.json", "b.json", &result); err != nil {
		t.Fatalf("b.json: %v (the removal must continue)", err)
	}
	if !slices.Equal(result.Removed, []string{"b.json"}) {
		t.Errorf("removed = %v, want [b.json]", result.Removed)
	}
}

// TestResetLinkPermReadsALongTargetWhole: readlinkat answers a target of 128 bytes or more by
// filling the buffer and reporting its size, not by failing, so a read that stopped at the first
// buffer would judge a cut-off path — and either direction of the verdict is wrong. A live target
// longer than that buffer must be judged present, and a dangling one whose first buffer's worth of
// bytes happens to name a real file must be judged absent; both are compared with os.Stat on the
// same link.
func TestResetLinkPermReadsALongTargetWhole(t *testing.T) {
	// The name is one byte longer than the first buffer, so a read that stopped there is a proper
	// prefix of it: the live case then names a missing file and the dangling case names a real one.
	prefix := strings.Repeat("n", 128)
	for _, tc := range []struct {
		name, target string
		exists       bool
	}{
		{"live_target", prefix + "aaa.txt", true},
		{"dangling_target_whose_prefix_is_a_file", prefix + "bbb.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			crw := filepath.Join(root, ".crw")
			sessions := filepath.Join(crw, "sessions")
			if err := os.MkdirAll(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			// A cut-off read stops at the prefix, so the live target needs that prefix to be missing
			// (the truncated read then answers absent where the whole target exists) and the dangling
			// one needs it to be a real file (the truncated read then answers present where the whole
			// target does not exist).
			if tc.exists {
				if err := os.WriteFile(filepath.Join(sessions, tc.target), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(sessions, prefix), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(sessions, "a.json")); err != nil {
				t.Fatal(err)
			}
			parent, err := os.OpenRoot(crw)
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
			_, oracleErr := os.Stat(filepath.Join(sessions, "a.json"))
			if (oracleErr == nil) != tc.exists {
				t.Fatalf("the control must resolve as %v: %v", tc.exists, oracleErr)
			}
			got, err := resetLinkTargetExists(pinned, "a.json")
			if err != nil {
				t.Fatalf("resetLinkTargetExists: %v", err)
			}
			if got != (oracleErr == nil) {
				t.Errorf("exists = %v, but os.Stat on the same link answers %v", got, oracleErr)
			}
		})
	}
}

// TestResetLinkPermALongChainIsDecidedInsideTheWalk: the walk resolves a target through one
// concatenated pathname, and the kernel applies its own limit to a single pathname
// (ENAMETOOLONG). A chain whose cumulative prefix crosses that limit is one the walk cannot
// decide, so A3's rule for an in-root lstat error applies and the walk answers absent, which keeps
// the link. The judgement must decide it there and must not hand it to the root-path judgement:
// that would open the directories A2 forbids, and a renamed pinned directory would turn the
// verdict into a reset-wide refusal instead of an absent target.
func TestResetLinkPermALongChainIsDecidedInsideTheWalk(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, ".crw", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nested directories, each name long enough that the cumulative pathname crosses the limit; the
	// nesting is built with relative steps so no single mkdir crosses it.
	const depth = 18
	name := strings.Repeat("d", 240)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sessions); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	for i := 0; i < depth; i++ {
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatalf("mkdir depth %d: %v", i, err)
		}
		// next -> <name>/next, so each link target is short and the kernel resolves the chain step by
		// step.
		if err := os.Symlink(name+"/next", "next"); err != nil {
			t.Fatalf("symlink depth %d: %v", i, err)
		}
		if err := os.Chdir(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("keep.txt", "next"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("keep.txt", []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sessions); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("next", "a.json"); err != nil {
		t.Fatal(err)
	}
	// The kernel resolves the short link name component by component and reaches the file, while the
	// walk cannot express that target in one pathname: this divergence keeps the link, the direction
	// the defect file records, and the control below pins that the kernel really does resolve it.
	if _, err := os.Stat(filepath.Join(sessions, "a.json")); err != nil {
		t.Fatalf("the control must resolve through the kernel: %v", err)
	}
	crw := filepath.Join(root, ".crw")
	parent, err := os.OpenRoot(crw)
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
	// The verdict must be the walk's own absent, the descriptor stat must not be asked for an in-root
	// target, and a renamed pinned directory must not turn the answer into a refusal.
	calls := 0
	stat := func(name string) (os.FileInfo, error) {
		calls++
		return pinned.Stat(name)
	}
	got, err := resetLinkTargetExistsWith(pinned, "a.json", stat)
	if err != nil {
		t.Fatalf("resetLinkTargetExists: %v (a target inside the root must be absent or present, not a refusal)", err)
	}
	if got {
		t.Errorf("exists = true, want false: the walk cannot express this target in one pathname and answers absent")
	}
	if calls != 0 {
		t.Errorf("root stat calls = %d, want 0: the target must not reach the root-path judgement", calls)
	}
	if err := os.Rename(sessions, filepath.Join(root, ".crw", "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	again, err := resetLinkTargetExistsWith(pinned, "a.json", stat)
	if err != nil {
		t.Fatalf("resetLinkTargetExists after the rename: %v (it must not refuse)", err)
	}
	if again != got {
		t.Errorf("after the rename exists = %v, want %v", again, got)
	}
	if calls != 0 {
		t.Errorf("root stat calls = %d after the rename, want 0", calls)
	}
}

// TestResetLinkPermALongChainToAnOutOfRootTargetKeepsTheLink: the length case must not be answered
// by following the candidate's own chain, because a following stat of the leaf reports the target of
// a chain that really leaves the root, and the judgement would then remove a link whose out-of-root
// target the contract refuses to act on once the pinned directory was renamed. The walk cannot
// express this target in one pathname, so A3's rule applies and the verdict is absent: the link is
// kept, and the root-path judgement is never reached.
func TestResetLinkPermALongChainToAnOutOfRootTargetKeepsTheLink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(root, ".crw", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	const depth = 18
	name := strings.Repeat("d", 240)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sessions); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	for i := 0; i < depth; i++ {
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatalf("mkdir depth %d: %v", i, err)
		}
		if err := os.Symlink(name+"/next", "next"); err != nil {
			t.Fatalf("symlink depth %d: %v", i, err)
		}
		if err := os.Chdir(name); err != nil {
			t.Fatal(err)
		}
	}
	// The deepest link leaves the root.
	if err := os.Symlink(outside, "next"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sessions); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("next", "a.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sessions, "a.json")); err != nil {
		t.Fatalf("the control must resolve through the kernel: %v", err)
	}
	crw := filepath.Join(root, ".crw")
	parent, err := os.OpenRoot(crw)
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
	calls := 0
	stat := func(name string) (os.FileInfo, error) {
		calls++
		return pinned.Stat(name)
	}
	if err := os.Rename(sessions, filepath.Join(root, ".crw", "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resetLinkTargetExistsWith(pinned, "a.json", stat)
	if err != nil {
		t.Fatalf("resetLinkTargetExists: %v (a renamed pin must not turn this into a refusal)", err)
	}
	if got {
		t.Errorf("exists = true, want false: the link must be kept, not removed for an out-of-root target")
	}
	if calls != 0 {
		t.Errorf("root stat calls = %d, want 0: the length case must be decided by the walk", calls)
	}
}
