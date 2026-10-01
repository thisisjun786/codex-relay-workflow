package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

var testRoot, testBinary string

func TestMain(m *testing.M) {
	if os.Getenv("CRW30_CONTROLLER_CRASH_HOME") != "" {
		os.Exit(m.Run())
	}
	testsupport.Main(m, func(root string) (func() error, error) {
		repository, err := filepath.Abs("../../..")
		if err != nil {
			return nil, err
		}
		testRoot = repository
		// The executable lives in installation/codex_session_relay, a directory of its own, from
		// which installationId derives (the goldens spell it as a placeholder).
		installation := filepath.Join(root, "installation", "codex_session_relay")
		if err = os.MkdirAll(installation, 0o700); err != nil {
			return nil, err
		}
		// A copy, not a link: the installation is the executable's own directory.
		testBinary = filepath.Join(installation, "crw")
		if err := testsupport.CopyCRW(testBinary); err != nil {
			return nil, err
		}
		return nil, os.Symlink(testBinary, filepath.Join(installation, "codex-session-relay"))
	})
}
