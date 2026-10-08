//go:build dev

package cxcfuzz

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// fsEntry is one entry of an input's "fs" array.
func fsEntry(path, kind, content, target string, mode int) pyjson.Object {
	entry := pyjson.Object{{Key: "path", Value: path}, {Key: "kind", Value: kind}}
	if content != "" {
		entry = entry.Set("content", content)
	}
	if target != "" {
		entry = entry.Set("target", target)
	}
	if mode != 0 {
		entry = entry.Set("mode", mode)
	}
	return entry
}

// fsInput is an input whose "fs" array is the given entries.
func fsInput(entries ...pyjson.Object) pyjson.Object {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return pyjson.Object{{Key: "fs", Value: list}}
}

// tree is every path under root with its kind, so two materialised roots can be compared.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		kind := "file"
		switch {
		case info.IsDir():
			kind = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			kind = "symlink"
		}
		found = append(found, kind+" "+rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	return found
}

// The same input builds the same tree in the two roots a case gets.
func TestScenariosBuildTheSameTreeInBothRoots(t *testing.T) {
	input := fsInput(
		fsEntry("dir", "dir", "", "", 0o755),
		fsEntry("dir/file.txt", "file", "hello", "", 0o644),
		fsEntry("link", "symlink", "", "dir/file.txt", 0),
	)
	one, two := t.TempDir(), t.TempDir()
	if n, err := Scenarios(one, input); err != nil || n != 3 {
		t.Fatalf("one: %d entries, %v", n, err)
	}
	if n, err := Scenarios(two, input); err != nil || n != 3 {
		t.Fatalf("two: %d entries, %v", n, err)
	}
	if a, b := tree(t, one), tree(t, two); !reflect.DeepEqual(a, b) {
		t.Fatalf("%v != %v", a, b)
	}
}

// A path outside the root is refused and the case root is removed, so nothing is materialised.
func TestScenariosRefuseAPathOutsideTheRoot(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../escape", "a/../../escape"} {
		base := t.TempDir()
		root := filepath.Join(base, "case")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Scenarios(root, fsInput(fsEntry(path, "file", "x", "", 0o644))); err == nil {
			t.Fatalf("%q was materialised", path)
		}
		if err := emptyBase(base); err != nil {
			t.Fatalf("%q: %v", path, err)
		}
	}
}

// emptyBase checks that a refusal removed the case root and created nothing beside it: the base holds
// no entry at all, so nothing the input named stands outside the case root.
func emptyBase(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("the refusal left %v under %s", entries, base)
	}
	return nil
}

// A symlink target outside the root is refused.
func TestScenariosRefuseASymlinkTargetOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := Scenarios(root, fsInput(fsEntry("link", "symlink", "", "../outside", 0))); err == nil {
		t.Fatal("an outside symlink target was materialised")
	}
	if _, err := Scenarios(root, fsInput(fsEntry("link", "symlink", "", "/etc/passwd", 0))); err == nil {
		t.Fatal("an absolute symlink target was materialised")
	}
}

// A path that resolves through a symlink standing under the root is refused too: the lexical check
// alone would let the write land outside it.
func TestScenariosRefuseAPathThatResolvesThroughAnOutsideSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scenarios(root, fsInput(fsEntry("escape/planted", "file", "x", "", 0o644))); err == nil {
		t.Fatal("a write through an outside symlink was materialised")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); err == nil {
		t.Fatal("the write reached outside the case root")
	}
}

// Generated content is written, never executed: a file holding a shell command leaves the
// command's effect undone.
func TestScenarioContentIsNeverExecuted(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "marker")
	if _, err := Scenarios(root, fsInput(fsEntry("run.sh", "file", "touch "+marker+"\n", "", 0o755))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the generated command ran")
	}
	raw, err := os.ReadFile(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), marker) {
		t.Fatal("the command text was not written as content")
	}
}

// ${ROOT} stands for one case's own root, so two roots compare equal.
func TestRootReplacement(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	if got := ReplaceRoot("wrote "+one+"/file", one); got != "wrote ${ROOT}/file" {
		t.Fatalf("%q", got)
	}
	if got := ReplaceRoot(two+"/file", one); got != two+"/file" {
		t.Fatalf("%q", got)
	}
	value := pyjson.Object{{Key: "path", Value: one + "/a"}, {Key: "list", Value: []any{one + "/b"}}}
	stripped := stripRoot(value, one)
	if got := canonical(stripped); got != `{"list": ["${ROOT}/b"], "path": "${ROOT}/a"}` {
		t.Fatalf("stripped %s", got)
	}
	keyed := pyjson.Object{{Key: one + "/a", Value: "x"}}
	if got := canonical(stripRoot(keyed, one)); got != `{"${ROOT}/a": "x"}` {
		t.Fatalf("keyed %s", got)
	}
}

// c3 (CRW-857): a symlink target that starts with the ROOT placeholder takes the case root, is
// checked for containment and is created with the substituted absolute target. Red first: the
// builder refused every absolute target, so no absolute link target could ever be generated.
func TestScenariosBuildAnAbsoluteRootTarget(t *testing.T) {
	root := t.TempDir()
	input := fsInput(
		fsEntry("codex-home/memories", "dir", "", "", 0o755),
		fsEntry("work/abs", "symlink", "", rootPlaceholder+"/codex-home/memories", 0),
	)
	if n, err := Scenarios(root, input); err != nil || n != 2 {
		t.Fatalf("%d entries, %v", n, err)
	}
	link := filepath.Join(root, "work", "abs")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "codex-home", "memories"); got != want {
		t.Fatalf("the link target is %q, want %q", got, want)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "codex-home", "memories"); resolved != want {
		t.Fatalf("the link resolves to %q, want %q", resolved, want)
	}
}

// c3 (CRW-857): the placeholder is no licence to leave the case root. An absolute target outside
// it, a placeholder target that walks out with .., and a plain relative escape are all still
// refused, and nothing is materialised.
func TestScenariosStillRefuseATargetOutsideTheRoot(t *testing.T) {
	// The link stands at work/abs, so a relative escape has to climb two levels: one "../" from
	// work/abs still lands inside the case root and is legitimately allowed.
	for _, target := range []string{"/etc/passwd", rootPlaceholder + "/../outside", rootPlaceholder + "/../escape", "../../outside"} {
		base := t.TempDir()
		root := filepath.Join(base, "case")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Scenarios(root, fsInput(fsEntry("work/abs", "symlink", "", target, 0))); err == nil {
			t.Fatalf("the target %q was materialised", target)
		}
		if err := emptyBase(base); err != nil {
			t.Fatalf("the target %q: %v", target, err)
		}
	}
}

// caseRoot makes a case root under a base of its own, so a test can prove that a refusal left nothing
// outside it: an escape would land in the base or above it.
func caseRoot(t *testing.T) (base, root string) {
	t.Helper()
	base = t.TempDir()
	root = filepath.Join(base, "case")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return base, root
}

// c1 (CRW-908): a link is judged against the tree built so far, so a later entry cannot walk out through
// an earlier entry's link. {a -> .} resolves to the case root, and {b -> a/../escape} then applies its
// '..' after that link has been followed, one level above the root. Red first: the old check cleaned the
// target lexically - turning a/../escape into escape, inside the root - and saw every entry before any
// was built, so both were allowed and the kernel resolved b outside the case root.
func TestScenariosRefuseALinkThatEscapesThroughAnEarlierLink(t *testing.T) {
	base, root := caseRoot(t)
	input := fsInput(
		fsEntry("a", "symlink", "", ".", 0),
		fsEntry("b", "symlink", "", "a/../escape", 0),
	)
	if _, err := Scenarios(root, input); err == nil {
		t.Fatal("a link that walks out through an earlier link was materialised")
	}
	if err := emptyBase(base); err != nil {
		t.Fatal(err)
	}
}

// c1 (CRW-908): the absolute form of the same escape. {abs -> ${ROOT}/.} resolves to the case root and
// {esc -> abs/../q} applies its '..' after that link, one level above it.
func TestScenariosRefuseAnAbsoluteSelfLinkThenDotDot(t *testing.T) {
	base, root := caseRoot(t)
	input := fsInput(
		fsEntry("abs", "symlink", "", rootPlaceholder+"/.", 0),
		fsEntry("esc", "symlink", "", "abs/../q", 0),
	)
	if _, err := Scenarios(root, input); err == nil {
		t.Fatal("an absolute self link followed by '..' was materialised")
	}
	if err := emptyBase(base); err != nil {
		t.Fatal(err)
	}
}

// c1 (CRW-908): the controls stay allowed. A relative link into the case tree, an absolute
// ROOT-prefixed link, and the 45-link chain the memorygate generator builds all still resolve inside
// the case root.
func TestScenariosStillAllowTheConfinedControls(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	const chain = 45
	entries := []pyjson.Object{
		fsEntry("codex-home/memories", "dir", "", "", 0o755),
		fsEntry("work/link", "symlink", "", "../codex-home/memories", 0),
		fsEntry("work/abs", "symlink", "", rootPlaceholder+"/codex-home/memories", 0),
	}
	for i := 0; i < chain; i++ {
		target := "c" + strconv.Itoa(i+1)
		if i == chain-1 {
			target = "../codex-home/memories"
		}
		entries = append(entries, fsEntry("work/c"+strconv.Itoa(i), "symlink", "", target, 0))
	}
	if n, err := Scenarios(root, fsInput(entries...)); err != nil || n != len(entries) {
		t.Fatalf("%d entries, %v", n, err)
	}
	for _, link := range []string{"work/link", "work/abs", "work/c0", "work/c44"} {
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, link))
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if want := filepath.Join(root, "codex-home", "memories"); resolved != want {
			t.Fatalf("%s resolves to %q, want %q", link, resolved, want)
		}
	}
}

// c7 d1 (CRW-908 generation 2): a refused build removes the root for certain, even when an entry gave
// a directory a mode that keeps it from being listed. The removal first makes every directory under the
// root owner-readable, writable and searchable - walked without following links - and only then removes
// the root, so a sealed directory cannot keep an escaping link on disk. Red first on the generation-1
// head: the removal error was discarded and root/sealed/escape survived the refusal.
func TestScenariosRemoveTheRootOfARefusedBuildWithASealedDirectory(t *testing.T) {
	base, root := caseRoot(t)
	input := fsInput(
		fsEntry("sealed", "dir", "", "", 0o300),
		fsEntry("sealed/escape", "symlink", "", "../../outside", 0),
	)
	// The sealed directory keeps a best-effort cleanup possible whatever the outcome.
	defer func() { _ = removeSealedForTest(base) }()
	if _, err := Scenarios(root, input); err == nil {
		t.Fatal("a link that leaves the case root was materialised")
	}
	if err := emptyBase(base); err != nil {
		t.Fatalf("the refused build left the root behind: %v", err)
	}
}

// c7 d2 (CRW-908 generation 2): the caller-level error preservation is driven through the real
// functions. evaluate answers a refused scenario with errRefused - the case the campaign counts as
// refused - and never reaches the worker pool, so a campaign with no pool exercises the refusal path.
// A refused build whose root survived is answered as its own RemovalError instead, which is the
// distinction the caller must see; the helpers the caller folds with (joinCleanup, refusedOutcome) are
// covered here too.
func TestEvaluateAnswersARefusedScenarioAsRefused(t *testing.T) {
	called := false
	cfg := Config{Target: Target{
		Name:     "refusal",
		Generate: func(rng *rand.Rand, size int) any { return nil },
		Go:       func(input any, env Env) (any, error) { called = true; return nil, nil },
		Compare:  compareJSON,
	}}
	c := &campaign{cfg: cfg}
	refused := fsInput(
		fsEntry("a", "symlink", "", ".", 0),
		fsEntry("b", "symlink", "", "a/../escape", 0),
	)
	_, _, _, err := c.evaluate(refused)
	if !errors.Is(err, errRefused) {
		t.Fatalf("a refused scenario answered %v, want errRefused", err)
	}
	if called {
		t.Fatal("the target's Go function ran for a refused scenario")
	}
}

// c7 d2 (CRW-908 generation 2): CheckCase reports a scenario it refused as a problem, so a caller sees
// a refused case rather than an empty replay result.
func TestCheckCaseReportsARefusedScenario(t *testing.T) {
	target := Target{
		Name:    "refusal",
		Go:      func(input any, env Env) (any, error) { return nil, nil },
		Compare: compareJSON,
	}
	refused := fsInput(
		fsEntry("a", "symlink", "", ".", 0),
		fsEntry("b", "symlink", "", "a/../escape", 0),
	)
	c := Case{Name: "refused", Input: canonical(refused), Tag: TagOpen, Go: "null"}
	if problem := CheckCase(target, c); !strings.Contains(problem, "the scenario is refused") {
		t.Fatalf("CheckCase answered %q, want a refused-scenario problem", problem)
	}
}

// c7 d1 (CRW-908 generation 2): an input the harness cannot read is a refusal of the case, so it removes
// the prepared root the way every other refusal does. Red first: the malformed-fs exits returned before
// the removal, so a root the caller had prepared stayed on the host.
func TestScenariosRemoveTheRootOfAMalformedInput(t *testing.T) {
	for _, c := range []struct {
		name  string
		input any
	}{
		{"fs is not an array", pyjson.Object{{Key: "fs", Value: "not an array"}}},
		{"an entry has a bad mode", fsInput(pyjson.Object{
			{Key: "path", Value: "x"}, {Key: "kind", Value: "file"}, {Key: "mode", Value: "bad"},
		})},
		{"an entry is missing its path", fsInput(pyjson.Object{{Key: "kind", Value: "file"}})},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, root := caseRoot(t)
			if err := PrepareRoot(root); err != nil {
				t.Fatal(err)
			}
			if _, err := Scenarios(root, c.input); err == nil {
				t.Fatal("a malformed input was accepted")
			}
			if err := emptyBase(base); err != nil {
				t.Fatalf("the refused input left the prepared root behind: %v", err)
			}
		})
	}
}

// c7 d2 (CRW-908 generation 2): the campaign classifies a case's error, and the classification is what
// keeps a removal failure from being counted as a refusal or a timeout. Each case is pinned here so a
// reordering that put the refusal check first would fail.
func TestClassifyCaseError(t *testing.T) {
	removal := RemovalError{Root: "/root", Refused: errRefused, Err: errors.New("permission denied")}
	for _, c := range []struct {
		name string
		err  error
		want caseOutcome
	}{
		{"no error", nil, caseOK},
		{"a plain refusal", errRefused, caseRefused},
		{"a worker death", errors.New("the worker died"), caseFailed},
		{"a timeout", Timeout{}, caseFailed},
		{"a removal failure", removal, caseRemovalFailed},
		{"a removal failure beside a refusal", errors.Join(errRefused, removal), caseRemovalFailed},
		{"a removal failure beside a timeout", errors.Join(Timeout{}, removal), caseRemovalFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyCaseError(c.err); got != c.want {
				t.Fatalf("classifyCaseError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// c7 d2 (CRW-908 generation 2): the helpers the caller folds errors with preserve a removal failure,
// and a deferred cleanup failure is wrapped as one, so a root that outlived a run is never read as a
// plain refusal or a transient worker death.
func TestRemovalFailureSurvivesTheCallerHelpers(t *testing.T) {
	root := t.TempDir()
	removal := RemovalError{Root: root, Refused: errors.New("a link left the root"), Err: errors.New("permission denied")}
	if got := refusedOutcome(removal); !errors.As(got, new(RemovalError)) {
		t.Fatalf("refusedOutcome turned a RemovalError into %v", got)
	}
	for _, c := range []struct {
		name  string
		err   error
		clean error
	}{
		{"cleanup alone", nil, removal},
		{"cleanup beside another error", errors.New("a worker died"), removal},
		{"cleanup first", removal, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := joinCleanup(c.err, c.clean); !errors.As(got, new(RemovalError)) {
				t.Fatalf("joinCleanup dropped the removal failure: %v", got)
			}
		})
	}
	if got := joinCleanup(nil, nil); got != nil {
		t.Fatalf("joinCleanup invented an error: %v", got)
	}
	// A deferred cleanup failure is wrapped as a removal failure, so a caller that preserves removal
	// failures preserves it. The base denies the write that removing the child needs; a process that may
	// bypass the mode removes it, which is correct, so the precondition is proved first.
	base := t.TempDir()
	sealed := filepath.Join(base, "sealed")
	if err := os.Mkdir(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(base, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(base, 0o700) }()
	if os.Remove(probe) != nil {
		if err := CleanupCaseRoot(sealed); !errors.As(err, new(RemovalError)) {
			t.Fatalf("a deferred cleanup failure was not a RemovalError: %v", err)
		}
	}
}

// c7 d2 (CRW-908 generation 2): CheckCase folds a cleanup problem into the replay problem instead of
// dropping it, so a case root that outlived replay is reported to the caller.
func TestCheckCaseReportsACleanupProblem(t *testing.T) {
	if got := joinProblem("", "the case root was not removed"); got != "the case root was not removed" {
		t.Fatalf("a cleanup problem alone was %q", got)
	}
	if got := joinProblem("go x, oracle y", "the case root was not removed"); !strings.Contains(got, "go x, oracle y") || !strings.Contains(got, "was not removed") {
		t.Fatalf("a cleanup problem hid or replaced the replay problem: %q", got)
	}
	if got := joinProblem("go x, oracle y", ""); got != "go x, oracle y" {
		t.Fatalf("an empty cleanup problem changed the replay problem: %q", got)
	}
}

// removeSealedForTest makes a test's own base removable again, so a red run does not leave a sealed
// directory behind for the test framework to trip over.
func removeSealedForTest(base string) error {
	_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(base)
}

// c7 d1 (CRW-908 generation 2): a removal that still fails is its own error that names the root and is
// not a refused case. The base denies the write that removing the root itself needs, so the removal
// fails after the helper has already made everything under the root removable; the error must name the
// root and must not be a plain refusal a target may run on. A process that may bypass the base's mode
// (root, or CAP_DAC_OVERRIDE) removes the root anyway, which is the correct outcome, so the test proves
// the precondition first and skips rather than failing where the mode cannot deny the removal.
func TestScenariosRemovalFailureIsItsOwnError(t *testing.T) {
	base, root := caseRoot(t)
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	// The precondition: a directory that the base's mode would keep from being removed. It is made while
	// the base is still writable, so the only question is whether this process may remove it afterwards.
	probe := filepath.Join(base, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(base, 0o700) }()
	privileged := os.Remove(probe) == nil
	_, err := Scenarios(root, fsInput(fsEntry("escape", "symlink", "", "../../outside", 0)))
	if err == nil {
		t.Fatal("a link that leaves the case root was materialised")
	}
	if privileged {
		// The mode cannot deny the removal here, so the root was removed and the confinement error is
		// the whole answer. Only the guarantee that the root is gone is checkable.
		if _, statErr := os.Stat(root); statErr == nil {
			t.Fatalf("a privileged removal left the case root %s behind", root)
		}
		t.Skip("this process may bypass the base's mode, so the removal was not denied")
	}
	var removal RemovalError
	if !errors.As(err, &removal) {
		t.Fatalf("the error is not a RemovalError: %v", err)
	}
	if removal.Root != root {
		t.Fatalf("the removal error names %q, want %q", removal.Root, root)
	}
}

// c1 (CRW-908): a '..' inside a not-yet-existing remainder is refused. The link {a -> nope/../x} names a
// component that does not exist yet, so the kernel cannot apply the '..' to it; the resolution is refused
// rather than left to the kernel to answer differently later. Red first: the old builder stored the raw
// text and cleaned only for its own check, so the entry was accepted.
func TestScenariosRefuseARemainderDotDot(t *testing.T) {
	base, root := caseRoot(t)
	if _, err := Scenarios(root, fsInput(fsEntry("a", "symlink", "", "nope/../x", 0))); err == nil {
		t.Fatal("a '..' behind a component that does not exist yet was materialised")
	}
	if err := emptyBase(base); err != nil {
		t.Fatal(err)
	}
}

// c1 (CRW-908): a link cycle is refused rather than resolved. Red first: the old check cleaned the two
// targets lexically and never followed them, so both links were created and only a later resolution could
// have discovered the cycle.
func TestScenariosRefuseALinkCycle(t *testing.T) {
	base, root := caseRoot(t)
	input := fsInput(
		fsEntry("a", "symlink", "", "b", 0),
		fsEntry("b", "symlink", "", "a", 0),
	)
	if _, err := Scenarios(root, input); err == nil {
		t.Fatal("a link cycle was materialised")
	}
	if err := emptyBase(base); err != nil {
		t.Fatal(err)
	}
}

// c1 (CRW-908): a link k levels deep that points at the case root, followed by k+1 '..', walks above the
// root. The target is written as ${ROOT}/. and not as a bare ${ROOT}: rootSubstitutedPath substitutes
// only a target that opens with the placeholder AND a separator, so a bare placeholder would be stored
// literally and the link would be refused as a dangling relative link rather than as a link to the root.
// The test therefore first proves the link really reaches the root, and only then that the escape is
// refused. Red first: the lexical check saw the '..' collapse against the link's nominal name and stayed
// inside, while the kernel followed the link to the root first and then climbed out of it.
func TestScenariosRefuseADeepLinkToTheRootThenDotDot(t *testing.T) {
	// First prove the shape really points at the case root: the link is built on its own and resolves
	// to the root, so the refusal below is about a link to the root and not about a dangling link.
	_, root := caseRoot(t)
	if _, err := Scenarios(root, fsInput(
		fsEntry("d0", "dir", "", "", 0o755),
		fsEntry("d0/d1", "dir", "", "", 0o755),
		fsEntry("d0/d1/root", "symlink", "", rootPlaceholder+"/.", 0),
	)); err != nil {
		t.Fatalf("the link to the case root was refused: %v", err)
	}
	link := filepath.Join(root, "d0", "d1", "root")
	if resolved, err := filepath.EvalSymlinks(link); err != nil || resolved != root {
		t.Fatalf("the link resolves to %q, %v; want the case root %q", resolved, err, root)
	}
	for _, depth := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			deepest := ""
			entries := make([]pyjson.Object, 0, depth+2)
			for i := 0; i < depth; i++ {
				deepest = filepath.Join(deepest, "d"+strconv.Itoa(i))
				entries = append(entries, fsEntry(deepest, "dir", "", "", 0o755))
			}
			link := filepath.Join(deepest, "root")
			entries = append(entries,
				fsEntry(link, "symlink", "", rootPlaceholder+"/.", 0),
				fsEntry("esc", "symlink", "", link+"/"+strings.Repeat("../", depth+1)+"q", 0),
			)
			// The link alone is built in its own root, so its resolution is proved for this depth before
			// the escaping entry is added.
			_, linkRoot := caseRoot(t)
			if _, err := Scenarios(linkRoot, fsInput(entries[:len(entries)-1]...)); err != nil {
				t.Fatalf("the %d-deep link to the case root was refused: %v", depth, err)
			}
			if resolved, err := filepath.EvalSymlinks(filepath.Join(linkRoot, link)); err != nil || resolved != linkRoot {
				t.Fatalf("the %d-deep link resolves to %q, %v; want the case root %q", depth, resolved, err, linkRoot)
			}
			base, root := caseRoot(t)
			if _, err := Scenarios(root, fsInput(entries...)); err == nil {
				t.Fatalf("a %d-deep link to the root followed by %d '..' was materialised", depth, depth+1)
			}
			if err := emptyBase(base); err != nil {
				t.Fatal(err)
			}
		})
	}
}
