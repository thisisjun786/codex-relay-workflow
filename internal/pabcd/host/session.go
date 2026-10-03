package host

import (
	"database/sql"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Refusal is why a native session could not be verified. Its text is the oracle's message, shown to
// the person as is. Every Refusal is one of the constants below, so no value read from the database
// can reach it.
type Refusal string

func (r Refusal) Error() string { return string(r) }

// NativeSession is a Codex session whose id and working directory the thread database confirms.
type NativeSession struct{ SessionID, Cwd, DBPath string }

const (
	noThreadID    = Refusal("CODEX_THREAD_ID is absent. Run this command inside the native Codex session.")
	badThreadID   = Refusal("CODEX_THREAD_ID must be an unmodified native UUID.")
	noWorkdir     = Refusal("Cannot resolve the working directory. Run from the native session's directory.")
	noStateDB     = Refusal("Cannot locate the native state database. Check CODEX_SQLITE_HOME or CODEX_HOME.")
	noStateFile   = Refusal("Native state database is missing. Check CODEX_SQLITE_HOME or CODEX_HOME.")
	notRegular    = Refusal("Newest native state database must be a regular file, not a symlink or directory.")
	unreadable    = Refusal("Cannot read the newest native state database or its threads schema. Check database access and Node SQLite support.")
	noRow         = Refusal("Native session row is missing. Run inside the intended Codex session.")
	archivedRow   = Refusal("Native session is archived or has an invalid archive flag.")
	notRootSource = Refusal("Native source is not a supported root session; subagent, unknown and malformed sources cannot bind.")
	badStoredCwd  = Refusal("Native session has an invalid working directory.")
	noStoredCwd   = Refusal("Cannot resolve the native session's working directory.")
	otherCwd      = Refusal("Working directory does not match the native session. Run from its exact directory.")
)

// ResolveNativeSession verifies CODEX_THREAD_ID and cwd against the newest state_<N>.sqlite
// (resolveNativeSession): a root session (cli, vscode, exec or mcp), not archived, whose stored
// working directory is cwd after symlinks. It is corroboration for a CLI only, never a hook's
// identity resolver, and it never creates or migrates a native database.
func ResolveNativeSession(cwd string, env LookupEnv) (NativeSession, error) {
	id, set := env("CODEX_THREAD_ID")
	if !set {
		return NativeSession{}, noThreadID
	}
	if !isUUID(id) {
		return NativeSession{}, badThreadID
	}
	// Node holds every string it reads, from argv, the environment, the account database, the file
	// system and SQLite, decoded from UTF-8 with U+FFFD for each invalid sequence, so the oracle never
	// names a path by its invalid bytes. The decode sits where a path enters (cwd, home, the stored
	// cwd) and where the OS answers with one (canonical, getcwd); ids and sources cannot match
	// whichever way they are decoded.
	cwd = decodeUTF8([]byte(cwd))
	canonicalCwd, err := canonical(cwd)
	if info, statErr := os.Lstat(canonicalCwd); err != nil || statErr != nil || !info.IsDir() {
		return NativeSession{}, noWorkdir
	}
	home, err := CodexSQLiteHome(env)
	if err != nil {
		return NativeSession{}, noStateDB
	}
	home = decodeUTF8([]byte(home))
	dbPath, err := newestStateDB(home)
	if err != nil {
		return NativeSession{}, err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return NativeSession{}, unreadable
	}
	defer db.Close()
	var rowID, rowCwd, archived, source any
	switch err := db.QueryRow(ThreadQuery, id).Scan(&rowID, &rowCwd, &archived, &source); {
	case errors.Is(err, sql.ErrNoRows):
		return NativeSession{}, noRow
	case err != nil || !safe(rowID, rowCwd, archived, source): // a malformed file, a missing table or column: never an older database
		return NativeSession{}, unreadable
	}
	if got, _ := rowID.(string); got != id {
		return NativeSession{}, noRow
	}
	if !isZero(archived) {
		return NativeSession{}, archivedRow
	}
	if kind, _ := source.(string); kind != "cli" && kind != "vscode" && kind != "exec" && kind != "mcp" {
		return NativeSession{}, notRootSource
	}
	// node:sqlite hands TEXT to JavaScript decoded as UTF-8 with U+FFFD for each invalid sequence, so
	// the path the oracle resolves is not always the bytes stored (a known defect, kept).
	stored, _ := rowCwd.(string)
	stored = decodeUTF8([]byte(stored))
	if !filepath.IsAbs(stored) {
		return NativeSession{}, badStoredCwd
	}
	storedCanonical, err := canonical(stored)
	if err != nil {
		return NativeSession{}, noStoredCwd
	}
	if storedCanonical != canonicalCwd {
		return NativeSession{}, otherCwd
	}
	return NativeSession{SessionID: id, Cwd: canonicalCwd, DBPath: dbPath}, nil
}

// newestStateDB is the highest-numbered state_<N>.sqlite in home (ties in name order), which must
// be a regular file: no fallback to an older database. The directory is listed as the OS resolves
// home, but the path that is opened is cleaned lexically, as path.resolve does, so a home ending in
// <symlink>/.. lists one directory and opens a file of another (a known defect, kept).
func newestStateDB(home string) (string, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", noStateDB
	}
	var best string
	var bestVersion *big.Int
	for _, entry := range entries {
		version, ok := stateVersion(entry.Name())
		if !ok {
			continue
		}
		if best != "" {
			if order := version.Cmp(bestVersion); order < 0 || order == 0 && entry.Name() > best {
				continue
			}
		}
		best, bestVersion = entry.Name(), version
	}
	if best == "" {
		return "", noStateFile
	}
	path, err := workdirPath(filepath.Join(home, best))
	if err != nil {
		return "", noStateDB
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", noStateDB
	}
	if !info.Mode().IsRegular() {
		return "", notRegular
	}
	return path, nil
}

// stateVersion is N of a state_<N>.sqlite file name; N has any number of digits.
func stateVersion(name string) (*big.Int, bool) {
	digits, found := strings.CutPrefix(name, "state_")
	if digits, ok := strings.CutSuffix(digits, ".sqlite"); found && ok && digits != "" && strings.Trim(digits, "0123456789") == "" {
		return new(big.Int).SetString(digits, 10)
	}
	return nil, false
}

// maxLinks is how many symbolic links one path may pass through before realpath(3) fails with
// ELOOP: glibc's limit, 40. filepath.EvalSymlinks allows 255.
const maxLinks = 40

// canonical is realpathSync.native, which libuv answers with realpath(3): an absolute path with
// every symlink resolved, failing as the C library does once a path passes more than maxLinks of
// them. The empty path is refused, where a relative one resolves against the working directory.
// The walk keeps the bytes of every name; the answer is decoded as the JavaScript string Node
// hands back, so two names that differ only in invalid bytes are the same path to the oracle.
func canonical(path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	resolved := "/"
	if !filepath.IsAbs(path) {
		wd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		resolved = wd
	}
	links := 0
	for pending := path; pending != ""; {
		name, rest, more := strings.Cut(pending, "/") // more: a separator follows, so a file cannot be this component
		pending = rest
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if more && !info.IsDir() {
				return "", syscall.ENOTDIR
			}
			resolved = next
			continue
		}
		if links++; links > maxLinks {
			return "", syscall.ELOOP
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		if more {
			target += "/" + pending
		}
		pending = target
	}
	return decodeUTF8([]byte(resolved)), nil
}

// workdirPath is path.resolve of one path: cleaned lexically and, when relative, joined to the
// working directory as getcwd reports it (process.cwd(), decoded as a JavaScript string), where
// os.Getwd would answer with $PWD.
func workdirPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		wd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		path = decodeUTF8([]byte(wd)) + string(filepath.Separator) + path
	}
	return filepath.Clean(path), nil
}

// safe is false when a scanned INTEGER is beyond what a JavaScript number holds exactly: node:sqlite
// throws on it, so the oracle reads the whole row as unreadable. A REAL is a number and passes.
func safe(values ...any) bool {
	for _, v := range values {
		if n, ok := v.(int64); ok && (n > 1<<53-1 || n < -(1<<53-1)) {
			return false
		}
	}
	return true
}

// isZero is JavaScript's `archived === 0`: the integer 0, or the real 0.0 a column without affinity reads as.
func isZero(v any) bool {
	switch n := v.(type) {
	case int64:
		return n == 0
	case float64:
		return n == 0
	}
	return false
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
			return false
		}
	}
	return true
}
