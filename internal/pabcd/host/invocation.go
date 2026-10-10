package host

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// BinEnv overrides the crw command a directive names (CODEXCLAW_CXC in CXC).
const BinEnv = "CRW_BIN"

// Invocation is the command an emitted directive uses for crw on this machine (cxcInvocation of
// cxc-ops/src/cxc-resolve.ts, port decision 2): BinEnv, trimmed, unless it is blank, else the
// absolute path of the installer's runtime pointer, <home>/.local/share/crw-runtime/current/bin/crw,
// as one literal shell word (ShellWord). BinEnv is a command prefix of any number of words and is
// used as written. crw is not on PATH and has no payload dispatcher, so the oracle's PATH and
// dispatcher rungs are gone. Unlike Node's os.homedir() in Home, the home here must be absolute:
// HOME, else the account's passwd home. The oracle never fails, so an emit site decides what to do
// with the error that says to set BinEnv.
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
	return ShellWord(filepath.Join(home, ".local", "share", "crw-runtime", "current", "bin", "crw")), nil
}

// backtick is a backtick spelled without one: the shell's printf writes octal 140.
const backtick = `"$(printf '\140')"`

// ShellWord is s as one literal word of a POSIX shell (sh, dash, bash, zsh): single-quoted, with a
// single quote closed, escaped with a backslash and reopened. The oracle put the path in double quotes
// without escaping, so a dollar sign, backtick, double quote or backslash in the home was expanded or
// broke the command (CRW-1138). A backtick is written as a command substitution of printf, outside the
// quotes, so the word holds no backtick character: an emitted command sits in a Markdown code span,
// which a backtick would end. crw is released for Linux and macOS only (.goreleaser.yaml), whose hook
// and model shells are POSIX, so there is no win32 spelling.
func ShellWord(s string) string {
	parts := strings.Split(s, "`")
	for i, part := range parts {
		parts[i] = "'" + strings.ReplaceAll(part, "'", `'\''`) + "'"
	}
	return strings.Join(parts, backtick)
}
