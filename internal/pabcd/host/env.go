// Package host reads the state the Codex host keeps and the PABCD ports depend on: the goals
// database, a transcript's tail and the command a directive names for crw. They are the Go form
// of CXC v0.2.40 pabcd-state/src/{goal-active,transcript}.ts and cxc-ops/src/cxc-resolve.ts.
// Every reader is read-only: none creates, migrates or repairs host state.
package host

import (
	"os/user"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// LookupEnv is os.LookupEnv: a test supplies a map through it, and an unset variable stays
// distinct from an empty one.
type LookupEnv func(key string) (value string, set bool)

// Home is Node's os.homedir(): HOME as given, even when empty, else the account's passwd entry. An
// empty or relative home is not repaired, so a directory under it resolves against the working
// directory, as the oracle's does.
func Home(env LookupEnv) (string, error) {
	if home, set := env("HOME"); set {
		return home, nil
	}
	return accountHome()
}

func accountHome() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	return account.HomeDir, nil
}

// CodexSQLiteHome is the directory of the host's databases: CODEX_SQLITE_HOME, else CODEX_HOME,
// else ~/.codex. An empty value counts as unset, as the oracle's `a || b` does.
func CodexSQLiteHome(env LookupEnv) (string, error) {
	for _, key := range []string{"CODEX_SQLITE_HOME", "CODEX_HOME"} {
		if value, _ := env(key); value != "" {
			return value, nil
		}
	}
	return homeDir(env, ".codex")
}

// CRWHome is the directory of CRW's per-user files: CRW_HOME unless it is blank, else ~/.crw
// (codexclawHome of recall/src/index-db.ts:20, which skill-search shares). The value is used as
// given, not trimmed; a consumer that trims, as subagent-config does, layers that on.
func CRWHome(env LookupEnv) (string, error) {
	if value, _ := env("CRW_HOME"); text.Trim(value) != "" {
		return value, nil
	}
	return homeDir(env, ".crw")
}

func homeDir(env LookupEnv, name string) (string, error) {
	home, err := Home(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name), nil
}
