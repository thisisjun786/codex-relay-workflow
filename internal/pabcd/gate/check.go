package gate

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"

// CheckGateResult is the verdict of ValidateCheckReceipt: Reason says why not when OK is false.
type CheckGateResult struct {
	OK     bool
	Reason string
}

// ValidateCheckReceipt is not ported yet.
func ValidateCheckReceipt(st state.State, sessionID, receiptPath, cwd string) CheckGateResult {
	return CheckGateResult{Reason: "not implemented"}
}
