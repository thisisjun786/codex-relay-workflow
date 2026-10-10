package host

import (
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// Root is a host directory resolved once from the environment: the path a reader lists and opens below,
// where the path came from (crwconfig's Source), and the variable that supplied it or its home. The path is
// kept as the environment spelled it: never cleaned and never decoded, so a spelling that mixes a symbolic
// link and ".." names the directory the kernel resolves it to, and bytes that are not UTF-8 stay the bytes on
// disk. Join builds a path below it the same way, so what a reader lists and what it opens are one directory.
type Root struct {
	Path     string
	Source   crwconfig.Source
	Variable string // CODEX_SQLITE_HOME, CODEX_HOME or HOME; empty for the account's passwd home
	// Note says why a fallback was taken where an earlier reading took another directory (an empty HOME,
	// which the oracle read as the working directory). A caller that reports the root may show it; nothing
	// moves or reads the other directory.
	Note string
}

// Join is crwconfig.JoinRoot below the root: raw text, nothing cleaned.
func (r Root) Join(rel ...string) string { return crwconfig.JoinRoot(r.Path, rel...) }

// RootError is why no root could be resolved: a variable or the account home names a relative directory,
// or there is no home at all. Its text names the variable to set.
type RootError struct{ Reason string }

func (e *RootError) Error() string { return e.Reason }

// CodexSQLiteRoot is the directory of the host's databases (state_<N>.sqlite, goals_1.sqlite):
// CODEX_SQLITE_HOME, else CODEX_HOME, else .codex below the home. CODEX_SQLITE_HOME is a database-only
// override and names no other profile directory. An empty variable counts as unset; a set one is used as
// given, whitespace included, and must be absolute, because a relative one would be read wherever the
// command happens to run (crwconfig's rule for its roots). The home is HostHome.
func CodexSQLiteRoot(env LookupEnv) (Root, error) { return codexSQLiteRoot(env, accountHome) }

func codexSQLiteRoot(env LookupEnv, account func() (string, error)) (Root, error) {
	for _, key := range []string{"CODEX_SQLITE_HOME", "CODEX_HOME"} {
		if value, _ := env(key); value != "" {
			if !filepath.IsAbs(value) {
				return Root{}, &RootError{key + " is not an absolute path; set it to the absolute directory of the native databases"}
			}
			return Root{Path: value, Source: crwconfig.SourceEnv, Variable: key}, nil
		}
	}
	home, err := hostHome(env, account)
	if err != nil {
		return Root{}, err
	}
	home.Path = home.Join(".codex")
	home.Source = crwconfig.SourceDefault
	return home, nil
}

// HostHome is the home the Codex host itself resolves (the dirs crate Codex uses): HOME when it is set and
// not empty, else the account's passwd home. Unlike Home (Node's os.homedir()), an empty HOME is not a
// directory relative to the working directory, and the home must be absolute.
func HostHome(env LookupEnv) (Root, error) { return hostHome(env, accountHome) }

func hostHome(env LookupEnv, account func() (string, error)) (Root, error) {
	value, set := env("HOME")
	if value != "" {
		if !filepath.IsAbs(value) {
			return Root{}, &RootError{"HOME is not an absolute path; set HOME, CODEX_HOME or CODEX_SQLITE_HOME to an absolute directory"}
		}
		return Root{Path: value, Source: crwconfig.SourceEnv, Variable: "HOME"}, nil
	}
	path, err := account()
	if err != nil || !filepath.IsAbs(path) {
		return Root{}, &RootError{"no absolute home directory: set HOME, CODEX_HOME or CODEX_SQLITE_HOME to an absolute directory"}
	}
	root := Root{Path: path, Source: crwconfig.SourceDefault}
	if set {
		root.Note = "HOME is set but empty: the account home " + path + " is used, as the Codex host uses it; a .codex below the working directory is not read"
	}
	return root, nil
}
