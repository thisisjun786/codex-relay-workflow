package adapter

import "github.com/thisisjun786/codex-relay-workflow/internal/relay/store"

// The adapter tests name the store's manifest, scope and frozen-copy functions by the adapter's
// earlier forwarders; the product calls the store directly (decision 52).

func VerifyFrozenDetailed(reference string, entries []Entry) (string, []string, []string, error) {
	return store.VerifyFrozenDetailed(reference, entries)
}
func VerifyFrozen(reference string, entries []Entry) (string, []string, error) {
	digest, problems, _, err := store.VerifyFrozenDetailed(reference, entries)
	return digest, problems, err
}

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

// The store already owns the receipt-intake traversal and hash implementation.
// Adapter callers use the same code; no second filesystem authorization exists.
const BestEffortDetection = store.BestEffortDetection
const LeaseEnforced = store.LeaseEnforced

type ScopeError = store.RefusedError
type PathBinding = store.ArtifactBinding
type AuthorizedFile = store.AuthorizedFile

func NormalizeDeclaredPath(path string) (string, error) { return store.NormalizeDeclaredPath(path) }
func IsWithin(root, path string) bool                   { return store.IsWithin(root, path) }
func AtLeast(actual, minimum store.PathBinding) bool {
	return actual == minimum || actual == LeaseEnforced && minimum == BestEffortDetection
}
func OpenAuthorized(path string, roots []string, lease bool) (*AuthorizedFile, error) {
	return store.OpenAuthorized(path, roots, lease)
}
func HashAuthorized(handle *AuthorizedFile, between func() error) (string, int64, error) {
	return store.HashAuthorized(handle, between)
}
func HashPath(path string, roots []string, lease bool) (string, int64, PathBinding, error) {
	return store.HashArtifact(path, roots, lease)
}
