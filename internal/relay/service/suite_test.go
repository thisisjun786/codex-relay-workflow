package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

var testRoot, testBinary, testPython string
var buildEnvironment []string

func TestMain(m *testing.M) {
	if os.Getenv("CRW30_CONTROLLER_CRASH_HOME") != "" {
		os.Exit(m.Run())
	}
	buildEnvironment = os.Environ()
	root, err := filepath.Abs("../../..")
	if err != nil {
		panic(err)
	}
	testRoot = root
	testPython = filepath.Join(root, ".venv/bin/codex-session-relay")
	home, err := os.MkdirTemp("", "crw-service-test-")
	if err != nil {
		panic(err)
	}
	// The executable lives in installation/codex_session_relay. When pyoracle asks the live
	// Python (record and check), that directory is an unchanged copy of Python's package, so
	// both runtimes derive installationId from precisely the same physical directory; on replay
	// it holds the executable alone, and the recordings spell the ID as a placeholder.
	installation := filepath.Join(home, "installation", "codex_session_relay")
	if pyoracle.Live() {
		err = os.CopyFS(filepath.Join(home, "installation"), os.DirFS(filepath.Join(root, "packages/codex-session-relay/src")))
	} else {
		err = os.MkdirAll(installation, 0o700)
	}
	if err != nil {
		panic(err)
	}
	// A copy, not a link: the installation is the executable's own directory.
	testBinary = filepath.Join(installation, "crw")
	if err := testsupport.CopyCRW(testBinary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(home)
		os.Exit(1)
	}
	if err = os.Symlink(testBinary, filepath.Join(installation, "codex-session-relay")); err != nil {
		panic(err)
	}
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
		if err = os.Setenv(key, filepath.Join(home, key)); err != nil {
			panic(err)
		}
	}
	if err = os.Setenv("PYTHONPATH", filepath.Join(home, "installation")); err != nil {
		panic(err)
	}
	// Python children import the bridge from the checkout; they never write bytecode there.
	if err = os.Setenv("PYTHONDONTWRITEBYTECODE", "1"); err != nil {
		panic(err)
	}
	code := m.Run()
	if err = errors.Join(os.RemoveAll(home), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
