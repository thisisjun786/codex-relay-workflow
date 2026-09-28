package adapter

import "github.com/thisisjun786/codex-relay-workflow/internal/relay/store"

func VerifyFrozenDetailed(reference string, entries []Entry) (string, []string, []string, error) {
	return store.VerifyFrozenDetailed(reference, entries)
}
func VerifyFrozen(reference string, entries []Entry) (string, []string, error) {
	digest, problems, _, err := store.VerifyFrozenDetailed(reference, entries)
	return digest, problems, err
}
