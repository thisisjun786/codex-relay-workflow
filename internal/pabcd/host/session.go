package host

import (
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
//
// Note is the diagnostic of the root policy (Root.Note) when the databases were read under a root other than the
// one an earlier reading took (an empty HOME); it is set on a refusal too, because a refusal is when the person
// needs it. A caller shows it on stderr.
type NativeSession struct{ SessionID, Cwd, DBPath, Note string }

const (
	noThreadID    = Refusal("CODEX_THREAD_ID is absent. Run this command inside the native Codex session.")
	badThreadID   = Refusal("CODEX_THREAD_ID must be an unmodified native UUID.")
	noWorkdir     = Refusal("Cannot resolve the working directory. Run from the native session's directory.")
	noStateDB     = Refusal("Cannot locate the native state database. Check CODEX_SQLITE_HOME or CODEX_HOME.")
	relativeRoot  = Refusal("Native state database directory is not an absolute path. Set CODEX_SQLITE_HOME or CODEX_HOME to an absolute directory, or HOME to an absolute home; a relative or empty home is not read from the working directory.")
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
// identity resolver, and it never creates or migrates a native database. The databases are read
// under CodexSQLiteRoot, the directory the goals database is read under too.
func ResolveNativeSession(cwd string, env LookupEnv) (NativeSession, error) {
	return resolveNativeSession(cwd, env, accountHome)
}

func resolveNativeSession(cwd string, env LookupEnv, account func() (string, error)) (NativeSession, error) {
	id, set := env("CODEX_THREAD_ID")
	if !set {
		return NativeSession{}, noThreadID
	}
	if !isUUID(id) {
		return NativeSession{}, badThreadID
	}
	// Node holds every string it reads, from argv, the environment, the account database, the file
	// system and SQLite, decoded from UTF-8 with U+FFFD for each invalid sequence, so the oracle never
	// names a path by its invalid bytes. The decode sits where a working directory enters (cwd, the
	// stored cwd) and where the OS answers with one (canonical, getcwd); ids and sources cannot match
	// whichever way they are decoded. The database root is not decoded (CRW-1136): it is listed and
	// opened as the bytes the environment gave.
	cwd = decodeUTF8([]byte(cwd))
	canonicalCwd, err := canonical(cwd)
	if info, statErr := os.Lstat(canonicalCwd); err != nil || statErr != nil || !info.IsDir() {
		return NativeSession{}, noWorkdir
	}
	root, err := codexSQLiteRoot(env, account)
	if err != nil {
		return NativeSession{}, relativeRoot
	}
	refuse := func(err error) (NativeSession, error) { return NativeSession{Note: root.Note}, err }
	dbPath, err := newestStateDB(root)
	if err != nil {
		return refuse(err)
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return refuse(unreadable)
	}
	defer db.Close()
	rows, err := db.Query(ThreadQuery, id)
	if err != nil {
		return refuse(unreadable)
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil { // a lock, or a malformed file: never an older database
			return refuse(unreadable)
		}
		return refuse(noRow)
	}
	names, err := rows.Columns()
	var field [4]any
	if err != nil || rows.Scan(&field[0], &field[1], &field[2], &field[3]) != nil || !safe(field[:]...) {
		return refuse(unreadable)
	}
	// JavaScript reads the columns as properties of the row, which keep the case the table declares,
	// so a column declared ID, CWD, ARCHIVED or SOURCE leaves the field it names missing.
	for i, want := range [...]string{"id", "cwd", "archived", "source"} {
		if names[i] != want {
			field[i] = nil
		}
	}
	rowID, rowCwd, archived, source := field[0], field[1], field[2], field[3]
	if got, _ := rowID.(string); got != id {
		return refuse(noRow)
	}
	if !isZero(archived) {
		return refuse(archivedRow)
	}
	if kind, _ := source.(string); kind != "cli" && kind != "vscode" && kind != "exec" && kind != "mcp" {
		return refuse(notRootSource)
	}
	// node:sqlite hands TEXT to JavaScript decoded as UTF-8 with U+FFFD for each invalid sequence, so
	// the path the oracle resolves is not always the bytes stored (a known defect, kept).
	stored, _ := rowCwd.(string)
	stored = decodeUTF8([]byte(stored))
	if !filepath.IsAbs(stored) {
		return refuse(badStoredCwd)
	}
	storedCanonical, err := canonical(stored)
	if err != nil {
		return refuse(noStoredCwd)
	}
	if storedCanonical != canonicalCwd {
		return refuse(otherCwd)
	}
	return NativeSession{SessionID: id, Cwd: canonicalCwd, DBPath: dbPath, Note: root.Note}, nil
}

// newestStateDB is the highest-numbered state_<N>.sqlite in root (ties in name order), which must
// be a regular file, not a symlink: no fallback to an older database. The directory listed and the
// path opened are the root as spelled, joined without cleaning (Root.Join), so a root ending in
// <symlink>/.. lists and opens the one directory the kernel resolves it to. The oracle cleaned the
// opened path lexically (path.resolve) and read another directory's file; CRW-1136 fixed that. A
// symlinked directory on the way to the root is followed; only the database file itself must not be
// a link.
func newestStateDB(root Root) (string, error) {
	entries, err := os.ReadDir(root.Path)
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
	path := root.Join(best)
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
