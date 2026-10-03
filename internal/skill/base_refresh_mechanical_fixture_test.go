package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic repository the mechanical-resolution tests run on (CRW-412). It is a real git
// repository: dev is the base branch, feature the pull request branch whose tip is the verified
// head, and the helpers build the merge the parent makes when the forge reports a conflict and the
// resolutions it may or may not make of it. plugin.json is derived from payload/ by regen.sh
// (a stand-in for the record-version command: a checksum of the payload files), and backlog.md is
// an append-only list.

const (
	mechRepository = "owner/repo"
	backlogBase    = "# Backlog\n- e1\n- e2\n"
)

const mechRegenScript = "sum=$(cat payload/one.txt payload/two.txt | cksum | cut -d' ' -f1)\nprintf '{\\n  \"version\": \"0.4.0+%s\"\\n}\\n' \"$sum\" > plugin.json\n"

type mechFixture struct {
	t        *testing.T
	r        *refreshRepo
	previous string // the verified head: the tip of feature before the update
	devTip   string // the tip of dev the update merges
}

func newMechFixture(t *testing.T) *mechFixture {
	t.Helper()
	r := newRefreshRepoFormat(t, "sha1")
	f := &mechFixture{t: t, r: r}
	f.put("backlog.md", backlogBase)
	f.put("payload/one.txt", "one\n")
	f.put("payload/two.txt", "two\n")
	f.put("regen.sh", mechRegenScript)
	f.put("flaky.sh", "echo \"$$\" > plugin.json\n")
	f.put("append.sh", "echo more >> plugin.json\n")
	f.put("other.txt", "other\n")
	for _, list := range []string{"docs/list.md", "contract/golden/list.txt", "internal/relay/list.md"} {
		f.put(list, "# List\n- a\n")
	}
	f.regen()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "mechanical base")
	r.branchFrom("feature", "dev")
	return f
}

func (f *mechFixture) put(file, content string) {
	f.t.Helper()
	path := filepath.Join(f.r.path, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *mechFixture) read(file string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.r.path, file))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// regen is what the regeneration command does: it records the version of the payload now in the work tree.
func (f *mechFixture) regen() {
	f.t.Helper()
	cmd := exec.Command("sh", "regen.sh")
	cmd.Dir = f.r.path
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("regen.sh: %v\n%s", err, out)
	}
}

// commitOn writes files on a branch (and records the version again when regen is set) and returns the new tip.
func (f *mechFixture) commitOn(branch, message string, files map[string]string, regen bool) string {
	f.t.Helper()
	f.r.git("checkout", "-q", branch)
	for file, content := range files {
		f.put(file, content)
	}
	if regen {
		f.regen()
	}
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "-m", message)
	return f.r.git("rev-parse", "HEAD")
}

// standard is the usual picture: both sides append to backlog.md and edit a payload file, each
// recording its own version, so the merge conflicts in backlog.md and in plugin.json.
func (f *mechFixture) standard() {
	f.t.Helper()
	f.previous = f.commitOn("feature", "previous work", map[string]string{"backlog.md": backlogBase + "- p1\n- p2\n", "payload/one.txt": "one P\n"}, true)
	f.devTip = f.commitOn("dev", "dev work", map[string]string{"backlog.md": backlogBase + "- d1\n", "payload/two.txt": "two D\n"}, true)
}

// startMerge is the parent's update: merge dev into feature, which stops on the conflicts.
func (f *mechFixture) startMerge() {
	f.t.Helper()
	f.r.git("checkout", "-q", "feature")
	f.r.gitFails("merge", "--no-ff", "-m", "Merge branch 'dev' into feature", "dev")
}

// finish commits the work tree as the merge and returns the new head.
func (f *mechFixture) finish() string {
	f.t.Helper()
	f.r.git("add", "-A")
	f.r.git("commit", "-q", "-m", "Merge branch 'dev' into feature")
	return f.r.git("rev-parse", "HEAD")
}

// resolve settles the standard conflicts the way the rules say: the backlog as given, the version recorded again.
func (f *mechFixture) resolve(backlog string) string {
	f.t.Helper()
	f.put("backlog.md", backlog)
	f.regen()
	return f.finish()
}

// merged is the usual happy path: both sides' lines kept, the version recorded again on the merged tree.
func (f *mechFixture) merged() string {
	f.t.Helper()
	f.standard()
	f.startMerge()
	return f.resolve(backlogBase + "- p1\n- p2\n- d1\n")
}

type mechRegion struct {
	Repository string "json:\"repository\""
	Path       string "json:\"path\""
	Kind       string "json:\"kind\""
	Key        string "json:\"key,omitempty\""
	Change     string "json:\"change,omitempty\""
	Grade      string "json:\"grade\""
	Rule       string "json:\"rule,omitempty\""
}

// mechanical is a file region declared mechanical with its rule.
func mechanical(path, rule string) mechRegion {
	return mechRegion{Repository: mechRepository, Path: path, Kind: "file", Grade: "mechanical", Rule: rule}
}

func (f *mechFixture) regionsFile(regions ...mechRegion) string {
	f.t.Helper()
	data, err := json.Marshal(regions)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.regionsText(string(data))
}

func (f *mechFixture) regionsText(text string) string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "regions.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// standardRegions are the declarations of the usual node: the backlog is a union, the manifest is regenerated.
func (f *mechFixture) standardRegions() string {
	return f.regionsFile(mechanical("backlog.md", "union"), mechanical("plugin.json", "regenerate:sh regen.sh"))
}

// run asks the check about head, with the regions in the given files (the standard ones when there is none).
func (f *mechFixture) run(head string, regionFiles ...string) skillProcessResult {
	f.t.Helper()
	return f.runWith(head, regionFiles)
}

func (f *mechFixture) runWith(head string, regionFiles []string, extra ...string) skillProcessResult {
	f.t.Helper()
	if len(regionFiles) == 0 {
		regionFiles = []string{f.standardRegions()}
	}
	args := []string{"base-refresh", "mechanical", "--repo", f.r.path, "--previous", f.previous, "--head", head, "--base", "dev", "--repository", mechRepository, "--regenerate-timeout", "2m"}
	for _, file := range regionFiles {
		args = append(args, "--regions", file)
	}
	return runSkillInProcess(append(args, extra...)...)
}

func wantMechRefused(t *testing.T, got skillProcessResult, code string, says ...string) {
	t.Helper()
	if got.exit != 1 || !strings.HasPrefix(got.stdout, "refused: "+code+":") || strings.Contains(got.stdout, "\nok:") {
		t.Fatalf("want refused %s, got %+v", code, got)
	}
	for _, s := range says {
		if !strings.Contains(got.stdout, s) {
			t.Fatalf("the refusal lacks %q: %+v", s, got)
		}
	}
	if !strings.Contains(got.stdout, "\nfacts: previous=") || !strings.Contains(got.stdout, "\nsafe side: ") {
		t.Fatalf("the refusal lacks its facts or safe-side line: %+v", got)
	}
}

func (f *mechFixture) wantMechPasses(got skillProcessResult, head string, applied ...string) {
	f.t.Helper()
	tree := f.r.git("rev-parse", head+"^{tree}")
	lines := []string{
		"ok: " + head + " is " + f.previous + " plus the tip of dev (" + f.devTip + ")",
		"evidence: previous=" + f.previous + " dev_tip=" + f.devTip + " head=" + head + " tree=" + tree + " rule=mechanical_resolution\n",
		"parents: (" + f.previous + ", " + f.devTip + ") in that order\n",
	}
	lines = append(lines, applied...)
	if got.exit != 0 || strings.Contains(got.stdout, "refused") {
		f.t.Fatalf("want a pass, got %+v", got)
	}
	for _, line := range lines {
		if !strings.Contains(got.stdout, line) {
			f.t.Fatalf("the pass lacks %q: %+v", line, got)
		}
	}
}
