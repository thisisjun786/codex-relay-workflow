package adapter

import "github.com/thisisjun786/codex-relay-workflow/internal/relay/store"

type Entry = store.ManifestEntry

const ManifestSerialization = store.ManifestSerialization

func CanonicalPayload(entries []Entry) (string, error) { return store.CanonicalPayload(entries) }
func RevisionHash(entries []Entry) (string, error)     { return store.ManifestRevision(entries) }
func BuildManifest(paths, roots []string, lease bool) ([]Entry, map[string]PathBinding, error) {
	entries := []Entry{}
	bindings := map[string]PathBinding{}
	for _, path := range paths {
		digest, size, binding, err := store.HashArtifact(path, roots, lease)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, Entry{Path: path, SHA256: digest, Bytes: &size})
		bindings[path] = binding
	}
	return entries, bindings, nil
}
func VerifyAgainstDiskDetailed(entries []Entry, roots []string, lease bool) ([]string, map[string]PathBinding, []string) {
	return store.VerifyAgainstDiskDetailed(entries, roots, lease)
}
func VerifyAgainstDisk(entries []Entry, roots []string, lease bool) ([]string, map[string]PathBinding) {
	problems, bindings, _ := store.VerifyAgainstDiskDetailed(entries, roots, lease)
	return problems, bindings
}
func Freeze(entries []Entry, destination string) (string, error) {
	err := store.FreezeManifest(entries, destination)
	return destination, err
}
