package crwconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGoAt makes a Go toolchain root at root whose bin/go prints version.
func fakeGoAt(t *testing.T, root, version string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = env ]; then echo '" + root + "'; else echo 'go version " + version + " linux/amd64'; fi\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// CRW-1319 (evaluation a48fe518 d3): the probe of the verifier's tools tells what a run would use, and when no Go root is
// configured it looks for one: the root of the go on PATH and the usual install locations below the home and the system, each
// with its version, and the problem it reports names the command that records the first one.
func TestVerifierProbeFindsAGoRoot(t *testing.T) {
	home := t.TempDir()
	empty := filepath.Join(home, "empty-path")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + empty, "CRW_CONFIG=" + config}

	// nothing configured, nothing on PATH: not runnable, and the Go root below the home is found and named
	local := fakeGoAt(t, filepath.Join(home, ".local", "go", "go"), "go1.27.1")
	p := ProbeVerifierTools(context.Background(), env, "")
	if p.Runnable || p.Mode != "path" || p.Go != "" || len(p.Problems) == 0 {
		t.Fatalf("nothing configured: %+v", p)
	}
	var found bool
	for _, c := range p.Candidates {
		found = found || (c.Root == local && c.Version == "go1.27.1")
	}
	if !found || len(p.Candidates) == 0 {
		t.Fatalf("the Go root below the home is not a candidate: %+v", p.Candidates)
	}
	if hint := strings.Join(p.Problems, "; "); !strings.Contains(hint, "crw config verifier --go-root "+p.Candidates[0].Root) {
		t.Errorf("the problems do not name the command that records the candidate: %s", hint)
	}

	// the go on PATH names its own root (go env GOROOT), and it is found as a candidate too; crw-dev is still not on PATH
	other := fakeGoAt(t, filepath.Join(home, "elsewhere", "go"), "go1.27.0")
	env[1] = "PATH=" + filepath.Join(other, "bin")
	p = ProbeVerifierTools(context.Background(), env, "")
	found = false
	for _, c := range p.Candidates {
		found = found || c.Root == other
	}
	if p.GoSource != "path" || p.GoVersion != "go1.27.0" || !found || len(p.Notes) == 0 {
		t.Errorf("go on PATH: %+v", p)
	}

	// a configured root: runnable in tree mode, no problem, no note, no search
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+local+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env[1] = "PATH=" + empty
	p = ProbeVerifierTools(context.Background(), env, "")
	if !p.Runnable || p.Mode != "tree" || p.Go != filepath.Join(local, "bin", "go") || p.GoSource != "config" || p.GoVersion != "go1.27.1" ||
		len(p.Problems) != 0 || len(p.Notes) != 0 || len(p.Candidates) != 0 {
		t.Errorf("a configured root: %+v", p)
	}

	// a configured crw-dev that is not there is a problem naming it
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+local+`","crw_dev":"`+filepath.Join(home, "no-crw-dev")+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p = ProbeVerifierTools(context.Background(), env, "")
	if p.Runnable || p.Mode != "binary" || !strings.Contains(strings.Join(p.Problems, "; "), "crw-dev") {
		t.Errorf("a crw-dev that does not run: %+v", p)
	}
}

// CRW-1319: a Go root that a reboot removes (below /tmp or /var/tmp, such as /tmp/go-sdk) is never offered as a candidate.
func TestVerifierCandidateSkipsATemporaryRoot(t *testing.T) {
	for root, want := range map[string]bool{"/tmp/go-sdk": true, "/tmp": true, "/var/tmp/go": true, "/home/u/.local/go/go": false, "/usr/local/go": false, "/tmpx/go": false} {
		if got := temporaryRoot(root); got != want {
			t.Errorf("temporaryRoot(%q) = %v, want %v", root, got, want)
		}
	}
}
