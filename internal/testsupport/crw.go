package testsupport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// CRWBinaryEnv names the crw binary every test runs instead of building its own. `make test`
// builds ./cmd/crw once, release-shaped (-trimpath, as `make build` and goreleaser build it), and
// passes it to every package, so no package spends its time linking another copy. A relative
// path is taken from the package directory the test runs in.
const CRWBinaryEnv = "CRW_TEST_BINARY"

// startEnviron and startDir are the environment and working directory this test process
// started with, taken before any TestMain isolates HOME or the XDG directories: a build under
// them finds this checkout and reuses the invoking toolchain's module and build caches instead
// of compiling from a cold cache.
var (
	startEnviron = os.Environ()
	startDir, _  = os.Getwd()
	goTool, _    = exec.LookPath("go")
)

var (
	crwBuilt = sync.OnceValues(buildCRW)
	crwMu    sync.Mutex
	crwDir   string // the directory of the copy this process built, which RemoveCRW removes
)

// CRW is the crw binary under test: $CRW_TEST_BINARY when set, else ./cmd/crw built once per
// test process (with -trimpath), which the package's TestMain removes with RemoveCRW. A test that
// needs the binary under a name or at a place of its own copies it there with CRWAt.
func CRW(t testing.TB) string {
	t.Helper()
	path, err := CRWPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// CRWPath is CRW for callers without a test, such as a TestMain.
func CRWPath() (string, error) { return crwBuilt() }

// CRWAt copies the crw under test to path, creating its directory, and returns path. The copy
// is a regular file, so the binary's own location (os.Executable) is path, not the shared copy.
func CRWAt(t testing.TB, path string) string {
	t.Helper()
	if err := CopyCRW(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// CopyCRW is CRWAt for callers without a test.
func CopyCRW(path string) (err error) {
	source, err := CRWPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

// RemoveCRW removes the crw this process built, if it built one; the binary CRWBinaryEnv names
// is never touched. A TestMain calls it after m.Run.
func RemoveCRW() error {
	crwMu.Lock()
	defer crwMu.Unlock()
	if crwDir == "" {
		return nil
	}
	err := os.RemoveAll(crwDir)
	crwDir = ""
	return err
}

func buildCRW() (string, error) {
	if named := os.Getenv(CRWBinaryEnv); named != "" {
		path, err := filepath.Abs(named)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("testsupport: %s: %w", CRWBinaryEnv, err)
		}
		return path, nil
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	if goTool == "" {
		return "", errors.New("testsupport: no go tool on PATH to build ./cmd/crw")
	}
	dir, err := os.MkdirTemp("", "crw-test-binary-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "crw")
	build := exec.Command(goTool, "build", "-trimpath", "-o", path, "./cmd/crw")
	build.Dir, build.Env = root, startEnviron
	if output, err := build.CombinedOutput(); err != nil {
		return "", errors.Join(fmt.Errorf("testsupport: go build ./cmd/crw: %w\n%s", err, output), os.RemoveAll(dir))
	}
	crwMu.Lock()
	crwDir = dir
	crwMu.Unlock()
	// A test process this one starts (a test binary rerun as a helper) reuses the build.
	return path, os.Setenv(CRWBinaryEnv, path)
}

// moduleRoot is the directory holding go.mod at or above the directory the test started in.
func moduleRoot() (string, error) {
	for dir := startDir; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("testsupport: no go.mod at or above %s", startDir)
		}
	}
}
