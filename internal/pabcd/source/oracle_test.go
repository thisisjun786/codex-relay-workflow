package source

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The oracle's answers for the scenario trees of testdata/scenarios.json were recorded once with
// testdata/record-oracle.mjs (CXC v0.2.40 under Node 24); no Node runs here. The recorded git status output goes
// through the parser and the hasher (so that check does not depend on the git on this host), and a live capture
// must give the recorded kind, commit id and, when this host's git prints the recorded status bytes, hash.

type scenario struct {
	ID      string `json:"id"`
	Base    string `json:"base"`
	Script  string `json:"script"` // run with sh -ec in the tree, as the recorder runs it
	Cwd     string `json:"cwd"`    // where to capture, relative to the tree; empty is its root
	Options struct {
		GeneratedPaths []string `json:"generatedPaths"`
	} `json:"options"`
}

type recorded struct {
	Kind      Kind    `json:"kind"`
	CommitSha string  `json:"commitSha"`
	Dirty     bool    `json:"dirty"`
	TreeHash  string  `json:"treeHash"`
	StatusZ   *string `json:"statusZ"` // base64 of git status --porcelain=v1 -z --untracked-files=all
}

func readJSON(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	must(t, json.Unmarshal(raw, into))
}

func b64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	must(t, err)
	return string(b)
}

// build realizes a scenario: the oracle tests' repository (or no repository, or an empty one) and its script.
func build(t *testing.T, base string, sc scenario) string {
	t.Helper()
	root := newRepo(t, base)
	if sc.Base != "" {
		root = tempIn(t, base)
		if sc.Base == "unborn" {
			gitIn(t, root, "init", "-q", "-b", "main", ".")
		}
	}
	cmd := exec.Command("sh", "-ec", sc.Script)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scenario %s: %v\n%s", sc.ID, err, out)
	}
	return root
}

func TestOracleParity(t *testing.T) {
	var scenarios []scenario
	var golden map[string]recorded
	readJSON(t, "scenarios.json", &scenarios)
	readJSON(t, "oracle-identities.json", &golden)
	if len(golden) != len(scenarios) {
		t.Fatalf("%d scenarios, %d recorded answers", len(scenarios), len(golden))
	}
	base := hermetic(t)
	for _, sc := range scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			want, opts := golden[sc.ID], Options{GeneratedPaths: sc.Options.GeneratedPaths}
			root := build(t, base, sc)
			dir := filepath.Join(root, sc.Cwd)
			check := func(what string, got Identity) {
				t.Helper()
				if got.Dirty != want.Dirty || got.TreeHash != want.TreeHash {
					t.Fatalf("%s: dirty %v tree %s, oracle dirty %v tree %s", what, got.Dirty, got.TreeHash, want.Dirty, want.TreeHash)
				}
			}
			if want.StatusZ != nil {
				check("recorded status", resolve(dir, want.CommitSha, "", []byte(b64(t, *want.StatusZ)), opts))
			}
			got := Capture(dir, opts)
			if got.Kind != want.Kind || got.CommitSha != want.CommitSha {
				t.Fatalf("capture: kind %s commit %q, oracle kind %s commit %q", got.Kind, got.CommitSha, want.Kind, want.CommitSha)
			}
			if want.StatusZ != nil && gitIn(t, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all") != b64(t, *want.StatusZ) {
				t.Skip("this host's git prints other status bytes than the recording; the recorded status checked the hashing")
			}
			check("capture", got)
		})
	}
}

// The oracle recorded this tree hash for a linked worktree whose only change is the untracked file "implemented"
// holding "yes\n", captured the way a session bound to that worktree captures it
// (contract/fixtures/cxc/cli__session__source_binds_linked_worktree.json).
func TestCorpusAnchorLinkedWorktree(t *testing.T) {
	base := hermetic(t)
	root := newRepo(t, base)
	worktree := filepath.Join(base, "wt")
	gitIn(t, root, "worktree", "add", "-q", "-b", "work", worktree)
	writeFile(t, worktree, "implemented", "yes\n")
	if got, want := Capture(worktree, Options{ExcludeStateArtifacts: true}).TreeHash, "1402c9e5de3ced20986c8b130532286dfad3ebafef30c0a74b570fd345887e95"; got != want {
		t.Fatalf("tree hash %s, oracle %s", got, want)
	}
	if !LooksLikeRepo(root) || !LooksLikeRepo(worktree) || LooksLikeRepo(base) {
		t.Fatal("a repository and a linked worktree (whose .git is a file) look like repositories, their parent does not")
	}
}
