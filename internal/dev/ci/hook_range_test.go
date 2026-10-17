//go:build dev

package ci

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const hookRangeScannerScript = `#!/bin/sh
if [ "$1" = version ]; then
  echo "${CRW1598_SCANNER_VERSION:-8.30.1}"
  exit 0
fi
if [ "$1" != git ]; then
  echo "hook-range scanner: unexpected command $1" >&2
  exit 2
fi
opts=
for arg in "$@"; do
  case $arg in --log-opts=*) opts=${arg#--log-opts=} ;; esac
done
[ -n "$opts" ] || { echo 'hook-range scanner: no --log-opts' >&2; exit 2; }
if [ -n "${CRW1598_SCANNER_ARGV:-}" ]; then
  printf '%s\n' "$@" >> "$CRW1598_SCANNER_ARGV"
fi
if [ -n "${CRW1598_SCANNER_REVS:-}" ]; then
  git -C "$2" log --format=%H $opts > "$CRW1598_SCANNER_REVS" 2>/dev/null || exit 2
fi
if [ -n "${CRW1598_MARKER:-}" ] && git -C "$2" log -p -U0 $opts | grep -qF "$CRW1598_MARKER"; then
  echo "hook-range scanner: a leak"
  exit 1
fi
exit 0
`

func hookRangeScanner(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitleaks")
	if err := os.WriteFile(path, []byte(hookRangeScannerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func hookRun(t *testing.T, root string, argv []string, stdin string, env []string) result {
	t.Helper()
	path := mustHookPath(t, root)
	cmd := exec.Command("bash", append([]string{path}, argv...)...)
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running the hook: %v", err)
	}
	return result{code: code, stdout: out.String()}
}

func hookRangeEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + t.TempDir(),
		"CRW_CI_GITLEAKS=" + hookRangeScanner(t),
		"CRW1598_SCANNER_VERSION=" + localGitleaksPin,
		"CRW1598_MARKER=CRW1598-LEAKED-VALUE",
	}
	return append(env, extra...)
}

func hookRangeCapture(t *testing.T) (argvFile, revsFile string, env []string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	revsFile = filepath.Join(dir, "revs")
	env = []string{"CRW1598_SCANNER_ARGV=" + argvFile, "CRW1598_SCANNER_REVS=" + revsFile}
	return argvFile, revsFile, env
}

func hookRangeLogOpts(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the scanner recorded no argv: %v", err)
	}
	var opts []string
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "--log-opts="); ok {
			opts = append(opts, v)
		}
	}
	return opts
}

func hookRangeRevs(t *testing.T, revsFile string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(revsFile)
	if err != nil {
		t.Fatalf("the scanner recorded no revisions: %v", err)
	}
	revs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			revs[line] = true
		}
	}
	return revs
}

func hookGitShim(t *testing.T, mode string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var body string
	switch mode {
	case "for-each-ref":
		body = "for arg in \"$@\"; do\n  if [ \"$arg\" = for-each-ref ]; then\n    exit 1\n  fi\ndone\n"
	case "rev-list-exclusion":
		body = "saw_list=false\nsaw_not=false\nfor arg in \"$@\"; do\n  [ \"$arg\" = rev-list ] && saw_list=true\n  [ \"$arg\" = --not ] && saw_not=true\ndone\nif $saw_list && $saw_not; then\n  exit 1\nfi\n"
	default:
		t.Fatalf("unknown git shim mode %q", mode)
	}
	dir := t.TempDir()
	shim := "#!/bin/sh\n" + body + "exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHookRangeNewRemoteExcludesPublished(t *testing.T) {
	for _, zeros := range []struct{ name, sha string }{
		{"sha1", strings.Repeat("0", 40)},
		{"sha256", strings.Repeat("0", 64)},
	} {
		t.Run(zeros.name, func(t *testing.T) {
			repo := newRepo(t)
			repo.write("secret.txt", "token = CRW1598-LEAKED-VALUE\n")
			repo.write("big.bin", strings.Repeat("x", (2<<20)+1))
			repo.commit()
			published := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			repo.git("update-ref", "refs/remotes/publish/dev", published)
			repo.write("clean.txt", "nothing new\n")
			repo.commit()
			head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			repo.git("update-ref", "refs/remotes/origin/dev", head)
			if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
				t.Fatal(err)
			}
			argvFile, revsFile, capture := hookRangeCapture(t)
			stdin := fmt.Sprintf("refs/heads/feature %s refs/heads/feature %s\n", head, zeros.sha)
			res := hookRun(t, repo.root, []string{"publish"}, stdin, hookRangeEnv(t, capture...))
			if res.code != 0 {
				t.Fatalf("a push of unpublished commits only was blocked:\n%s", res.stdout)
			}
			opts := hookRangeLogOpts(t, argvFile)
			want := "-m " + head + " --not --remotes=publish"
			if len(opts) != 1 || opts[0] != want {
				t.Fatalf("scanner log-opts = %v, want [%q]", opts, want)
			}
			revs := hookRangeRevs(t, revsFile)
			if !revs[head] {
				t.Errorf("the scanned commits do not contain the pushed head %s", head)
			}
			if revs[published] {
				t.Errorf("the scanned commits contain the already-published %s", published)
			}
		})
	}
}

func TestHookRangeNewRemoteBlocksNewFindings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		content string
		want    string
	}{
		{"secret", "secret.txt", "token = CRW1598-LEAKED-VALUE\n", "Gitleaks found a secret"},
		{"blob", "big.bin", strings.Repeat("x", (2<<20)+1), "blob over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			repo.write("base.txt", "clean\n")
			repo.commit()
			published := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			repo.git("update-ref", "refs/remotes/publish/dev", published)
			repo.write(tc.file, tc.content)
			repo.commit()
			if err := os.Remove(filepath.Join(repo.root, tc.file)); err != nil {
				t.Fatal(err)
			}
			repo.commit()
			head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
				t.Fatal(err)
			}
			stdin := fmt.Sprintf("refs/heads/feature %s refs/heads/feature %s\n", head, strings.Repeat("0", 40))
			res := hookRun(t, repo.root, []string{"publish"}, stdin, hookRangeEnv(t))
			if res.code == 0 {
				t.Fatalf("a push carrying a new %s went through:\n%s", tc.name, res.stdout)
			}
			if !strings.Contains(res.stdout, tc.want) {
				t.Errorf("the refusal does not name the %s finding:\n%s", tc.name, res.stdout)
			}
		})
	}
}

func TestHookRangeFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		argv       []string
		publishRef bool
		otherRef   bool
		shim       string
	}{
		{"remote has no refs", []string{"publish"}, false, false, ""},
		{"only another remote has refs", []string{"publish"}, false, true, ""},
		{"missing remote name", nil, true, false, ""},
		{"url instead of a remote name", []string{"ssh://git@example.invalid/repo.git"}, true, false, ""},
		{"for-each-ref fails", []string{"publish"}, true, false, "for-each-ref"},
		{"excluded range unreadable", []string{"publish"}, true, false, "rev-list-exclusion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			repo.write("secret.txt", "token = CRW1598-LEAKED-VALUE\n")
			repo.commit()
			marked := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			if tc.publishRef {
				repo.git("update-ref", "refs/remotes/publish/dev", marked)
			}
			if tc.otherRef {
				repo.git("update-ref", "refs/remotes/origin/dev", marked)
			}
			repo.write("clean.txt", "nothing new\n")
			repo.commit()
			head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
			if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
				t.Fatal(err)
			}
			var extra []string
			if tc.shim != "" {
				extra = append(extra, "PATH="+hookGitShim(t, tc.shim)+":"+os.Getenv("PATH"))
			}
			argvFile, _, capture := hookRangeCapture(t)
			env := hookRangeEnv(t, append(capture, extra...)...)
			stdin := fmt.Sprintf("refs/heads/feature %s refs/heads/feature %s\n", head, strings.Repeat("0", 40))
			res := hookRun(t, repo.root, tc.argv, stdin, env)
			if res.code == 0 {
				t.Fatalf("a fallback push over the whole history went through:\n%s", res.stdout)
			}
			if !strings.Contains(res.stdout, "Gitleaks found a secret") {
				t.Errorf("the refusal does not name the finding:\n%s", res.stdout)
			}
			opts := hookRangeLogOpts(t, argvFile)
			want := "-m " + head
			if len(opts) != 1 || opts[0] != want {
				t.Errorf("scanner log-opts = %v, want [%q]", opts, want)
			}
		})
	}
}

func TestHookRangeExistingRef(t *testing.T) {
	repo := newRepo(t)
	repo.write("base.txt", "clean\n")
	repo.commit()
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.write("secret.txt", "token = CRW1598-LEAKED-VALUE\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	argvFile, _, capture := hookRangeCapture(t)
	stdin := fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", head, base)
	res := hookRun(t, repo.root, []string{"origin"}, stdin, hookRangeEnv(t, capture...))
	if res.code == 0 {
		t.Fatalf("a push carrying the marker went through:\n%s", res.stdout)
	}
	opts := hookRangeLogOpts(t, argvFile)
	want := "-m " + base + ".." + head
	if len(opts) != 1 || opts[0] != want {
		t.Errorf("scanner log-opts = %v, want [%q]", opts, want)
	}
}

func TestHookRangeUpgrade(t *testing.T) {
	repo := newRepo(t)
	repo.write("secret.txt", "token = CRW1598-LEAKED-VALUE\n")
	repo.write("big.bin", strings.Repeat("x", (2<<20)+1))
	repo.commit()
	published := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.git("update-ref", "refs/remotes/publish/dev", published)
	repo.write("clean.txt", "nothing new\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	path := mustHookPath(t, repo.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	older := "#!/usr/bin/env bash\n" + localHookMarker + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(older), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := localHookInstall(path); err != nil {
		t.Fatalf("the install did not upgrade an owned hook: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte(localHookScript)) {
		t.Fatal("the upgraded hook is not the current script")
	}
	stdin := fmt.Sprintf("refs/heads/feature %s refs/heads/feature %s\n", head, strings.Repeat("0", 40))
	res := hookRun(t, repo.root, []string{"publish"}, stdin, hookRangeEnv(t))
	if res.code != 0 {
		t.Fatalf("the upgraded hook blocked a push of unpublished commits only:\n%s", res.stdout)
	}
}
