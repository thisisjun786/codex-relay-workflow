package gate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The oracle's check-gate-generated.test.ts over real git repositories.

const (
	epoch     = "c-20260829000000-testep"
	sessionID = "test-session-0001"
	moved     = "the source changed after the check ran (working tree went dirty)"
)

// hermetic isolates git from the machine: no inherited routing or configuration, a fixed identity, discovery stopping at the
// returned directory, which holds one test's repositories. Setenv forbids t.Parallel.
func hermetic(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	base := t.TempDir()
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(name, value)
	}
	return base
}

// repo is a repository with a tracked source file and a tracked generated artifact.
func repo(t *testing.T, base string) string {
	t.Helper()
	root, err := os.MkdirTemp(base, "r-")
	must(t, err)
	writeFile(t, filepath.Join(root, "build", "graph.json"), `{"nodes":0}`)
	writeFile(t, filepath.Join(root, "src.txt"), "source")
	for _, args := range [][]string{{"init", "-q", "-b", "main", "."}, {"add", "-A"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return root
}

// gateAfter writes a check receipt for a fresh repository (declaring generated, body fields overridden), moves the tree by change
// and runs the C->D gate on the receipt.
func gateAfter(t *testing.T, generated []string, overrides map[string]any, change func(t *testing.T, root string)) CheckGateResult {
	t.Helper()
	root := repo(t, hermetic(t))
	body := map[string]any{
		"kind": "test", "command": "npm test", "exitCode": 0, "createdAt": "2026-01-01T00:00:00Z",
		"sourceIdentity": source.Capture(root, source.Options{ExcludeStateArtifacts: true, GeneratedPaths: generated}),
		"ownerSessionId": sessionID, "checkEpoch": epoch,
	}
	if generated != nil {
		body["generatedPaths"] = generated
	}
	for k, v := range overrides {
		body[k] = v
	}
	receipt := filepath.Join(root, stateDir, "evidence", sessionID, "test-receipt.json")
	writeFile(t, receipt, body)
	if change != nil {
		change(t, root)
	}
	e := epoch
	return ValidateCheckReceipt(state.State{Phase: state.PhaseC, CheckEpoch: &e}, sessionID, receipt, root)
}

// edit is a change that rewrites one file of the repository.
func edit(rel, body string) func(*testing.T, string) {
	return func(t *testing.T, root string) { writeFile(t, filepath.Join(root, rel), body) }
}

func TestADeclaredGeneratedPathDoesNotBreakTheGateWhenItKeepsChanging(t *testing.T) {
	// The declared artifact moves AFTER the receipt was written, as a concurrently regenerated artifact does.
	if got := gateAfter(t, []string{"build"}, nil, edit("build/graph.json", `{"nodes":42}`)); !got.OK {
		t.Fatal(got.Reason)
	}
}

func TestAnUndeclaredChangeStillFailsTheGate(t *testing.T) {
	if got := gateAfter(t, []string{"build"}, nil, edit("src.txt", "rewritten")); got.OK || !strings.Contains(got.Reason, moved) {
		t.Fatalf("%+v", got)
	}
}

func TestAReceiptWithNoGeneratedPathsKeepsTheStrictBehaviour(t *testing.T) {
	if got := gateAfter(t, nil, nil, edit("build/graph.json", `{"nodes":42}`)); got.OK || !strings.Contains(got.Reason, moved) {
		t.Fatalf("%+v", got)
	}
}

func TestAnUnchangedTreePassesWhetherOrNotPathsAreDeclared(t *testing.T) {
	if got := gateAfter(t, nil, nil, nil); !got.OK {
		t.Fatal(got.Reason)
	}
}

func TestDeclaringAPathCannotLaunderAChangeOutsideIt(t *testing.T) {
	// "docs" does not exist; the real change is in build/, so it must still be caught.
	if got := gateAfter(t, []string{"docs"}, nil, edit("build/graph.json", `{"nodes":42}`)); got.OK || !strings.Contains(got.Reason, moved) {
		t.Fatalf("%+v", got)
	}
}

func TestAMalformedGeneratedPathsListDegradesToNoExclusionsNotARejection(t *testing.T) {
	// Unchanged tree: the receipt is still usable, so a bad list did not reject it.
	if got := gateAfter(t, nil, map[string]any{"generatedPaths": []any{42, "", nil}}, nil); !got.OK {
		t.Fatal(got.Reason)
	}
}
