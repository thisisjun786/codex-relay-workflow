//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1579 (R2-2): the shell sites that make a temporary directory follow D1. An unset or empty
// TMPDIR is /tmp; a relative TMPDIR stops with tmpdir_not_absolute on stderr and exit status 2 before
// any directory is made; an absolute TMPDIR is used.

const (
	visualizerScript   = "plugins/crw/skills/crw-dev-visualizer/scripts/diagram-to-html.sh"
	tmpdirNotAbsoluteF = "tmpdir_not_absolute: TMPDIR=%s; set TMPDIR to an absolute directory"
)

// shellEnv is the exact environment of a shell-site run: a PATH (with fakeBin first when given) and,
// when tmpdir is set, a TMPDIR of that value (set but empty included).
func shellEnv(t *testing.T, fakeBin string, tmpdir *string) []string {
	t.Helper()
	path := os.Getenv("PATH")
	if fakeBin != "" {
		path = fakeBin + ":" + path
	}
	env := []string{"PATH=" + path, "HOME=" + t.TempDir()}
	if tmpdir != nil {
		env = append(env, "TMPDIR="+*tmpdir)
	}
	return env
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func ptr(s string) *string { return &s }

// runVisualizer runs diagram-to-html.sh in a fresh empty working directory and answers it.
func runVisualizer(t *testing.T, tmpdir *string) (result, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	work := t.TempDir()
	input := filepath.Join(t.TempDir(), "in.html")
	if err := os.WriteFile(input, []byte("<p>x</p>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runEnv(t, work, shellEnv(t, "", tmpdir), "bash", filepath.Join(repoRoot(), visualizerScript), "raw", input)
	return got, work
}

func TestVisualizerScript_TMPDIR(t *testing.T) {
	for _, rel := range []string{"rel/tmp", ".", "scratch"} {
		t.Run("relative "+rel+" is refused before any mkdir", func(t *testing.T) {
			got, work := runVisualizer(t, ptr(rel))
			if want := strings.Replace(tmpdirNotAbsoluteF, "%s", rel, 1); got.code != 2 || !strings.Contains(got.stderr, want) {
				t.Errorf("exit %d, stderr %q; want exit 2 naming %q", got.code, got.stderr, want)
			}
			if names := listing(t, work); len(names) != 0 {
				t.Errorf("the working directory holds %v: nothing may be made", names)
			}
		})
	}
	for name, tmpdir := range map[string]*string{"unset": nil, "empty": ptr("")} {
		t.Run(name+" uses /tmp", func(t *testing.T) {
			got, work := runVisualizer(t, tmpdir)
			out := strings.TrimSpace(got.stdout)
			if got.code != 0 || !strings.HasPrefix(out, "/tmp/codex-diagrams/") {
				t.Fatalf("exit %d, stdout %q, stderr %q; want an output under /tmp/codex-diagrams", got.code, got.stdout, got.stderr)
			}
			t.Cleanup(func() { os.Remove(out) })
			if names := listing(t, work); len(names) != 0 {
				t.Errorf("the working directory holds %v", names)
			}
		})
	}
	t.Run("an absolute TMPDIR is used", func(t *testing.T) {
		base := t.TempDir()
		got, _ := runVisualizer(t, &base)
		if want := filepath.Join(base, "codex-diagrams") + "/"; got.code != 0 || !strings.HasPrefix(strings.TrimSpace(got.stdout), want) {
			t.Errorf("exit %d, stdout %q; want an output under %s", got.code, got.stdout, want)
		}
	})
}

// fakeGit is a git that records the directory of each `git -C <dir>` call and answers success.
func fakeGit(t *testing.T) (bin, log string) {
	t.Helper()
	bin, log = t.TempDir(), filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\nif [ \"$1\" = -C ]; then echo \"$2\" >> '" + log + "'; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// runAuditWithGitignore runs the audit on a project that holds a .gitignore (so the .env check makes
// its scratch directory) and answers the result, the working directory and the scratch directories
// git was asked about.
func runAuditWithGitignore(t *testing.T, tmpdir *string) (result, string, []string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, log := fakeGit(t)
	got := runEnv(t, work, shellEnv(t, bin, tmpdir), "bash", filepath.Join(repoRoot(), scaffoldAuditScript), work)
	data, _ := os.ReadFile(log)
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return got, work, calls
}

func TestScaffoldAuditScript_TMPDIR(t *testing.T) {
	for _, rel := range []string{"rel/tmp", ".", "scratch"} {
		t.Run("relative "+rel+" is refused before any mkdir", func(t *testing.T) {
			got, work, calls := runAuditWithGitignore(t, ptr(rel))
			if want := strings.Replace(tmpdirNotAbsoluteF, "%s", rel, 1); got.code != 2 || !strings.Contains(got.stderr, want) {
				t.Errorf("exit %d, stderr %q; want exit 2 naming %q", got.code, got.stderr, want)
			}
			if names := listing(t, work); len(names) != 1 || names[0] != ".gitignore" {
				t.Errorf("the working directory holds %v: nothing may be made", names)
			}
			if len(calls) != 0 {
				t.Errorf("git was asked about %v: nothing may be made", calls)
			}
		})
	}
	for name, tmpdir := range map[string]*string{"unset": nil, "empty": ptr("")} {
		t.Run(name+" uses /tmp", func(t *testing.T) {
			got, _, calls := runAuditWithGitignore(t, tmpdir)
			if got.code != 0 && got.code != 1 {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
			if len(calls) == 0 || !strings.HasPrefix(calls[0], "/tmp/scaffold-audit.") {
				t.Errorf("scratch directories %v; want one under /tmp", calls)
			}
		})
	}
	t.Run("an absolute TMPDIR is used", func(t *testing.T) {
		base := t.TempDir()
		_, _, calls := runAuditWithGitignore(t, &base)
		if len(calls) == 0 || !strings.HasPrefix(calls[0], base+"/scaffold-audit.") {
			t.Errorf("scratch directories %v; want one under %s", calls, base)
		}
	})
}
