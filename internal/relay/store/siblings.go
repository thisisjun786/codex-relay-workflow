package store

import (
	"os"
	"path/filepath"
	"sort"
)

// StateEnv is STATE_ENV: the environment override for the state directory.
const StateEnv = "CODEX_SESSION_RELAY_STATE"

// CanonicalSocket is canonical_socket: expanded, absolute and fully resolved.
func CanonicalSocket(path string) (string, error) { return canonicalSocket(path) }

// ResolvePath is Path.resolve() (non-strict): symlinks followed, a missing tail kept.
func ResolvePath(path string) (string, error) { return resolvePath(path) }

// ExpandUser is Path.expanduser(): an unknown ~user is an error, as Python's RuntimeError.
func ExpandUser(path string) (string, error) { return expandUser(path) }

// StoreSocket is store_socket: the canonical socket a store recorded for itself, or "".
func StoreSocket(dbPath string) string { return storeSocket(dbPath) }

// siblingStoreDirs is `sorted(p for p in Path(root).iterdir() if p.is_dir())` minus skip, keeping
// only directories whose relay.sqlite3 exists. An unreadable root is no candidates, as in Python.
func siblingStoreDirs(root, skip string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
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
