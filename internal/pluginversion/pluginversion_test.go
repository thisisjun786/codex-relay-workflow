package pluginversion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The version rules on a real temporary repository: the manifest records the version that names the
// payload of the working tree, and VersionOfTree answers the same version from the commit.

func pluginTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "dev"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		pluginGit(t, repo, args...)
	}
	return repo
}

func pluginGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func pluginWrite(t *testing.T, repo, file, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// manifestText is the fixture manifest with the version and description given.
func manifestText(version, description string) string {
	return "{\n  \"name\": \"crw\",\n  \"version\": \"" + version + "\",\n  \"description\": \"" + description + "\"\n}\n"
}

// recordPluginVersion writes the version that names the working tree's payload into the manifest, as
// crw-dev ci plugin --record-version does.
func recordPluginVersion(t *testing.T, repo string) string {
	t.Helper()
	p, errs := DirectoryPayload(filepath.Join(repo, filepath.FromSlash(PluginRelative)))
	if len(errs) > 0 {
		t.Fatalf("the fixture payload: %v", errs)
	}
	current, err := ManifestVersion(p)
	if err != nil {
		t.Fatal(err)
	}
	next, err := PayloadVersion(p, current)
	if err != nil {
		t.Fatal(err)
	}
	pluginWrite(t, repo, ManifestRepoPath, manifestText(next, "d"))
	return next
}

// pluginFixture is a repository with one package under plugins/crw whose manifest names its payload.
func pluginFixture(t *testing.T) string {
	t.Helper()
	repo := pluginTestRepo(t)
	pluginWrite(t, repo, ManifestRepoPath, manifestText("0.4.0", "d"))
	pluginWrite(t, repo, PluginRelative+"/LICENSE", "MIT\n")
	pluginWrite(t, repo, PluginRelative+"/skills/crw-run/SKILL.md", "---\nname: crw-run\ndescription: d\n---\n")
	recordPluginVersion(t, repo)
	pluginGit(t, repo, "add", "-A")
	pluginGit(t, repo, "commit", "-q", "-m", "package")
	return repo
}

func TestVersionOfTreeNamesThePayloadOfACommit(t *testing.T) {
	repo := pluginFixture(t)
	recorded := strings.TrimSpace(pluginGit(t, repo, "show", "HEAD:"+ManifestRepoPath))
	got, err := VersionOfTree(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded, "\"version\": \""+got+"\"") {
		t.Fatalf("VersionOfTree = %q, which is not the version the manifest records:\n%s", got, recorded)
	}
	// The same rules on the work tree, as crw-dev ci plugin reads it.
	p, errs := DirectoryPayload(filepath.Join(repo, filepath.FromSlash(PluginRelative)))
	if len(errs) > 0 {
		t.Fatalf("the work tree payload: %v", errs)
	}
	version, err := ManifestVersion(p)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := PayloadVersion(p, version); err != nil || again != got {
		t.Fatalf("the work tree derives %q, %v; want %q", again, err, got)
	}
}

func TestVersionOfTreeFollowsThePayload(t *testing.T) {
	repo := pluginFixture(t)
	before, err := VersionOfTree(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	pluginWrite(t, repo, PluginRelative+"/skills/crw-run/SKILL.md", "---\nname: crw-run\ndescription: d\n---\nchanged\n")
	after := recordPluginVersion(t, repo)
	pluginGit(t, repo, "add", "-A")
	pluginGit(t, repo, "commit", "-q", "-m", "changed")
	got, err := VersionOfTree(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got != after || got == before {
		t.Fatalf("VersionOfTree = %q after the change; want the newly recorded %q and not the old %q", got, after, before)
	}
	if old, err := VersionOfTree(context.Background(), repo, "HEAD~1"); err != nil || old != before {
		t.Fatalf("VersionOfTree of the earlier commit = %q, %v; want %q", old, err, before)
	}
}

func TestManifestVersionElidedComparesApartFromTheVersion(t *testing.T) {
	one, version, err := ManifestVersionElided([]byte(manifestText("0.4.0+aaaaaaaaaaaa", "d")))
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.4.0+aaaaaaaaaaaa" {
		t.Fatalf("the version is %q", version)
	}
	two, _, err := ManifestVersionElided([]byte(manifestText("0.4.0+bbbbbbbbbbbb", "d")))
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("two manifests that differ only in the version did not elide alike:\n%s\n%s", one, two)
	}
	changed, _, err := ManifestVersionElided([]byte(manifestText("0.4.0+aaaaaaaaaaaa", "other")))
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) == string(one) {
		t.Fatal("a manifest with another line changed elided alike")
	}
	if _, _, err := ManifestVersionElided([]byte("{\"name\": \"crw\"}")); err == nil {
		t.Fatal("a manifest that records no version was accepted")
	}
	if _, _, err := ManifestVersionElided([]byte("not json")); err == nil {
		t.Fatal("a manifest that is not JSON was accepted")
	}
}
