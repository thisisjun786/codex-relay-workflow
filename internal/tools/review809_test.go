package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// review809Host is a temporary host whose temp_root is <home>/tools/downloads with <home>/tools
// already present, which is the shape the race needs: the ancestor exists when createRoot scans and
// is removed before createRoot reaches it.
func review809Host(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	toolsDir := filepath.Join(base, "tools")
	if err := os.Mkdir(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return toolsDir, filepath.Join(toolsDir, "downloads")
}

// review809Seam installs the createRoot mkdir seam for one test and restores the production value
// after it. The seam is package state, so a test that uses it must not run in parallel.
func review809Seam(t *testing.T, seam func(path string)) {
	t.Helper()
	saved := createRootBeforeMkdir
	createRootBeforeMkdir = seam
	t.Cleanup(func() { createRootBeforeMkdir = saved })
}

// C1: an ancestor the scan found is removed by a concurrent install before this call reaches it.
// createRoot recomputes the missing components and continues, recording both the re-made ancestor
// and the target as this call own, so its cleanup leaves neither behind.
func TestToolsReview809AncestorRemovedDuringCreate(t *testing.T) {
	toolsDir, tempRoot := review809Host(t)

	removed := false
	review809Seam(t, func(path string) {
		if path != tempRoot || removed {
			return
		}
		removed = true
		if err := os.Remove(toolsDir); err != nil {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(tempRoot)
	if err != nil {
		t.Fatalf("createRoot with an ancestor removed between its scan and its mkdir: %v", err)
	}
	// Both the re-made ancestor and the target are this call's. The ancestor may be named more than
	// once, because it was made once before it vanished and once again after; what matters is that
	// every entry is one of the two and that the ancestor comes before the target, which is the order
	// removeCreated's reverse walk needs.
	first, last := -1, -1
	for i, path := range created {
		switch path {
		case toolsDir:
			if first < 0 {
				first = i
			}
		case tempRoot:
			last = i
		default:
			t.Fatalf("createRoot recorded %q, which is neither the ancestor nor the target", path)
		}
	}
	if first < 0 || last < 0 {
		t.Fatalf("createRoot recorded %v, want both the ancestor and the target", created)
	}
	if first > last {
		t.Fatalf("createRoot recorded %v, want the ancestor before the target", created)
	}
	removeCreated(created)
	for _, path := range []string{toolsDir, tempRoot} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s survived the cleanup: %v", path, statErr)
		}
	}
}

// C2: the same removed ancestor must not turn a valid install into a host failure. The whole
// command exits 0 and leaves a complete install.
func TestToolsReview809InstallSurvivesARemovedAncestor(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	toolsRoot := filepath.Join(tree.home, "absent", "tools")
	tempRoot := filepath.Join(toolsRoot, "downloads")
	tree = review754ConfigTree(t, map[string]string{"tools_root": toolsRoot, "temp_root": tempRoot})
	if err := os.MkdirAll(toolsRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	removed := false
	review809Seam(t, func(path string) {
		if path != tempRoot || removed {
			return
		}
		removed = true
		if err := os.Remove(toolsRoot); err != nil {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	code, out, errOut := runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
	if code != 0 || errOut != "" || out != pin.ExecutablePath(toolsRoot)+"\n" {
		t.Fatalf("install with an ancestor removed: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if _, err := installedPath(pin, toolsRoot); err != nil {
		t.Fatalf("the install is not readable as an install: %v", err)
	}
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Errorf("the download root was left behind: %v", err)
	}
}

// C3: the recompute is bounded. A seam that removes the ancestor every time must end in the
// not-exist error rather than an endless walk, and createRoot must remove what it made first.
func TestToolsReview809GivesUpAfterThreeRounds(t *testing.T) {
	toolsDir, tempRoot := review809Host(t)

	attempts := 0
	review809Seam(t, func(path string) {
		if path != tempRoot {
			return
		}
		attempts++
		if err := os.Remove(toolsDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(tempRoot)
	if err == nil {
		t.Fatalf("createRoot answered success with %v while the ancestor kept vanishing", created)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("createRoot answered %v, want the not-exist error it gave up on", err)
	}
	// The initial walk plus at most three recomputes: the bound is what keeps a root that keeps
	// vanishing from spinning.
	if attempts > 4 {
		t.Errorf("createRoot reached the target mkdir %d times, want at most 4", attempts)
	}
	for _, path := range []string{toolsDir, tempRoot} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s was left behind after createRoot gave up: %v", path, statErr)
		}
	}
}

// C1, ordering side: a component this call already made can vanish together with its ancestor, and
// the retry then makes the ancestor again before that component. The record must keep the ancestor
// before the component, or removeCreated walks the list backwards, tries the ancestor while it is
// still non-empty, and leaves it behind.
func TestToolsReview809RecreatedAncestorIsRemovedInOrder(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(parent, "child")
	leaf := filepath.Join(child, "leaf")

	removed := false
	review809Seam(t, func(path string) {
		if path != leaf || removed {
			return
		}
		removed = true
		// The concurrent install takes back the ancestor and the component this call already made.
		if err := os.RemoveAll(parent); err != nil {
			t.Errorf("the seam could not remove the ancestor: %v", err)
		}
	})

	created, err := createRoot(leaf)
	if err != nil {
		t.Fatalf("createRoot with a made ancestor removed: %v", err)
	}
	removeCreated(created)
	for _, path := range []string{parent, child, leaf} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("%s survived the cleanup of %v: %v", path, created, statErr)
		}
	}
}

// C1, ownership side: a path this call made can be removed by a concurrent install and made again
// by it before this call's next mkdir. That mkdir then answers EEXIST, so the directory standing
// at the path is not this call's and must not be removed as if it were.
func TestToolsReview809KeepsAnotherCallsDirectory(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "a")
	target := filepath.Join(ancestor, "b")

	phase := 0
	review809Seam(t, func(path string) {
		switch {
		case path == target && phase == 0:
			// The concurrent install takes its ancestor back before this call's mkdir reaches the
			// target, which is what makes this call recompute.
			phase = 1
			if err := os.Remove(ancestor); err != nil {
				t.Errorf("the seam could not remove the ancestor: %v", err)
			}
		case path == ancestor && phase == 1:
			// The concurrent install then makes that path again, so this call's mkdir answers
			// EEXIST and the directory standing there belongs to the other call.
			phase = 2
			if err := os.Mkdir(ancestor, 0o755); err != nil {
				t.Errorf("the seam could not make the ancestor again: %v", err)
			}
		}
	})

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot with a path another install took over: %v", err)
	}
	removeCreated(created)
	if info, statErr := os.Stat(ancestor); statErr != nil || !info.IsDir() {
		t.Fatalf("this call removed the directory the other install made: %v", statErr)
	}
}
