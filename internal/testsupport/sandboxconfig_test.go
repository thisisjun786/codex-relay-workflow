package testsupport

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
)

// CRW-1265: the isolation writes the crw configuration a test process and every process it starts read, and
// its sandbox_root is the temporary directory, wherever TMPDIR lies. Before, nothing named it, the rule's
// default /scratch/crw decided, and a workspace under a TMPDIR elsewhere (/srv/ci/t, /tmp) was refused as
// outside_dedicated_root, or as home_ancestor when it held the isolated HOME.
func TestIsolate_names_the_temporary_directory_as_the_sandbox_root(t *testing.T) {
	// Given: a TMPDIR of the test's own, isolated as a TestMain does
	tmp := privateTMPDIR(t)
	previous := sandboxConfigFile
	t.Cleanup(func() { sandboxConfigFile = previous })
	root := isolateForTest(t)
	real, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Then: the isolated configuration home holds the crw configuration, SandboxConfig names it, and its
	// sandbox_root is the temporary directory
	want := filepath.Join(root, "xdg-config", "crw", "config.json")
	if got := SandboxConfig(); got != want {
		t.Fatalf("SandboxConfig() = %q, want %q", got, want)
	}
	file, err := crwconfig.Load(os.Getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	if path, source := file.Path(); path != want || source != crwconfig.SourceEnv {
		t.Fatalf("the configuration read is %q (%s), want %q from XDG_CONFIG_HOME", path, source, want)
	}
	if got := file.Roots()[crwconfig.RootSandbox].Path; got != real {
		t.Fatalf("sandbox_root = %q, want the temporary directory %q", got, real)
	}

	// And: the rule takes a workspace below it, and the directory that holds the isolated HOME, as the lane
	// saw them under /scratch/crw
	workspace := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{workspace, root} {
		if err := sandboxroot.Check(dir, "workspace-write"); err != nil {
			t.Errorf("Check(%s) = %v, want a dedicated root", dir, err)
		}
	}

	// And: the rule is not weakened outside it: the directory above the temporary directory is refused
	outside := filepath.Dir(real)
	var refusal *sandboxroot.Refusal
	if err := sandboxroot.Check(outside, "workspace-write"); !errors.As(err, &refusal) {
		t.Fatalf("Check(%s) = %v, want a refusal", outside, err)
	}
}

// CRW-1579 (R1-3, D7): the document names the temporary directory's real path as sandbox_root and nothing else: no project
// roots (there are no fallback parents) and no socket root (sockets are not judged by the sandbox rule).
func TestSandboxConfigDocument_isTheTempRootOnly(t *testing.T) {
	tmp := t.TempDir()
	doc, err := SandboxConfigDocument(tmp)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(tmp)
	want := map[string]any{"paths": map[string]any{"sandbox_root": real}}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("document = %v, want exactly %v", doc, want)
	}
	if _, err := SandboxConfigDocument(filepath.Join(tmp, "absent")); err == nil {
		t.Fatal("a temporary directory that does not exist gave a document")
	}
}
