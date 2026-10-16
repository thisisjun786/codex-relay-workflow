//go:build dev

package ci

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// The version internal/pluginversion recomputes from a commit is the version crw-dev ci plugin
// reports for that commit's work tree, so the base-refresh check and the packaging check cannot
// disagree about the same head.
func Test47_PLG_20_VersionOfTreeMatchesThePackagingCheck(t *testing.T) {
	r := pluginRepo(t, sourceFiles(t, goodFiles(t)))
	got := pluginCLI(t, r.root, "--json")
	if got.code != 0 {
		t.Fatalf("crw-dev ci plugin: %+v", got)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, got.stdout)
	}
	version, _ := report["version"].(string)
	if version == "" {
		t.Fatalf("the report names no version: %s", got.stdout)
	}
	fromTree, err := pluginversion.VersionOfTree(context.Background(), r.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "VersionOfTree", fromTree, version)
}

// The manifest's committed version is the fixed release (CRW-1582): branches that edit plugin
// files merge without touching it, the source check reads it, and the payload's version is
// derived into the installation artifacts - never back into the source.
func TestPluginSourceBranchesKeepFixedVersion(t *testing.T) {
	plain := goodFiles(t).with(manifestPath, dumps(testManifest()))
	r := pluginRepo(t, plain)
	base := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	fixed := r.git("show", "HEAD:plugins/crw/"+manifestPath)

	// two plugin-edit branches off the same base merge without ever touching the manifest
	r.git("checkout", "-q", "-b", "plugin-a")
	r.write("plugins/crw/skills/crw-run/references/a.md", "a\n")
	r.gitCommit("a")
	r.git("checkout", "-q", base)
	r.git("checkout", "-q", "-b", "plugin-b")
	r.write("plugins/crw/skills/crw-run/references/b.md", "b\n")
	r.gitCommit("b")
	r.git("checkout", "-q", base)
	r.git("checkout", "-q", "-b", "merged")
	r.git("merge", "--no-ff", "-m", "merge plugin-a", "plugin-a")
	r.git("merge", "--no-ff", "-m", "merge plugin-b", "plugin-b")

	if got := r.git("show", "HEAD:plugins/crw/"+manifestPath); got != fixed {
		t.Fatalf("the merged manifest is %q, want the fixed release %q", got, fixed)
	}
	if diff := r.git("diff", base, "HEAD", "--", "plugins/crw/"+manifestPath); strings.TrimSpace(diff) != "" {
		t.Fatalf("the manifest changed over the plugin merges:\n%s", diff)
	}
	got := pluginCLI(t, r.root, "--json")
	if got.code != 0 {
		t.Fatalf("crw-dev ci plugin: %+v", got)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, got.stdout)
	}
	want, err := pluginversion.VersionOfTree(context.Background(), r.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "the derived version", report["version"], want)
	expectEqual(t, "the source version", report["sourceVersion"], "0.1.0")
	if dirty := strings.TrimSpace(r.git("status", "--porcelain")); dirty != "" {
		t.Fatalf("the check left the tree dirty:\n%s", dirty)
	}
}

// `crw-dev ci plugin --stage dir` writes a local marketplace - the catalog and a plugins/crw
// payload stamped with the payload's derived version - without touching the source
// (CRW-1582). The stamped artifact passes the payload check; the source's fixed version does
// not change.
func TestPluginStageStampsOnlyArtifact(t *testing.T) {
	plain := goodFiles(t).with(manifestPath, dumps(testManifest()))
	r := pluginRepo(t, plain)
	fixed := r.git("show", "HEAD:plugins/crw/"+manifestPath)
	catalog := r.git("show", "HEAD:"+marketplacePath)
	derived := regexp.MustCompile(`^0\.1\.0\+[0-9a-f]{12}$`)

	dest := filepath.Join(t.TempDir(), "marketplace")
	got := pluginCLI(t, r.root, "--stage", dest, "--json")
	if got.code != 0 {
		t.Fatalf("crw-dev ci plugin --stage: %+v", got)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, got.stdout)
	}
	// the report carries the artifact's own file count and digest - the ones --payload recomputes
	// from the written bytes - and the plain-text run of the same command prints them too
	if files, _ := report["files"].(float64); files <= 0 {
		t.Fatalf("the report names %v files", report["files"])
	}
	if digest, _ := report["digest"].(string); len(digest) != 64 {
		t.Fatalf("the report's digest %q", digest)
	}
	text := pluginCLI(t, r.root, "--stage", filepath.Join(t.TempDir(), "marketplace-text"))
	if text.code != 0 || !strings.Contains(text.stdout, "files, digest") || strings.Contains(text.stdout, "%!") {
		t.Fatalf("the plain-text stage: %+v", text)
	}
	want, err := pluginversion.VersionOfTree(context.Background(), r.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	version, _ := report["version"].(string)
	if version != want || version == "0.1.0" || !derived.MatchString(version) {
		t.Fatalf("the staged version %q: want %q matching %s", version, want, derived)
	}
	// the artifact carries the derived version and the source's catalog, byte for byte
	manifestOut, err := os.ReadFile(filepath.Join(dest, "plugins", "crw", manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	var m doc
	if err := json.Unmarshal(manifestOut, &m); err != nil || m["version"] != version {
		t.Fatalf("the staged manifest (%v) does not carry %q:\n%s", err, version, manifestOut)
	}
	cat, err := os.ReadFile(filepath.Join(dest, marketplacePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(cat) != catalog {
		t.Errorf("the staged catalog %s is not the source's %s", cat, catalog)
	}
	if got := pluginPayload(t, filepath.Join(dest, "plugins", "crw")); got.code != 0 {
		t.Errorf("the stamped artifact fails the payload check: %+v", got)
	}
	// the source is what it was
	if got := r.git("show", "HEAD:plugins/crw/"+manifestPath); got != fixed {
		t.Errorf("the source manifest became %q, want %q", got, fixed)
	}
	if dirty := strings.TrimSpace(r.git("status", "--porcelain")); dirty != "" {
		t.Errorf("the stage left the tree dirty:\n%s", dirty)
	}
	// a changed payload derives a changed suffix, on another new destination
	r.write("plugins/crw/skills/crw-run/references/new.md", "n\n")
	r.gitCommit("changed payload")
	dest2 := filepath.Join(t.TempDir(), "marketplace2")
	got = pluginCLI(t, r.root, "--stage", dest2, "--json")
	if got.code != 0 {
		t.Fatalf("the second stage: %+v", got)
	}
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatal(err)
	}
	if version2, _ := report["version"].(string); version2 == version || !derived.MatchString(version2) {
		t.Errorf("the changed payload staged %q, want a new derived suffix", version2)
	}
	if got := r.git("show", "HEAD:plugins/crw/"+manifestPath); got != fixed {
		t.Errorf("the source manifest became %q", got)
	}
	// a staged payload that no longer matches its version does not pass --payload
	if err := os.WriteFile(filepath.Join(dest, "plugins", "crw", "LICENSE"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pluginPayload(t, filepath.Join(dest, "plugins", "crw")); got.code == 0 {
		t.Errorf("a tampered artifact passed --payload")
	}
	// a destination that exists is refused, and what it held stands
	dest3 := filepath.Join(t.TempDir(), "marketplace")
	if err := os.MkdirAll(dest3, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dest3, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pluginCLI(t, r.root, "--stage", dest3); got.code == 0 {
		t.Errorf("staging onto an existing directory succeeded")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep\n" {
		t.Errorf("the staged-over sentinel is %q (%v)", data, err)
	}
	// a destination inside the source plugin is refused before it is made
	inside := filepath.Join(r.root, "plugins", "crw", "marketplace")
	if got := pluginCLI(t, r.root, "--stage", inside); got.code == 0 {
		t.Errorf("staging into the source plugin succeeded")
	}
	if _, err := os.Stat(inside); err == nil {
		t.Errorf("the refused destination was made: %s", inside)
	}
	// a source manifest that already carries a derived version is not a fixed release
	bad := pluginRepo(t, goodFiles(t).with(manifestPath, dumps(testManifest().set("version", "0.1.0+0123456789ab"))))
	dest4 := filepath.Join(t.TempDir(), "marketplace")
	if got := pluginCLI(t, bad.root, "--stage", dest4); got.code == 0 {
		t.Errorf("staging a derived source version succeeded")
	}
	if _, err := os.Stat(dest4); err == nil {
		t.Errorf("the refused source staged an artifact at %s", dest4)
	}
}

// --record-version no longer writes the payload's version into the source (CRW-1582): the
// source keeps a fixed release, and the derived version lands in staged artifacts instead.
// The flag refuses, pointing at --stage, and the checkout is untouched.
func TestPluginRecordVersionCannotRewriteSource(t *testing.T) {
	plain := goodFiles(t).with(manifestPath, dumps(testManifest()))
	r := pluginRepo(t, plain)
	file := filepath.Join(r.root, "plugins", "crw", manifestPath)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := pluginCLI(t, r.root, "--record-version")
	if got.code == 0 {
		t.Fatalf("ci plugin --record-version rewrote the source: %s", got.stdout)
	}
	if !strings.Contains(got.stdout+got.stderr, "--stage") {
		t.Errorf("the refusal does not name --stage:\n%s\n%s", got.stdout, got.stderr)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("the manifest was rewritten to %q", after)
	}
	if dirty := strings.TrimSpace(r.git("status", "--porcelain")); dirty != "" {
		t.Errorf("the refused flag left the tree dirty:\n%s", dirty)
	}
}
