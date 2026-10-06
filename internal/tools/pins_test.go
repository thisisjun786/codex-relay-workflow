package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the module root, so the pin test reads the
// repository's own scripts/ci/secrets.sh rather than a copy.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// secretsShValue reads one shell assignment out of scripts/ci/secrets.sh.
func secretsShValue(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "ci", "secrets.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(name) + "=(.*)$")
	match := pattern.FindStringSubmatch(string(data))
	if match == nil {
		t.Fatalf("scripts/ci/secrets.sh does not assign %s", name)
	}
	value := strings.TrimSpace(match[1])
	value = strings.Trim(value, "\"")
	// The script interpolates ${scan_version} into the archive name, so the raw assignment is a
	// template; substitute the version the same way the script does.
	return strings.ReplaceAll(value, "${scan_version}", secretsShValueVersion(t))
}

func secretsShValueVersion(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "ci", "secrets.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile("(?m)^scan_version=(.*)$").FindStringSubmatch(string(data))
	if match == nil {
		t.Fatal("scripts/ci/secrets.sh does not assign scan_version")
	}
	return strings.Trim(strings.TrimSpace(match[1]), "\"")
}

// C4: the pin table and the CI secret scan must name the same gitleaks release. A divergence
// between pins.go and scripts/ci/secrets.sh fails here.
func TestPinsMatchTheCISecretsScript(t *testing.T) {
	pin, ok := Lookup("gitleaks")
	if !ok {
		t.Fatal("pins.go does not pin gitleaks")
	}
	if want := secretsShValue(t, "scan_version"); pin.Version != want {
		t.Errorf("pins.go pins gitleaks %s, scripts/ci/secrets.sh scans %s", pin.Version, want)
	}
	if want := secretsShValue(t, "scan_archive"); pin.ArchiveName() != want {
		t.Errorf("pins.go names %s, scripts/ci/secrets.sh downloads %s", pin.ArchiveName(), want)
	}
	if want := secretsShValue(t, "scan_digest"); pin.SHA256 != want {
		t.Errorf("pins.go pins sha256 %s, scripts/ci/secrets.sh verifies %s", pin.SHA256, want)
	}
}

// The pin row itself carries the release the issue fixed, and the archive URL is derived from it
// the way the CI script derives its download URL.
func TestPinRowMatchesTheDecidedRelease(t *testing.T) {
	pin, ok := Lookup("gitleaks")
	if !ok {
		t.Fatal("pins.go does not pin gitleaks")
	}
	if pin.Version != "8.30.1" || pin.ArchiveName() != "gitleaks_8.30.1_linux_x64.tar.gz" {
		t.Errorf("pin = %s / %s", pin.Version, pin.ArchiveName())
	}
	if pin.SHA256 != "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb" {
		t.Errorf("pin sha256 = %s", pin.SHA256)
	}
	if pin.Executable != "gitleaks" {
		t.Errorf("pin executable = %s", pin.Executable)
	}
	if got, want := pin.ArchiveURL(), "https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz"; got != want {
		t.Errorf("ArchiveURL() = %s, want %s", got, want)
	}
	if !pin.Supports("linux", "amd64") {
		t.Error("the pin does not support linux/amd64")
	}
	if pin.Supports("darwin", "arm64") || pin.Supports("windows", "amd64") {
		t.Error("the pin supports a platform it does not pin")
	}
}

// Lookup answers only the pinned names, and Pins is a stable ordered table.
func TestLookupOnlyAnswersPinnedNames(t *testing.T) {
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup answered an unpinned name")
	}
	if len(Pins) == 0 || Pins[0].Name != "gitleaks" {
		t.Fatalf("the pin table = %+v", Pins)
	}
}
