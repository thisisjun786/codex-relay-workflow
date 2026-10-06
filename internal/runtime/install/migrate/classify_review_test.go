package migrate

// classify_review_test.go holds the red-first cases of CRW-677: the four review findings of the M2b state-copy
// classifier (internal/runtime/install/migrate/classify.go) that the CRW-655 correction and CRW-672 do not cover.
// Every case builds synthetic roots only; isolate puts HOME, CODEX_HOME, CRW_HOME and TMPDIR in temporary
// directories, so nothing here reads or writes the real ~/.codex, ~/.crw or ~/.codexclaw.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestClassifyReviewDestinationRootTemp: the mapped project destination root is registered for the destination
// temporary scan even when the source root has no mapped children, so an abandoned older-run temporary there is
// reported (Devin yellow and Codex P2 at line 92 of the classifier pull request).
func TestClassifyReviewDestinationRootTemp(t *testing.T) {
	ws, _, dst := invRowProject(t)
	invTree(t, dst, map[string]string{invOldTemp: "left over by an interrupted run"})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	it := invWant(t, p, ScopeProject, "", DispSkip)
	if it.Destination != invOldTemp || it.Reason != inventoryReasonOldTemp {
		t.Errorf("destination-root temporary = %+v, want destination %q reason %q", it, invOldTemp, inventoryReasonOldTemp)
	}
	if _, err := os.Lstat(filepath.Join(dst, invOldTemp)); err != nil {
		t.Errorf("the older-run temporary was touched: %v", err)
	}
}

// TestClassifyReviewSkippedContainerLink: a container that holds only skip rows is not registered as a destination
// directory, so a link at its destination no longer refuses the whole preflight (Devin yellow and Codex P2 at line
// 305). The user source here holds only the executable-cache rows below runtime/ast-grep.
func TestClassifyReviewSkippedContainerLink(t *testing.T) {
	base := isolate(t)
	src, dst := filepath.Join(base, "eu"), filepath.Join(base, "ev")
	invTree(t, src, map[string]string{"runtime/ast-grep/linux-x64/sg": "the executable installation cache"})
	mkdirs(t, dst)
	invLink(t, filepath.Join(dst, "runtime"), filepath.Join(base, "elsewhere"))
	p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: src, ToHome: dst})
	must(t, err)
	invWant(t, p, ScopeUser, "runtime/ast-grep/linux-x64/sg", DispSkip)
}

// TestClassifyReviewExcludedContainerFile: a container that holds only excluded rows is not registered as a
// destination directory, so a regular file where the source holds that container no longer refuses the preflight
// with ReasonNotDirectory (pair evaluation of the classifier pull request, P1 5).
func TestClassifyReviewExcludedContainerFile(t *testing.T) {
	ws, src, dst := invRowProject(t)
	invTree(t, src, map[string]string{"cache/x": "derived diskcache data"})
	invTree(t, dst, map[string]string{"cache": "a regular file where the source holds a container"})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	invWant(t, p, ScopeProject, "cache", DispSkip)
}

// TestClassifyReviewDestTempsClosesHandles: classifyDestTemps closes every parent handle classifyOpenDest opened
// when the last destination directory is absent, so repeated scans leave the process's open descriptor count
// unchanged (Devin yellow at line 391). The count is read from /proc/self/fd, so the case is Linux-only.
func TestClassifyReviewDestTempsClosesHandles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the open descriptor count is read from /proc/self/fd")
	}
	ws, src, dst := invRowProject(t)
	invTree(t, src, map[string]string{"goalplans/rec-1/goalplan.json": "{}"})
	mkdirs(t, filepath.Join(dst, "goalplans")) // the last directory of the goalplans destination path stays absent
	classifyOnce := func() {
		t.Helper()
		r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
		must(t, err)
		if _, err := classify(r); err != nil {
			_ = r.Close()
			t.Fatalf("classify = %v", err)
		}
		must(t, r.Close())
	}
	classifyOnce() // warm up the descriptors the runtime itself holds
	before := classifyReviewOpenDescriptors(t)
	for i := 0; i < 16; i++ {
		classifyOnce()
	}
	if after := classifyReviewOpenDescriptors(t); after != before {
		t.Errorf("open descriptors grew from %d to %d: classifyDestTemps leaks the parents it opened", before, after)
	}
}

func classifyReviewOpenDescriptors(t *testing.T) int {
	t.Helper()
	names, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd is not readable: %v", err)
	}
	return len(names)
}

// TestClassifyReviewAbsentUserSourceFallback: a selected user pair whose source root is absent still enumerates the
// literal ~/.codexclaw fallback service files and reports them excluded, and nothing is copied from the absent
// source (Codex P2 at line 51).
func TestClassifyReviewAbsentUserSourceFallback(t *testing.T) {
	base := isolate(t)
	home := os.Getenv("HOME")
	invTree(t, filepath.Join(home, ProjectSourceName), map[string]string{"serve.out.log": "the messenger service log"})
	p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: filepath.Join(base, "absent-user"), ToHome: filepath.Join(base, "ev")})
	must(t, err)
	it := invWant(t, p, ScopeUser, filepath.Join(home, ProjectSourceName, "serve.out.log"), DispSkip)
	if it.Reason != inventoryReasonFallback {
		t.Errorf("fallback reason = %q, want %q", it.Reason, inventoryReasonFallback)
	}
	for _, it := range p.Items {
		if it.Scope == ScopeUser && it.Source == "." {
			t.Errorf("the absent user source produced a copy item: %+v", it)
		}
	}
}
