package cli

import "github.com/thisisjun786/codex-relay-workflow/internal/contract"

// CheckPacketSelection is the packet-check registry adapter: only its store-backed check
// consults the shared selection refusal. --record and --applied read no selected store.
func CheckPacketSelection(services Services) error {
	payload, err := selectionRefusal(services)
	if err != nil {
		return err
	}
	if payload != nil {
		return &PayloadExit{Payload: payload, Code: contract.ExitRefused}
	}
	return nil
}
