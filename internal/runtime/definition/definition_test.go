package definition_test

import (
	"encoding/json"
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
