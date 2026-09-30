package store

import (
	"os"
	"path/filepath"
	"sort"
)

// StateEnv is STATE_ENV: the environment override for the state directory.
const StateEnv = "CODEX_SESSION_RELAY_STATE"

// CanonicalSocket is canonical_socket: expanded, absolute and fully resolved. It resolves strictly
// (ResolvePath), refusing a loop or a component it cannot search where canonical_socket's
// Path.resolve() keeps them (docs/port/known-defects.md, Python defects not carried over).
func CanonicalSocket(path string) (string, error) { return canonicalSocket(path) }

// ResolvePath follows every symbolic link and keeps a missing tail, but fails on a component it
// cannot examine or a loop, where Path.resolve() keeps them (Realpath).
func ResolvePath(path string) (string, error) { return resolvePath(path) }

// ResolveLoosely is Path.resolve() as ownership.mirror and the Stop client call it (strict=False):
// a component that cannot be examined is kept as spelled.
func ResolveLoosely(path string) string { return resolveLoosely(path) }

// ExpandUser is Path.expanduser(): an unknown ~user is an error, as Python's RuntimeError.
func ExpandUser(path string) (string, error) { return expandUser(path) }

// StoreSocket is store_socket: the canonical socket a store recorded for itself, or "".
func StoreSocket(dbPath string) string { return storeSocket(dbPath) }

// siblingStoreDirs is `sorted(p for p in Path(root).iterdir() if p.is_dir())` minus skip, keeping
// only directories whose relay.sqlite3 exists. An unreadable root is no candidates, as in Python.
// Each is spelled str(Path(root) / name), so a root under "//" keeps its two slashes.
func siblingStoreDirs(root, skip string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		path := PathlibChild(root, entry.Name())
		if info, err := os.Stat(path); err != nil || !info.IsDir() || entry.Name() == skip {
			continue
		}
		if exists(filepath.Join(path, "relay.sqlite3")) {
			found = append(found, path)
		}
	}
	sort.Strings(found)
	return found
}

// StoresWithoutProvenance is stores_without_provenance: store directories that never recorded
// which socket they serve.
func StoresWithoutProvenance(root, skip string) []string {
	found := []string{}
	for _, directory := range siblingStoreDirs(root, skip) {
		if storeSocket(filepath.Join(directory, "relay.sqlite3")) == "" {
			found = append(found, directory)
		}
	}
	return found
}

// StoresClaimingSocket is stores_claiming_socket: every store recording this socket.
func StoresClaimingSocket(root, socket, skip string) ([]string, error) {
	found := []string{}
	if socket == "" {
		return found, nil
	}
	wanted, err := canonicalSocket(socket)
	if err != nil {
		return nil, err
	}
	for _, directory := range siblingStoreDirs(root, skip) {
		if storeSocket(filepath.Join(directory, "relay.sqlite3")) == wanted {
			found = append(found, directory)
		}
	}
	return found, nil
}
