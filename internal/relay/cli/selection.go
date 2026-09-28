package cli

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
)

func selectionRefusal(s Services) (contract.OrderedObject, error) {
	return selection.Refusal(selection.Services{Selection: s.Selection, SocketPath: s.SocketPath, Program: s.Program})
}

func guardFallback(s Services) (string, error) {
	path, err := selection.GuardFallback(selection.Services{Selection: s.Selection, SocketPath: s.SocketPath, Program: s.Program})
	var refused *selection.Refused
	if errors.As(err, &refused) {
		return "", &PayloadExit{Payload: refused.Payload, Code: contract.ExitRefused}
	}
	return path, err
}
