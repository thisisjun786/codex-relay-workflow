//go:build dev

package cxcfuzz

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// A case root made twice over, or made over a tree that already holds some of its homes and files, is the same root
// afterwards, and what was in it stays (CRW-1066: PrepareRoot judges the root once and makes the homes as plain
// directories, so what it leaves behind must not change).
func TestPrepareRootAcceptsAnExistingTreeAndKeepsItsFiles(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	if err := os.MkdirAll(env.CrwHome, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(env.CrwHome, "keep.txt")
	if err := os.WriteFile(keep, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := PrepareRoot(root); err != nil {
			t.Fatalf("PrepareRoot #%d: %v", i, err)
		}
	}
	if raw, err := os.ReadFile(keep); err != nil || string(raw) != "kept" {
		t.Fatalf("keep.txt: %q, %v", raw, err)
	}
	for _, dir := range []string{env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			t.Errorf("%s: %v %v", dir, info, err)
		}
	}
}

// A home that is a plain file in the way is an error and no directory takes its place, as when MkdirAll made it.
func TestPrepareRootRefusesAHomeThatIsAFile(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	if err := os.WriteFile(env.Home, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(root); err == nil {
		t.Fatal("PrepareRoot made a home over a file")
	}
	if info, err := os.Lstat(env.Home); err != nil || info.IsDir() {
		t.Fatalf("the file was replaced: %v %v", info, err)
	}
}

// A case root that holds a directory nothing can list, with a link inside it that leaves the root, is removed whole
// and the link's target is not touched (CRW-1066: the removal tries the plain way first and only then makes the
// directories listable).
func TestRemoveCaseRootRemovesAnUnlistableDirectoryAndLeavesALinksTarget(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "case")
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(root, "home", "sealed")
	if err := os.MkdirAll(filepath.Join(sealed, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sealed, "inner", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	if err := RemoveCaseRoot(root); err != nil {
		t.Fatalf("RemoveCaseRoot: %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the root survived: %v", err)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "outside" {
		t.Fatalf("the link's target: %q, %v", raw, err)
	}
}

// prepareRootEachHome is how PrepareRoot made a root before CRW-1066: the guard judges the root and then each home.
func prepareRootEachHome(root string) error {
	if err := homeguard.Refuse(root); err != nil {
		return err
	}
	env := RootEnv(root)
	for _, dir := range []string{root, env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if err := homeguard.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// listing is every path below root, relative, with its kind and permission bits, in walk order.
func listing(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, rel+" "+info.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// On a fixed seed the state target gives the same canonical answer, and leaves the same tree, in a root made by
// PrepareRoot as in one made by the earlier home-by-home preparation, and the root is then removed by RemoveCaseRoot
// either way (CRW-1066 c2: the faster root preparation changes no answer).
func TestStateCasesAnswerTheSameInARootPreparedEitherWay(t *testing.T) {
	rng := rand.New(rand.NewSource(711))
	base := t.TempDir()
	for i := 0; i < 120; i++ {
		input, err := decode(canonical(stateGenerate(rng, rng.Int())))
		if err != nil {
			t.Fatal(err)
		}
		var answers, trees [2]string
		for side, prepare := range []func(string) error{PrepareRoot, prepareRootEachHome} {
			root := filepath.Join(base, fmt.Sprintf("case-%d-%d", i, side))
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := prepare(root); err != nil {
				t.Fatal(err)
			}
			if _, err := Scenarios(root, input); err != nil {
				t.Fatalf("case %d side %d: %v", i, side, err)
			}
			answer, err := stateGo(input, RootEnv(root))
			if err != nil {
				t.Fatal(err)
			}
			answers[side] = canonical(stripRoot(answer, root))
			trees[side] = strings.Join(listing(t, root), "\n")
			if err := RemoveCaseRoot(root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("case %d side %d: the root survived: %v", i, side, err)
			}
		}
		if answers[0] != answers[1] {
			t.Fatalf("case %d: the answers differ\n new %.300s\n old %.300s", i, answers[0], answers[1])
		}
		if trees[0] != trees[1] {
			t.Fatalf("case %d: the trees differ\n new %s\n old %s", i, trees[0], trees[1])
		}
	}
}
