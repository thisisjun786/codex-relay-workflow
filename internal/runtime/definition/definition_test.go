package definition_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// The Go definition carries components.json's retained fields and agrees with the committed
// file field for field; it carries no per-target digest, so no build has to regenerate it.
func TestDefinitionAgreesWithComponentsJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(golden.Root(), "scripts", "crw_runtime", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed struct {
		DefinitionVersion int `json:"definitionVersion"`
		Components        []struct {
			Component, ConsoleScript, Version, LicencePath, IdentityTool, ExerciseCommand string
			Upstream                                                                      struct{ Remote, Revision, Licence string }
		}
	}
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.DefinitionVersion != definition.Version || len(committed.Components) != len(definition.Components) {
		t.Fatalf("definitionVersion %d, %d components", committed.DefinitionVersion, len(committed.Components))
	}
	for i, c := range committed.Components {
		g := definition.Components[i]
		if c.Component != g.Name || c.ConsoleScript != g.ConsoleScript || c.Version != g.Version || c.LicencePath != g.LicencePath ||
			c.IdentityTool != g.IdentityTool || c.ExerciseCommand != g.ExerciseCommand || c.Upstream.Remote != g.Upstream.Remote ||
			c.Upstream.Revision != g.Upstream.Revision || c.Upstream.Licence != g.Upstream.Licence {
			t.Errorf("component %d: committed %+v, go %+v", i, c, g)
		}
		if _, err := os.Stat(filepath.Join(golden.Root(), g.LicencePath)); err != nil {
			t.Errorf("%s: licence %s: %v", g.Name, g.LicencePath, err)
		}
	}
	provenance, err := os.ReadFile(filepath.Join(golden.Root(), "packages", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range definition.Components {
		if !strings.Contains(string(provenance), g.Upstream.Revision) {
			t.Errorf("%s: upstream revision %s is not in packages/README.md", g.Name, g.Upstream.Revision)
		}
	}
	links := definition.Links()
	if len(links) != 3 || links[0] != "codex-session-relay" || links[1] != "codex-thread-bridge" || links[2] != "crw-completion-hook" {
		t.Fatalf("links %v", links)
	}
}

// The OPS-1.2 walk: relative path, a zero byte, the file's SHA-256, sorted; __pycache__
// excluded; the digest changes with any byte.
func TestDigestIsTheOPS12Walk(t *testing.T) {
	want := golden.Obj(golden.Section(t, "ops12"))
	root := filepath.Join(t.TempDir(), "pkg")
	for _, f := range golden.Obj(record.Get(want, "tree")) {
		path := filepath.Join(root, filepath.FromSlash(f.Key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte{}
		for _, r := range f.Value.(string) {
			data = append(data, byte(r))
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := definition.Digest(root)
	if err != nil || got != record.Get(want, "digest") {
		t.Fatalf("go %s (%v), python %v", got, err, record.Get(want, "digest"))
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "__pycache__", "more.pyc"), []byte("more"), 0o644); err != nil {
		t.Fatal(err)
	}
	if again, _ := definition.Digest(root); again != got {
		t.Fatal("a bytecode cache changed the digest")
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.py"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := definition.Digest(root); changed == got {
		t.Fatal("a changed file kept the digest")
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(root, "z"), 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(filepath.Join(root, "z"), 0o755)
		if _, err := definition.Digest(root); err == nil {
			t.Fatal("an unreadable subtree was left out instead of failing the walk")
		}
	}
}

// An entry the walk cannot examine is answered as definition.ops12_digest answers it: a link
// whose target is absent is not a file (os.DirEntry.is_file is False) and leaves the digest as
// it was, while a link loop or a link into a directory without search permission raises there,
// so here it fails the walk instead of producing a digest that claims the whole tree.
func TestDigestFailsOnAnEntryItCannotExamine(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.py"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := definition.Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if got, err := definition.Digest(root); err != nil || got != base {
		t.Fatalf("a dangling link: %s %v, want %s", got, err, base)
	}
	if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if got, err := definition.Digest(root); err == nil {
		t.Errorf("a link loop was left out of digest %s", got)
	}
	if err := os.Remove(filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		return // root searches any directory
	}
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.py"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(locked, "f.py"), filepath.Join(root, "behind")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	if got, err := definition.Digest(root); err == nil {
		t.Errorf("a link into a directory without search permission was left out of digest %s", got)
	}
}

// A file name that is not UTF-8 has no digest in Python: os.fsdecode spells its bytes as lone
// surrogates and ops12_digest's .encode() refuses them, so the walk fails in Python's words
// rather than answering a digest Python cannot compute. The files are taken in Python's order
// (code points, where such a byte sorts after U+4E2D and before U+10000), so an unreadable file
// Python reaches first is the failure, as it is there.
func TestDigestRefusesANameThatIsNotUTF8(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "\xe4\xb8\xad", "\xff"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := definition.Digest(root)
	var refused *definition.UnicodeEncodeError
	if !errors.As(err, &refused) || err.Error() != `'utf-8' codec can't encode character '\udcff' in position 0: surrogates not allowed` {
		t.Fatalf("a name that is not UTF-8: digest %q, error %v", got, err)
	}
	if err := os.Remove(filepath.Join(root, "\xff")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "z\xff\xfe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "z\xff\xfe", "q"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := definition.Digest(root); err == nil || err.Error() != `'utf-8' codec can't encode characters in position 1-2: surrogates not allowed` {
		t.Fatalf("a directory whose name is not UTF-8: %v", err)
	}
	if os.Geteuid() == 0 {
		return // root reads any file
	}
	ordered := t.TempDir()
	for _, name := range []string{"\x80", "\xe4\xb8\xad"} {
		if err := os.WriteFile(filepath.Join(ordered, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(ordered, "\xe4\xb8\xad"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(ordered, "\xe4\xb8\xad"), 0o644)
	if _, err := definition.Digest(ordered); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("U+4E2D sorts before the surrogate for 0x80, so its unreadable file is the failure: %v", err)
	}
}
