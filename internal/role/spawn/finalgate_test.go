package spawn

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
)

const (
	spawnFinalGateTestPacket = spawnFinalGateMarker + " please review the final gate"
	spawnFinalGateTestCwd    = "$" + "{WS}" // the working directory in a recorded case
)

// spawnFinalGateTestTree is the empty working directory of one test, in an environment that cannot reach the machine's Codex, CRW or
// git state: HOME, CODEX_HOME and CRW_HOME are temporary directories, no git routing is inherited, no user or system git
// configuration is read, the identity and dates are fixed, and repository discovery stops at the directory that holds the tree.
// Setenv forbids t.Parallel.
func spawnFinalGateTestTree(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"HOME": filepath.Join(base, "home"), "CODEX_HOME": filepath.Join(base, "codex"), "CRW_HOME": filepath.Join(base, "crw"),
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
	} {
		t.Setenv(name, value)
	}
	cwd := filepath.Join(base, "ws")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func spawnFinalGateTestWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// spawnFinalGateTestGit runs git in dir and returns its trimmed output.
func spawnFinalGateTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// spawnFinalGateTestCommit makes root a repository (or adds a commit to it) whose state directory is ignored, so the receipts
// written below it leave the tree clean, and returns the commit.
func spawnFinalGateTestCommit(t *testing.T, root, file string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		spawnFinalGateTestGit(t, root, "init", "-q", "-b", "main", ".")
		spawnFinalGateTestWrite(t, root, ".gitignore", ".crw/\n")
	}
	spawnFinalGateTestWrite(t, root, file, file+"\n")
	spawnFinalGateTestGit(t, root, "add", ".")
	spawnFinalGateTestGit(t, root, "commit", "-qm", file)
	return spawnFinalGateTestGit(t, root, "rev-parse", "HEAD")
}

// spawnFinalGateTestPlan writes the session state, the goalplan and the test receipt holding identity, below cwd.
func spawnFinalGateTestPlan(t *testing.T, cwd string, identity source.Identity) {
	t.Helper()
	receipt, err := json.Marshal(map[string]any{"kind": "test", "sourceIdentity": identity})
	if err != nil {
		t.Fatal(err)
	}
	spawnFinalGateTestWrite(t, cwd, ".crw/sessions/sess-1.json", `{"sessionId":"sess-1","slug":"demo"}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/goalplans/demo/goalplan.json", `{"criteria":[],"finalGate":{"testReceiptPath":".crw/evidence/test.json"}}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/evidence/test.json", string(receipt))
}

// The cases are the answers of the oracle (record.mjs ran the real final-gate-guard.ts): the 15 identity-injected cases of
// final-gate-guard.test.ts, whose two hook-route tests wait for the hook leg, and one case for each further branch. A case holds
// its inputs under the CRW names, the oracle's answer and the answer expected here, which differs only by the name substitution.
func TestCheckFinalGatePrereqsMatchesTheOracle(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name, Packet, Session string
			Files, Symlinks       map[string]string
			Dirs                  []string
			Capture               json.RawMessage
			Expected              FinalGateCheck
			Classification        string
			Reason                string
		}
	}
	raw, err := os.ReadFile("testdata/finalgate/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 71 {
		t.Fatalf("got %d recorded cases, want 71", len(fixture.Cases))
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Classification != "identical" && (c.Classification != "intentionally-changed" || c.Reason == "") {
				t.Fatal("unclassified oracle case")
			}
			cwd := spawnFinalGateTestTree(t)
			expand := func(s string) string { return strings.ReplaceAll(s, spawnFinalGateTestCwd, cwd) }
			for _, dir := range c.Dirs {
				if err := os.MkdirAll(filepath.Join(cwd, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for rel, body := range c.Files {
				spawnFinalGateTestWrite(t, cwd, rel, expand(body))
			}
			for rel, target := range c.Symlinks {
				if err := os.Symlink(target, filepath.Join(cwd, rel)); err != nil {
					t.Fatal(err)
				}
			}
			var capture func(string) source.Identity
			switch string(c.Capture) {
			case "null":
			case `"throw"`:
				capture = func(string) source.Identity { panic("the capture throws") }
			default:
				var identity source.Identity
				if err := json.Unmarshal(c.Capture, &identity); err != nil {
					t.Fatal(err)
				}
				capture = func(string) source.Identity { return identity }
			}
			got := CheckFinalGatePrereqs(expand(c.Packet), c.Session, cwd, capture)
			if want := (FinalGateCheck{c.Expected.OK, expand(c.Expected.Reason)}); got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// With no capture the tree is read as the session's own capture reads it, a repository here, and a receipt of another commit is
// stale.
func TestCheckFinalGatePrereqsReadsTheTreeWithoutACapture(t *testing.T) {
	cwd := spawnFinalGateTestTree(t)
	first := spawnFinalGateTestCommit(t, cwd, "first.txt")
	spawnFinalGateTestPlan(t, cwd, source.Identity{Kind: source.KindResolved, CommitSha: first})
	if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil); !got.OK {
		t.Fatalf("a receipt of the current commit was refused: %s", got.Reason)
	}
	second := spawnFinalGateTestCommit(t, cwd, "second.txt")
	got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil)
	want := "  - test receipt was produced against " + first[:7] + ", but the tree is now " + second[:7] + "\n"
	if got.OK || !strings.Contains(got.Reason, want) {
		t.Fatalf("a receipt of the first commit was not refused as stale: %+v", got)
	}
}

// A session bound to a worktree is compared in that worktree: the given capture is asked about its root, whose identity gets
// the root as its sourceRoot, so a receipt that carries none is stale, and with no capture the session's own capture answers alike.
func TestCheckFinalGatePrereqsComparesTheBoundWorktree(t *testing.T) {
	cwd := spawnFinalGateTestTree(t)
	root := filepath.Join(filepath.Dir(cwd), "src")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	head := spawnFinalGateTestCommit(t, root, "tracked.txt")
	if _, err := sourcesession.Bind(cwd, "sess-1", root); err != nil {
		t.Fatal(err)
	}
	var asked string
	given := func(dir string) source.Identity {
		asked = dir
		return source.Capture(dir, source.Options{ExcludeStateArtifacts: true})
	}
	for name, capture := range map[string]func(string) source.Identity{"a given capture": given, "no capture": nil} {
		spawnFinalGateTestPlan(t, cwd, source.Identity{Kind: source.KindResolved, CommitSha: head, SourceRoot: &root})
		asked = ""
		if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, capture); !got.OK {
			t.Errorf("%s: a receipt of the bound worktree was refused: %s", name, got.Reason)
		}
		if capture != nil && asked != root {
			t.Errorf("%s: the capture was asked about %q, want the bound root %q", name, asked, root)
		}
		spawnFinalGateTestPlan(t, cwd, source.Identity{Kind: source.KindResolved, CommitSha: head})
		got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, capture)
		if want := "test receipt was produced against " + head[:7] + ", but the tree is now " + head[:7]; got.OK || !strings.Contains(got.Reason, want) {
			t.Errorf("%s: a receipt without the source root was not refused as stale: %+v", name, got)
		}
	}
}
