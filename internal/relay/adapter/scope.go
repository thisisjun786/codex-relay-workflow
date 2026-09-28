package adapter

import "github.com/thisisjun786/codex-relay-workflow/internal/relay/store"

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
