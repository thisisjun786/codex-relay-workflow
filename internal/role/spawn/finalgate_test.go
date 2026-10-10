package spawn

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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
// its inputs under the CRW names, the oracle's answer and the answer expected here, which differs by the name substitution and, in the
// nine cases that keep a read inside the working directory, by the security fixes the reason of the case names.
func TestCheckFinalGatePrereqsMatchesTheOracle(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name, Packet, Session string
			Files, Symlinks       map[string]string
			Dirs                  []string
			Pad                   struct {
				File  string
				Bytes int
			}
			Capture        json.RawMessage
			Expected       FinalGateCheck
			Classification string
			Reason         string
		}
	}
	raw, err := os.ReadFile("testdata/finalgate/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 82 {
		t.Fatalf("got %d recorded cases, want 82", len(fixture.Cases))
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
			if c.Pad.File != "" {
				prior, err := os.ReadFile(filepath.Join(cwd, c.Pad.File))
				if err != nil {
					t.Fatal(err)
				}
				spawnFinalGateTestWrite(t, cwd, c.Pad.File, string(prior)+strings.Repeat(" ", c.Pad.Bytes))
			}
			for rel, target := range c.Symlinks {
				if err := os.Symlink(expand(target), filepath.Join(cwd, rel)); err != nil {
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

// A named pipe at a state path is refused without being opened for reading, which would wait for a writer forever (the oracle waits).
func TestCheckFinalGatePrereqsDoesNotWaitOnANamedPipe(t *testing.T) {
	cwd := spawnFinalGateTestTree(t)
	if err := os.MkdirAll(filepath.Join(cwd, ".crw", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(cwd, ".crw", "sessions", "sess-1.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	answered := make(chan FinalGateCheck, 1)
	go func() { answered <- CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil) }()
	select {
	case got := <-answered:
		if !got.OK {
			t.Fatalf("a named pipe in place of the session state was not fail-open: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the check is waiting on the named pipe")
	}
}

// The guard recaptures the tree under the receipt producer's own contract (CRW-1114, known-defects.md:1238): the state directory
// and the paths the receipt declared generated are left out, so a receipt that is right for the tree is not stale, while a change
// of a path the receipt did not declare still is.
func TestCheckFinalGatePrereqsRecapturesWithTheReceiptsExclusions(t *testing.T) {
	cwd := spawnFinalGateTestTree(t)
	spawnFinalGateTestGit(t, cwd, "init", "-q", "-b", "main", ".")
	spawnFinalGateTestWrite(t, cwd, "generated.txt", "one\n")
	spawnFinalGateTestWrite(t, cwd, "source.txt", "one\n")
	spawnFinalGateTestGit(t, cwd, "add", ".")
	spawnFinalGateTestGit(t, cwd, "commit", "-qm", "first")
	head := spawnFinalGateTestGit(t, cwd, "rev-parse", "HEAD")
	spawnFinalGateTestWrite(t, cwd, "generated.txt", "two\n")
	receipt := source.Capture(cwd, source.Options{ExcludeStateArtifacts: true, GeneratedPaths: []string{"generated.txt"}})
	if receipt.Dirty {
		t.Fatalf("the receipt's own capture is dirty: %+v", receipt)
	}
	body, err := json.Marshal(map[string]any{"kind": "test", "sourceIdentity": receipt, "generatedPaths": []string{"generated.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	spawnFinalGateTestWrite(t, cwd, ".crw/sessions/sess-1.json", `{"sessionId":"sess-1","slug":"demo"}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/goalplans/demo/goalplan.json", `{"criteria":[],"finalGate":{"testReceiptPath":".crw/evidence/test.json"}}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/evidence/test.json", string(body))
	if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil); !got.OK {
		t.Fatalf("a receipt that is right under its own exclusions was refused: %+v", got)
	}
	spawnFinalGateTestWrite(t, cwd, "source.txt", "two\n")
	got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, nil)
	if want := "produced against " + head[:7] + ", but the tree is now " + head[:7] + "+dirty"; got.OK || !strings.Contains(got.Reason, want) {
		t.Fatalf("a change of an undeclared path was not stale: %+v", got)
	}
}

// spawnFinalGateTestAliasTree is an empty working directory and a symbolic link to it beside it, so one tree is reachable by two
// spellings: the real path and the alias.
func spawnFinalGateTestAliasTree(t *testing.T) (real, alias string) {
	t.Helper()
	real = spawnFinalGateTestTree(t)
	alias = filepath.Join(filepath.Dir(real), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	return real, alias
}

// spawnFinalGateTestReceiptPlan writes the session state and a goalplan under root whose test receipt is recorded at recorded, and
// the test receipt, holding the aaaaaaa identity, at file (absolute, or below root).
func spawnFinalGateTestReceiptPlan(t *testing.T, root, recorded, file string) {
	t.Helper()
	receipt, err := json.Marshal(map[string]any{"kind": "test", "sourceIdentity": source.Identity{Kind: source.KindResolved, CommitSha: "aaaaaaa"}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(map[string]any{"criteria": []any{}, "finalGate": map[string]any{"testReceiptPath": recorded}})
	if err != nil {
		t.Fatal(err)
	}
	spawnFinalGateTestWrite(t, root, ".crw/sessions/sess-1.json", `{"sessionId":"sess-1","slug":"demo"}`)
	spawnFinalGateTestWrite(t, root, ".crw/goalplans/demo/goalplan.json", string(plan))
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, receipt, 0o644); err != nil {
		t.Fatal(err)
	}
}

// spawnFinalGateTestIdentity is the capture the receipts above match: the tree is at the aaaaaaa commit.
func spawnFinalGateTestIdentity(string) source.Identity {
	return source.Identity{Kind: source.KindResolved, CommitSha: "aaaaaaa"}
}

// A receipt given as an absolute path inside the working directory is read as the path below it, whatever the working directory is
// spelled as: ".", absolute, a relative alias, or a symbolic link to the real directory, and whichever spelling the receipt uses.
func TestCheckFinalGatePrereqsReadsAnAbsoluteReceiptInsideCwd(t *testing.T) {
	const receipt = ".crw/evidence/test.json"
	cases := []struct {
		name     string
		chdir    string // "real", "parent" or "" (the test's own directory)
		cwd      string // the working directory the check is given: "real" or "alias" stand for the absolute path unless chdir is set, then it is passed as written
		recorded string // the path the goalplan records, with "real" or "alias" standing for the two spellings of the tree
	}{
		{name: "cwd dot, absolute receipt", chdir: "real", cwd: ".", recorded: "real/" + receipt},
		{name: "cwd absolute, absolute receipt", cwd: "real", recorded: "real/" + receipt},
		{name: "cwd absolute, relative receipt", cwd: "real", recorded: receipt},
		{name: "cwd dot, relative receipt", chdir: "real", cwd: ".", recorded: receipt},
		{name: "cwd alias, absolute receipt by the real path", cwd: "alias", recorded: "real/" + receipt},
		{name: "cwd alias, absolute receipt by the alias path", cwd: "alias", recorded: "alias/" + receipt},
		{name: "cwd alias, relative receipt", cwd: "alias", recorded: receipt},
		{name: "cwd real, absolute receipt by the alias path", cwd: "real", recorded: "alias/" + receipt},
		{name: "relative alias cwd, absolute receipt by the real path", chdir: "parent", cwd: "alias", recorded: "real/" + receipt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			real, alias := spawnFinalGateTestAliasTree(t)
			paths := map[string]string{"real": real, "alias": alias}
			if c.chdir != "" {
				paths = nil // a relative cwd is given as written, not as the absolute path of the tree
			}
			switch c.chdir {
			case "real":
				t.Chdir(real)
			case "parent":
				t.Chdir(filepath.Dir(real))
			}
			recorded := strings.NewReplacer("real/", real+"/", "alias/", alias+"/").Replace(c.recorded)
			spawnFinalGateTestReceiptPlan(t, real, recorded, filepath.Join(real, receipt))
			cwd := c.cwd
			if p, ok := paths[cwd]; ok {
				cwd = p
			}
			if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, spawnFinalGateTestIdentity); !got.OK {
				t.Fatalf("a receipt at %q inside cwd %q was refused: %s", recorded, cwd, got.Reason)
			}
		})
	}
}

// On a case-insensitive file system (APFS, NTFS) a link to the working directory resolves to the spelling the link text holds, which
// can differ in case from the spelling the receipt path uses for the same directory: the two are one directory by identity, not by
// string. The seam makes two directories of a case-insensitive spelling stat alike, as such a file system does; the receipt is read
// below the real working directory, and a directory of another name is still not the working directory.
func TestCheckFinalGatePrereqsReadsAReceiptSpelledInAnotherCaseOfCwd(t *testing.T) {
	const receipt = ".crw/evidence/test.json"
	for _, c := range []struct {
		name      string
		spelling  func(real string) string // the directory the receipt path names
		wantReads bool
	}{
		{"the other case of the directory name", func(real string) string {
			return filepath.Join(filepath.Dir(real), strings.ToUpper(filepath.Base(real)))
		}, true},
		{"another directory name", func(real string) string { return filepath.Join(filepath.Dir(real), "other") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			real, alias := spawnFinalGateTestAliasTree(t)
			spelled := c.spelling(real)
			// On a case-insensitive file system the other case of the name is the working directory itself, which already exists.
			if err := os.Mkdir(spelled, 0o755); err != nil {
				got, statErr := os.Stat(spelled)
				want, wantErr := os.Stat(real)
				if !errors.Is(err, fs.ErrExist) || statErr != nil || wantErr != nil || !os.SameFile(want, got) {
					t.Fatal(err)
				}
			}
			recorded := filepath.Join(spelled, receipt)
			spawnFinalGateTestReceiptPlan(t, real, recorded, filepath.Join(real, receipt))
			defer func(sameDir func(a, b os.FileInfo) bool) { spawnFinalGateSameDir = sameDir }(spawnFinalGateSameDir)
			spawnFinalGateSameDir = func(a, b os.FileInfo) bool { return os.SameFile(a, b) || strings.EqualFold(a.Name(), b.Name()) }
			got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", alias, spawnFinalGateTestIdentity)
			if got.OK != c.wantReads {
				t.Fatalf("a receipt at %q with cwd %q: OK %v, want %v (%s)", recorded, alias, got.OK, c.wantReads, got.Reason)
			}
		})
	}
}

// A receipt that leaves the working directory is refused, by name, by an alias of the working directory, or by a link, as before the
// fix: every read stays below the os.Root of cwd (the oracle reads them, the port does not).
func TestCheckFinalGatePrereqsKeepsAReceiptOutsideCwdRejected(t *testing.T) {
	const receipt = ".crw/evidence/test.json"
	cases := []struct {
		name  string
		chdir bool // whether the check runs in the real tree, with "." as cwd
		setup func(t *testing.T, real, alias string) (cwd, recorded string)
	}{
		{
			name:  "cwd dot, absolute receipt beside cwd",
			chdir: true,
			setup: func(t *testing.T, real, alias string) (string, string) {
				outside := filepath.Join(filepath.Dir(real), "outside.json")
				spawnFinalGateTestReceiptPlan(t, real, outside, outside)
				return ".", outside
			},
		},
		{
			name: "cwd alias, absolute receipt beside cwd by the real path",
			setup: func(t *testing.T, real, alias string) (string, string) {
				outside := filepath.Join(real, "..", "outside.json")
				spawnFinalGateTestReceiptPlan(t, real, outside, outside)
				return alias, outside
			},
		},
		{
			name: "cwd alias, absolute receipt in a directory linked outside",
			setup: func(t *testing.T, real, alias string) (string, string) {
				outsideDir := filepath.Join(filepath.Dir(real), "outside-dir")
				if err := os.MkdirAll(filepath.Join(real, ".crw"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideDir, filepath.Join(real, ".crw", "linked")); err != nil {
					t.Fatal(err)
				}
				recorded := filepath.Join(alias, ".crw", "linked", "test.json")
				spawnFinalGateTestReceiptPlan(t, real, recorded, filepath.Join(outsideDir, "test.json"))
				return alias, recorded
			},
		},
		{
			name: "absolute link to a receipt inside cwd",
			setup: func(t *testing.T, real, alias string) (string, string) {
				link := filepath.Join(real, ".crw", "evidence", "link.json")
				spawnFinalGateTestReceiptPlan(t, real, link, filepath.Join(real, receipt))
				if err := os.Symlink(filepath.Join(real, receipt), link); err != nil {
					t.Fatal(err)
				}
				return real, link
			},
		},
		{
			name: "relative link out of cwd, reached by an absolute receipt through the alias",
			setup: func(t *testing.T, real, alias string) (string, string) {
				outside := filepath.Join(filepath.Dir(real), "outside.json")
				spawnFinalGateTestReceiptPlan(t, real, filepath.Join(alias, ".crw", "evidence", "escape.json"), outside)
				if err := os.MkdirAll(filepath.Join(real, ".crw", "evidence"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../../outside.json", filepath.Join(real, ".crw", "evidence", "escape.json")); err != nil {
					t.Fatal(err)
				}
				return alias, filepath.Join(alias, ".crw", "evidence", "escape.json")
			},
		},
		{
			name: "absolute directory link to a directory inside cwd",
			setup: func(t *testing.T, real, alias string) (string, string) {
				recorded := filepath.Join(real, ".crw", "linked", "test.json")
				spawnFinalGateTestReceiptPlan(t, real, recorded, filepath.Join(real, receipt))
				if err := os.Symlink(filepath.Join(real, ".crw", "evidence"), filepath.Join(real, ".crw", "linked")); err != nil {
					t.Fatal(err)
				}
				return real, recorded
			},
		},
		{
			name: "relative directory links out of cwd and back in",
			setup: func(t *testing.T, real, alias string) (string, string) {
				outsideDir := filepath.Join(filepath.Dir(real), "outside-dir")
				recorded := filepath.Join(alias, ".crw", "linked", "back", "test.json")
				spawnFinalGateTestReceiptPlan(t, real, recorded, filepath.Join(real, receipt))
				if err := os.MkdirAll(outsideDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../outside-dir", filepath.Join(real, ".crw", "linked")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../"+filepath.Base(real)+"/.crw/evidence", filepath.Join(outsideDir, "back")); err != nil {
					t.Fatal(err)
				}
				return alias, recorded
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			real, alias := spawnFinalGateTestAliasTree(t)
			if c.chdir {
				t.Chdir(real)
			}
			cwd, recorded := c.setup(t, real, alias)
			got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, spawnFinalGateTestIdentity)
			if want := "test receipt is missing, empty or unreadable: " + recorded; got.OK || !strings.Contains(got.Reason, want) {
				t.Fatalf("a receipt at %q outside cwd %q was not refused as unreadable: %+v", recorded, cwd, got)
			}
		})
	}
}
