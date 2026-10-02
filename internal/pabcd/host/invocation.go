package host

import (
	"errors"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// BinEnv overrides the crw command a directive names (CODEXCLAW_CXC in CXC).
const BinEnv = "CRW_BIN"

// Invocation is the command an emitted directive uses for crw on this machine (cxcInvocation of
// cxc-ops/src/cxc-resolve.ts, port decision 2): BinEnv, trimmed, unless it is blank, else the
// absolute, double-quoted path of the installer's runtime pointer,
// <home>/.local/share/crw-runtime/current/bin/crw. crw is not on PATH and has no payload
// dispatcher, so the oracle's PATH and dispatcher rungs are gone. Unlike Node's os.homedir() in
// Home, the home here must be absolute: HOME, else the account's passwd home. The oracle never
// fails, so an emit site decides what to do with the error that says to set BinEnv.
func Invocation(env LookupEnv) (string, error) { return invocationFrom(env, accountHome) }

func invocationFrom(env LookupEnv, account func() (string, error)) (string, error) {
	if value, _ := env(BinEnv); text.Trim(value) != "" {
		return text.Trim(value), nil
	}
	home, _ := env("HOME")
	if !filepath.IsAbs(home) {
		var err error
		if home, err = account(); err != nil || !filepath.IsAbs(home) {
			return "", errors.New("no absolute home directory holds the crw runtime pointer: set " + BinEnv)
		}
	}
	return `"` + filepath.Join(home, ".local", "share", "crw-runtime", "current", "bin", "crw") + `"`, nil
}
