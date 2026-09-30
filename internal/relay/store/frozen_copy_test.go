package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// frozenSource is one artifact under a root of its own and its manifest entry.
func frozenSource(t *testing.T, text string) (string, []ManifestEntry) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "deliver.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := BuildManifest([]string{path}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	return path, entries
}

func requireNoManifest(t *testing.T, destination string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(destination, "MANIFEST.json")); !os.IsNotExist(err) {
		t.Fatalf("a MANIFEST.json was published over bytes that were not confirmed: %v", err)
	}
}

// manifest.freeze checks the source's stat after copying it, as every authorized read does. Only
// the source's timestamps move here, so its bytes still hash to the digest and the re-hash after
// the copy would accept them: the refusal is the stability check's alone.
func TestFreezeRefusesASourceThatChangesDuringTheCopy(t *testing.T) {
	source, entries := frozenSource(t, "the delivered bytes")
	destination := filepath.Join(t.TempDir(), "frozen")
	afterFrozenCopy = func(path string) {
		if path != source {
			t.Errorf("the seam ran for %s", path)
		}
		old := time.Unix(0, 0)
		if err := os.Chtimes(source, old, old); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterFrozenCopy = nil })
	requireReason(t, FreezeManifest(entries, destination), ReasonArtifactMutated)
	requireNoManifest(t, destination)
}

// manifest.freeze re-reads every blob before it publishes a manifest over it. Bytes already
// stored under the digest's name are not trusted for having the name: the re-hash refuses them,
// and no MANIFEST.json claims them.
func TestFreezeRehashesABlobItFindsUnderTheDigestName(t *testing.T) {
	_, entries := frozenSource(t, "the delivered bytes")
	destination := filepath.Join(t.TempDir(), "frozen")
	files := filepath.Join(destination, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files, entries[0].SHA256), []byte("not those bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := FreezeManifest(entries, destination)
	requireReason(t, err, ReasonManifestUnverified)
	if !strings.Contains(err.Error(), "frozen copy of "+PythonRepr(entries[0].Path)+" hashes to ") {
		t.Fatalf("refused for another reason: %v", err)
	}
	requireNoManifest(t, destination)
}

// A frozen copy is written and verified where the fence writes and verifies it: a destination
// reached through a symlinked directory, or named relative to the working directory, is the
// place Path.resolve() names, and the pinned walk runs from there.
func TestFreezeAndVerifyResolveTheDestinationAsTheFenceDoes(t *testing.T) {
	_, entries := frozenSource(t, "the delivered bytes")
	t.Run("symlinked", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
			t.Fatal(err)
		}
		reference := filepath.Join(base, "link", "frozen")
		if err := FreezeManifest(entries, reference); err != nil {
			t.Fatalf("freeze through a symlinked directory: %v", err)
		}
		if problems, err := VerifyFrozen(reference, entries); err != nil || len(problems) != 0 {
			t.Fatalf("verify through a symlinked directory: %v %v", problems, err)
		}
	})
	t.Run("relative", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := FreezeManifest(entries, "frozenrel"); err != nil {
			t.Fatalf("freeze to a relative destination: %v", err)
		}
		if problems, err := VerifyFrozen("frozenrel", entries); err != nil || len(problems) != 0 {
			t.Fatalf("verify a relative reference: %v %v", problems, err)
		}
	})
}
