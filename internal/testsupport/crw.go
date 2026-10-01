package testsupport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// CRWBinaryEnv names the crw binary every test runs instead of building its own. `make test`
// builds ./cmd/crw once, release-shaped (-trimpath, as `make build` and goreleaser build it), and
// passes it to every package, so no package spends its time linking another copy. A relative
// path is taken from the package directory the test runs in.
const CRWBinaryEnv = "CRW_TEST_BINARY"

// CRWDevBinaryEnv names the crw-dev binary the tests run instead of building their own, as
// CRWBinaryEnv names crw.
const CRWDevBinaryEnv = "CRW_TEST_DEV_BINARY"

// startEnviron and startDir are the environment and working directory this test process
// started with, taken before any TestMain isolates HOME or the XDG directories: a build under
// them finds this checkout and reuses the invoking toolchain's module and build caches instead
// of compiling from a cold cache.
var (
	startEnviron = os.Environ()
	startDir, _  = os.Getwd()
	goTool, _    = exec.LookPath("go")
)

// CRW is the crw binary under test: $CRW_TEST_BINARY when set, else ./cmd/crw built once per
// test process (with -trimpath), which Main removes. A test that needs the binary under a name or
// at a place of its own copies it there with CRWAt.
func CRW(t testing.TB) string {
	t.Helper()
	path, err := CRWPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// CRWPath is CRW for callers without a test, such as a TestMain.
func CRWPath() (string, error) { return named(CRWBinaryEnv, "./cmd/crw", "-trimpath") }

// CRWDevPath is the development binary (./cmd/crw-dev, -tags dev) the tests run:
// $CRW_TEST_DEV_BINARY when set, else built once per test process, which Main removes.
func CRWDevPath() (string, error) { return named(CRWDevBinaryEnv, "./cmd/crw-dev", "-tags", "dev") }

// BuildCRW is ./cmd/crw built with the go build flags given, once per test process for each
// distinct set of flags, for a test that runs a seam a release build leaves out (a link-time
// clock, a build tag, an overlay). Main removes it. Every other test runs CRW.
func BuildCRW(t testing.TB, flags ...string) string {
	t.Helper()
	path, err := BuildCRWPath(flags...)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// BuildCRWPath is BuildCRW for callers without a test.
func BuildCRWPath(flags ...string) (string, error) { return built("./cmd/crw", flags) }

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
func CopyCRW(path string) error {
	source, err := CRWPath()
	if err != nil {
		return err
	}
	return CopyBinary(source, path)
}

// CopyBinary copies the executable at source to path, which must not exist yet, creating its
// directory.
func CopyBinary(source, path string) (err error) {
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

// build is one binary this process builds at most once.
type build struct {
	once sync.Once
	path string
	err  error
}

var (
	buildMu   sync.Mutex
	builds    = map[string]*build{}
	buildDirs []string // the directories of the binaries this process built, which removeBuilds removes
)

// named is the binary env names when it is set, else pkg built with flags, which a test process
// this one starts (a test binary rerun as a helper) then finds under env.
func named(env, pkg string, flags ...string) (string, error) {
	if named := os.Getenv(env); named != "" {
		// A relative override names a file from the package directory the process started in,
		// whatever directory a test has changed into since.
		path := named
		if !filepath.IsAbs(path) {
			path = filepath.Join(startDir, path)
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("testsupport: %s: %w", env, err)
		}
		return path, nil
	}
	path, err := built(pkg, flags)
	if err != nil {
		return "", err
	}
	return path, os.Setenv(env, path)
}

// built is pkg built with flags, once per process.
func built(pkg string, flags []string) (string, error) {
	key := strings.Join(append([]string{pkg}, flags...), "\x00")
	buildMu.Lock()
	b := builds[key]
	if b == nil {
		b = &build{}
		builds[key] = b
	}
	buildMu.Unlock()
	b.once.Do(func() { b.path, b.err = goBuild(pkg, flags) })
	return b.path, b.err
}

func goBuild(pkg string, flags []string) (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	if goTool == "" {
		return "", fmt.Errorf("testsupport: no go tool on PATH to build %s", pkg)
	}
	dir, err := os.MkdirTemp("", "crw-test-binary-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(pkg))
	args := append(append([]string{"build"}, flags...), "-o", path, pkg)
	command := exec.Command(goTool, args...)
	command.Dir, command.Env = root, startEnviron
	if output, err := command.CombinedOutput(); err != nil {
		return "", errors.Join(fmt.Errorf("testsupport: go %s: %w\n%s", strings.Join(args, " "), err, output), os.RemoveAll(dir))
	}
	buildMu.Lock()
	buildDirs = append(buildDirs, dir)
	buildMu.Unlock()
	return path, nil
}

// removeBuilds removes every binary this process built; a binary CRWBinaryEnv or CRWDevBinaryEnv
// names is never touched.
func removeBuilds() error {
	buildMu.Lock()
	defer buildMu.Unlock()
	var errs []error
	for _, dir := range buildDirs {
		errs = append(errs, os.RemoveAll(dir))
	}
	buildDirs = nil
	return errors.Join(errs...)
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
