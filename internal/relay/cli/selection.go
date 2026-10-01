package cli

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
)

func guardFallback(s dispatch.Services) (string, error) {
	path, err := selection.GuardFallback(selection.Services{Selection: s.Selection, SocketPath: s.SocketPath, Program: s.Program})
	var refused *selection.Refused
	if errors.As(err, &refused) {
		return "", &dispatch.PayloadExit{Payload: refused.Payload, Code: contract.ExitRefused}
	}
	return path, err
}
