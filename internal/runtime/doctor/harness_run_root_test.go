package doctor

// This file is the CRW-840 test for harnessRunRootScan's manifest judgement: a cached crw version
// counts as an installed root only when its .codex-plugin/plugin.json is a regular file. It is
// written red first against the baseline and green after the fix. HOME, CODEX_HOME and CRW_HOME all
// point into a temporary directory, so no resolution here can reach the developer's own Codex home;
// the cache the scan walks is <codexHome>/plugins/cache/<marketplace>/crw/<version>.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// harnessRunRootRegularHome is a temporary Codex home and the environment harnessRunRoot reads.
func harnessRunRootRegularHome(t *testing.T) (string, host.LookupEnv) {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	return codexHome, harnessRunEnv(map[string]string{
		"HOME":       home,
		"CODEX_HOME": codexHome,
		"CRW_HOME":   filepath.Join(home, "crw"),
	})
}

// harnessRunRootRegularVersion makes <codexHome>/plugins/cache/<marketplace>/crw/<version> and returns the
// version root the scan should answer for.
func harnessRunRootRegularVersion(t *testing.T, codexHome, marketplace, version string) string {
	t.Helper()
	root := filepath.Join(codexHome, "plugins", "cache", marketplace, "crw", version)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// harnessRunRootRegularManifest writes one version's regular .codex-plugin/plugin.json.
func harnessRunRootRegularManifest(t *testing.T, versionRoot, version string) {
	t.Helper()
	dir := filepath.Join(versionRoot, ".codex-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "{\"name\":\"crw\",\"version\":\"" + version + "\",\"hooks\":[]}"
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// harnessRunRootRegularManifestDir makes one version's manifest path a directory: a non-regular file the
// scan must not count.
func harnessRunRootRegularManifestDir(t *testing.T, versionRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(versionRoot, ".codex-plugin", "plugin.json"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// harnessRunRootRegularManifestLink makes one version's manifest path a link to target.
func harnessRunRootRegularManifestLink(t *testing.T, versionRoot, target string) {
	t.Helper()
	dir := filepath.Join(versionRoot, ".codex-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "plugin.json")); err != nil {
		t.Fatal(err)
	}
}

// TestHarnessRunRootRegularManifestBesideADirectoryManifest is the issue's case: a version whose
// manifest path is an empty directory is not an installed root, so the one regular manifest beside
// it is the single root (dev: the two-roots refusal).
func TestHarnessRunRootRegularManifestBesideADirectoryManifest(t *testing.T) {
	codexHome, env := harnessRunRootRegularHome(t)
	regular := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
	harnessRunRootRegularManifest(t, regular, "v1")
	harnessRunRootRegularManifestDir(t, harnessRunRootRegularVersion(t, codexHome, "m", "v2"))
	got, err := harnessRunRoot(HarnessOptions{}, env)
	if err != nil {
		t.Fatalf("harnessRunRoot: %v; want the regular v1 root", err)
	}
	if got != regular {
		t.Fatalf("harnessRunRoot = %q; want %q", got, regular)
	}
}

// TestHarnessRunRootControls pins the three neighbouring answers: two regular manifests stay the
// two-roots refusal, one regular manifest is that root, and none is the no-installed refusal.
func TestHarnessRunRootControls(t *testing.T) {
	t.Run("two_regular_manifests_stay_ambiguous", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		first := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, first, "v1")
		second := harnessRunRootRegularVersion(t, codexHome, "m", "v2")
		harnessRunRootRegularManifest(t, second, "v2")
		_, err := harnessRunRoot(HarnessOptions{}, env)
		if err == nil {
			t.Fatalf("two regular manifests answered a root instead of the two-roots refusal")
		}
		for _, want := range []string{"2 installed crw plugin roots", first, second} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
		}
	})
	t.Run("one_regular_manifest", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		only := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, only, "v1")
		got, err := harnessRunRoot(HarnessOptions{}, env)
		if err != nil || got != only {
			t.Fatalf("harnessRunRoot = %q, %v; want %q", got, err, only)
		}
	})
	t.Run("no_manifest", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		_, err := harnessRunRoot(HarnessOptions{}, env)
		if err == nil || !strings.Contains(err.Error(), "no installed crw plugin") {
			t.Fatalf("error %v; want the no-installed refusal", err)
		}
	})
}

// TestHarnessRunRootLinkRules keeps the link rules the issue leaves in place: a manifest link that
// resolves to a regular file counts, a link version directory that resolves to one counts, a
// dangling link is absent and a link to a directory is absent.
func TestHarnessRunRootLinkRules(t *testing.T) {
	t.Run("a_link_to_a_regular_file_counts", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		first := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, first, "v1")
		harnessRunRootRegularManifestLink(t, harnessRunRootRegularVersion(t, codexHome, "m", "v2"), filepath.Join(first, ".codex-plugin", "plugin.json"))
		_, err := harnessRunRoot(HarnessOptions{}, env)
		if err == nil || !strings.Contains(err.Error(), "2 installed crw plugin roots") {
			t.Fatalf("error %v; want the two-roots refusal", err)
		}
	})
	t.Run("a_link_version_directory_counts", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		first := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, first, "v1")
		real := filepath.Join(codexHome, "elsewhere", "v2")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		harnessRunRootRegularManifest(t, real, "v2")
		if err := os.Symlink(real, filepath.Join(codexHome, "plugins", "cache", "m", "crw", "v2")); err != nil {
			t.Fatal(err)
		}
		_, err := harnessRunRoot(HarnessOptions{}, env)
		if err == nil || !strings.Contains(err.Error(), "2 installed crw plugin roots") {
			t.Fatalf("error %v; want the two-roots refusal", err)
		}
	})
	t.Run("a_dangling_link_is_absent", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		only := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, only, "v1")
		harnessRunRootRegularManifestLink(t, harnessRunRootRegularVersion(t, codexHome, "m", "v2"), filepath.Join(codexHome, "absent.json"))
		got, err := harnessRunRoot(HarnessOptions{}, env)
		if err != nil || got != only {
			t.Fatalf("harnessRunRoot = %q, %v; want %q", got, err, only)
		}
	})
	t.Run("a_link_to_a_directory_is_absent", func(t *testing.T) {
		codexHome, env := harnessRunRootRegularHome(t)
		only := harnessRunRootRegularVersion(t, codexHome, "m", "v1")
		harnessRunRootRegularManifest(t, only, "v1")
		dir := filepath.Join(codexHome, "manifest-dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		harnessRunRootRegularManifestLink(t, harnessRunRootRegularVersion(t, codexHome, "m", "v2"), dir)
		got, err := harnessRunRoot(HarnessOptions{}, env)
		if err != nil || got != only {
			t.Fatalf("harnessRunRoot = %q, %v; want %q", got, err, only)
		}
	})
}

// TestHarnessRunRootUnreadableDirectoryIsAnError: a directory the scan cannot read may hide another
// installed root, so the scan refuses instead of picking a root from an incomplete count.
func TestHarnessRunRootUnreadableDirectoryIsAnError(t *testing.T) {
	for _, where := range []string{"marketplace", "plugin"} {
		t.Run(where, func(t *testing.T) {
			codexHome, env := harnessRunRootRegularHome(t)
			harnessRunRootRegularManifest(t, harnessRunRootRegularVersion(t, codexHome, "public", "v1"), "v1")
			hidden := filepath.Join(codexHome, "plugins", "cache", "local")
			if where == "plugin" {
				hidden = filepath.Join(hidden, "crw")
			}
			if err := os.MkdirAll(hidden, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(hidden, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })
			if _, err := os.ReadDir(hidden); err == nil {
				t.Skip("the directory stays readable despite mode 000 (privileged user)")
			}
			_, err := harnessRunRoot(HarnessOptions{}, env)
			if err == nil || !strings.Contains(err.Error(), "cannot read") {
				t.Fatalf("error %v; want the unreadable-cache refusal", err)
			}
		})
	}
}

// TestHarnessRunRootCLIDiagnosesTheRegularRoot is the user-visible case the issue reproduces: with
// one version's manifest path a directory, the command diagnoses the regular root and reports its
// version instead of refusing with the two-roots error.
func TestHarnessRunRootCLIDiagnosesTheRegularRoot(t *testing.T) {
	codexHome, env := harnessRunRootRegularHome(t)
	harnessRunRootRegularManifest(t, harnessRunRootRegularVersion(t, codexHome, "m", "v1"), "v1")
	harnessRunRootRegularManifestDir(t, harnessRunRootRegularVersion(t, codexHome, "m", "v2"))
	tmp := t.TempDir()
	states := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	var stdout, stderr bytes.Buffer
	RunHarnessDoctorCLI([]string{"--json"}, &stdout, &stderr, env, func() (string, error) { return tmp, nil }, harnessRunStub(states, "codex-cli 1.2.3\n"), time.Now())
	if strings.Contains(stderr.String(), "installed crw plugin") {
		t.Fatalf("stderr = %q; the command refused to pick the one regular root", stderr.String())
	}
	var report struct {
		PluginVersion *string `json:"pluginVersion"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if report.PluginVersion == nil || *report.PluginVersion != "v1" {
		t.Fatalf("pluginVersion = %v; want the regular root's v1", report.PluginVersion)
	}
}
