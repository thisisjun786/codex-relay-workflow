package hook

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ownerFallback keeps the serving owner's store while applying the same lazy
// provenance and override refusals as the relay CLI. A pin bypasses this function.
func ownerFallback(state, socket, program string) func() (string, error) {
	return func() (string, error) {
		selected, err := store.ResolveStateDir("", socket)
		if err != nil {
			return "", err
		}
		if filepath.Clean(selected.Path) != filepath.Clean(state) {
			selected, err = store.ResolveStateDir(state, socket)
			if err != nil {
				return "", err
			}
		}
		if program == "" {
			program = "codex-session-relay"
		}
		return selection.GuardFallback(selection.Services{Selection: selected, SocketPath: socket, Program: program})
	}
}

func evaluateOwner(ctx context.Context, stop Object, options GuardOptions) (Object, error) {
	verdict, err := Evaluate(ctx, stop, options)
	var refused *selection.Refused
	if errors.As(err, &refused) {
		return refused.Payload, nil
	}
	return verdict, err
}
