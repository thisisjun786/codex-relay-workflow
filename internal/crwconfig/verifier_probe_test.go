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

// probeHere is the probe as a run with env makes it. A root is judged by what it holds, never by where it is (CRW-1579), so the
// test's homes below TMPDIR, which may be below /tmp, need no seam.
func probeHere(env []string) VerifierProbe {
	return ProbeVerifierTools(context.Background(), env, "")
}

// CRW-1319 (evaluation a48fe518 d3): the probe of the verifier's tools tells what a run would use, and when no Go root is
// configured it looks for one: the root of the go on PATH and the usual install locations below the home and the system, each
// with its version, and the problem it reports names the command that records the first one. A root whose links resolve into
// another directory is offered like any other (CRW-1579: judged by content, not place; TestGoRootCandidates_judgeByContentNotPlace).
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
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// nothing configured, nothing on PATH: not runnable, and the Go root below the home is found and named; a root of the
	// usual locations whose link resolves into another directory is found too, newest first
	local := fakeGoAt(t, filepath.Join(home, ".local", "go", "go"), "go1.27.1")
	linked := fakeGoAt(t, filepath.Join(elsewhere, "go-sdk"), "go1.28.0")
	if err := os.MkdirAll(filepath.Join(home, "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(home, "sdk", "go1.28.0")); err != nil {
		t.Fatal(err)
	}
	p := probeHere(env)
	if p.Runnable || p.Mode != "path" || p.Go != "" || len(p.Problems) == 0 {
		t.Fatalf("nothing configured: %+v", p)
	}
	var found, foundLinked bool
	for _, c := range p.Candidates {
		found = found || (c.Root == local && c.Version == "go1.27.1")
		foundLinked = foundLinked || (c.Root == filepath.Join(home, "sdk", "go1.28.0") && c.Version == "go1.28.0")
	}
	if !found || len(p.Candidates) == 0 {
		t.Fatalf("the Go root below the home is not a candidate: %+v", p.Candidates)
	}
	if !foundLinked || p.Candidates[0].Version != "go1.28.0" {
		t.Errorf("a working root whose link resolves into another directory is not the newest candidate: %+v", p.Candidates)
	}
	if hint := strings.Join(p.Problems, "; "); !strings.Contains(hint, "crw config verifier --go-root "+p.Candidates[0].Root) {
		t.Errorf("the problems do not name the command that records the candidate: %s", hint)
	}

	// the go on PATH names its own root (go env GOROOT), and it is found as a candidate too; crw-dev is still not on PATH
	other := fakeGoAt(t, filepath.Join(home, "elsewhere", "go"), "go1.27.0")
	env[1] = "PATH=" + filepath.Join(other, "bin")
	p = probeHere(env)
	found = false
	for _, c := range p.Candidates {
		found = found || c.Root == other
	}
	if p.GoSource != "path" || p.GoVersion != "go1.27.0" || !found || len(p.Notes) == 0 {
		t.Errorf("go on PATH: %+v", p)
	}

	// the go on PATH in another directory runs and names its root, which is offered once (the home's link to it is the same
	// directory), first
	env[1] = "PATH=" + filepath.Join(linked, "bin")
	p = probeHere(env)
	var linkedCount int
	for _, c := range p.Candidates {
		if c.Version == "go1.28.0" {
			linkedCount++
		}
	}
	if p.GoVersion != "go1.28.0" || len(p.Candidates) == 0 || p.Candidates[0].Root != linked || linkedCount != 1 {
		t.Errorf("go on PATH in another directory: %+v", p)
	}

	// a configured root: runnable in tree mode, no problem, no note, no search
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+local+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env[1] = "PATH=" + empty
	p = probeHere(env)
	if !p.Runnable || p.Mode != "tree" || p.Go != filepath.Join(local, "bin", "go") || p.GoSource != "config" || p.GoVersion != "go1.27.1" ||
		len(p.Problems) != 0 || len(p.Notes) != 0 || len(p.Candidates) != 0 {
		t.Errorf("a configured root: %+v", p)
	}

	// a configured crw-dev that is not there is a problem naming it
	if err := os.WriteFile(config, []byte(`{"verifier":{"go_root":"`+local+`","crw_dev":"`+filepath.Join(home, "no-crw-dev")+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p = probeHere(env)
	if p.Runnable || p.Mode != "binary" || !strings.Contains(strings.Join(p.Problems, "; "), "crw-dev") {
		t.Errorf("a crw-dev that does not run: %+v", p)
	}
}

// CRW-1319, changed by CRW-1579 (U-A5-2, R1-4): the rule this test named, that a Go root below /tmp or /var/tmp is never offered,
// is gone; a root is judged by content, not place (TestGoRootCandidates_judgeByContentNotPlace). The test keeps its name and pins
// the change at both former places: a working root below /tmp and one below /var/tmp is offered, not skipped.
func TestVerifierCandidateSkipsATemporaryRoot(t *testing.T) {
	for _, place := range []string{"/tmp", "/var/tmp"} {
		t.Run(place, func(t *testing.T) {
			t.Setenv("TMPDIR", place)
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(home, place+"/") {
				t.Fatalf("the test's home %s is not below %s", home, place)
			}
			empty := filepath.Join(home, "empty-path")
			if err := os.MkdirAll(empty, 0o755); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(home, "config.json")
			if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			root := fakeGoAt(t, filepath.Join(home, ".local", "go", "go"), "go1.27.1")
			p := probeHere([]string{"HOME=" + home, "PATH=" + empty, "CRW_CONFIG=" + config})
			var found bool
			for _, c := range p.Candidates {
				found = found || (c.Root == root && c.Version == "go1.27.1")
			}
			if !found {
				t.Errorf("a working Go root below %s is skipped: %+v", place, p.Candidates)
			}
		})
	}
}

// CRW-1579 (U-A5-2, R1-4): a Go root is judged by what it holds, never by where it is. A working root (its bin/go an executable
// regular file that answers `go version`) below /tmp is offered like any other, the root of a go on PATH below /tmp too; a root
// whose go does not answer, one whose bin/go is not executable and one whose bin/go is not a regular file are left out wherever they
// are. The default temporary directory is a location, never a ground for leaving a root out.
func TestGoRootCandidates_judgeByContentNotPlace(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, "/tmp/") {
		t.Fatalf("the test's home %s is not below /tmp", home)
	}
	empty := filepath.Join(home, "empty-path")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	local := fakeGoAt(t, filepath.Join(home, ".local", "go", "go"), "go1.27.1")
	silent := filepath.Join(home, "sdk", "go1.28.0") // its go exits 1 and prints nothing
	if err := os.MkdirAll(filepath.Join(silent, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(silent, "bin", "go"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := fakeGoAt(t, filepath.Join(home, "sdk", "go1.29.0"), "go1.29.0") // its go answers but is not executable
	if err := os.Chmod(filepath.Join(plain, "bin", "go"), 0o644); err != nil {
		t.Fatal(err)
	}
	notFile := filepath.Join(home, "sdk", "go1.30.0") // its bin/go is a directory
	if err := os.MkdirAll(filepath.Join(notFile, "bin", "go"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := []string{"HOME=" + home, "PATH=" + empty, "CRW_CONFIG=" + config}
	p := probeHere(env)
	var found bool
	for _, c := range p.Candidates {
		found = found || (c.Root == local && c.Version == "go1.27.1")
		switch c.Root {
		case silent, plain, notFile:
			t.Errorf("a root whose bin/go does not answer as a go is a candidate: %+v", c)
		}
	}
	if !found {
		t.Fatalf("a working Go root below /tmp is not a candidate: %+v", p)
	}
	if hint := strings.Join(p.Problems, "; "); !strings.Contains(hint, "crw config verifier --go-root "+p.Candidates[0].Root) {
		t.Errorf("the problems do not name the command that records the candidate: %s", hint)
	}

	// the go on PATH below /tmp names its own root, which is offered
	other := fakeGoAt(t, filepath.Join(home, "elsewhere", "go"), "go1.26.0")
	env[1] = "PATH=" + filepath.Join(other, "bin")
	p = probeHere(env)
	found = false
	for _, c := range p.Candidates {
		found = found || (c.Root == other && c.Version == "go1.26.0")
	}
	if p.GoVersion != "go1.26.0" || !found {
		t.Errorf("the root of a go on PATH below /tmp is not a candidate: %+v", p)
	}

	// a TMPDIR the one place refuses: no root can be probed, and the problems say why instead of "none was found", with no go
	// on PATH (only the search probes) and with one (its own probe fails the same way)
	t.Setenv("TMPDIR", "rel")
	for _, path := range []string{empty, filepath.Join(other, "bin")} {
		env[1] = "PATH=" + path
		p = probeHere(env)
		problems := strings.Join(p.Problems, "; ")
		if len(p.Candidates) != 0 || !strings.Contains(problems, "the Go toolchain roots on this host could not be probed") ||
			!strings.Contains(problems, "tmpdir_not_absolute") || strings.Contains(problems, "no Go toolchain root was found") {
			t.Errorf("PATH=%s, a relative TMPDIR: %+v", path, p)
		}
	}
}
