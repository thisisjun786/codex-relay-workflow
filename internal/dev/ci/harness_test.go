//go:build dev

package ci

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// crwDev is the crw-dev binary TestMain builds, so parity tests run the real command line.
var crwDev string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crw-dev-test-")
	if err != nil {
		panic(err)
	}
	crwDev = filepath.Join(dir, "crw-dev")
	build := exec.Command("go", "build", "-tags", "dev", "-o", crwDev, "./cmd/crw-dev")
	build.Dir = repoRoot()
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("building crw-dev: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// repoRoot is the checkout this package sits in.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// result is one command's observable outcome.
type result struct {
	code           int
	stdout, stderr string
}

func runCommand(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	return runEnv(t, dir, append(os.Environ(), env...), name, args...)
}

// runEnv runs a command with exactly env (nil: the test's environment).
func runEnv(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v", name, err)
	}
	return result{code, stdout.String(), stderr.String()}
}

// python runs a repository Python script (scripts/ci/<name>.py or another path).
func python(t *testing.T, dir string, env []string, script string, args ...string) result {
	t.Helper()
	return runCommand(t, dir, env, "python3", append([]string{filepath.Join(repoRoot(), script)}, args...)...)
}

// goCheck runs `crw-dev ci <check>`.
func goCheck(t *testing.T, dir string, env []string, check string, args ...string) result {
	t.Helper()
	return runCommand(t, dir, env, crwDev, append([]string{"ci", check}, args...)...)
}

// sameResult requires the Go command to match the Python script byte for byte.
func sameResult(t *testing.T, label string, py, got result) {
	t.Helper()
	if py != got {
		t.Errorf("%s: Go differs from Python\npython: %d\n%s\n%s\ngo:     %d\n%s\n%s", label,
			py.code, py.stdout, py.stderr, got.code, got.stdout, got.stderr)
	}
}

// fixtureRepo is a scratch Git repository like the Python tests' setUp.
type fixtureRepo struct {
	t    *testing.T
	root string
}

func newRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	r := &fixtureRepo{t, t.TempDir()}
	r.git("init", "-q")
	r.git("config", "user.name", "CI fixture")
	r.git("config", "user.email", "ci@example.invalid")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *fixtureRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *fixtureRepo) write(path, text string) {
	r.t.Helper()
	target := filepath.Join(r.root, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fixtureRepo) commit() {
	r.t.Helper()
	r.git("add", ".")
	r.git("commit", "-qm", "fixture")
}

func (r *fixtureRepo) head() string {
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}
