package session

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// GateResult is the verdict of CheckBound; Reason says why not when OK is false.
type GateResult struct {
	OK     bool
	Reason string
}

// CheckBound refuses entry to a goalplan-bound cycle whose source identity cannot be resolved: C -> D needs a test receipt
// (CHECK-BINDING-01) and a receipt cannot be bound to a commit without one, so the verdict is given before the plan, audit
// and implementation are spent. The predicate is "the source identity is unavailable", never "there is no .git": binding a
// git source worktree clears it without this function knowing anything about split working directories. A thrown binding or
// source-root error is also "cannot resolve". Call it only for a bound session; an unbound one closes D on its check output
// and exit code alone. A dirty tree is a resolved identity.
func CheckBound(cwd, sessionID string) GateResult {
	exclude := true
	id, err := Capture(cwd, sessionID, CaptureOptions{ExcludeStateArtifacts: &exclude})
	if err != nil {
		return GateResult{Reason: err.Error()}
	}
	if id.Kind != source.KindUnavailable {
		return GateResult{OK: true}
	}
	return GateResult{Reason: strings.Join([]string{
		"this workspace has no resolvable git source identity, and a goalplan-bound",
		"cycle cannot be closed without one: C -> D requires a testReceiptPath",
		"(CHECK-BINDING-01) and `crw pabcd receipt test` refuses to write a receipt it cannot",
		"bind to a commit.",
		"",
		"Pick one:",
		"  - bind a git source tree:  crw relay session source <absolute-path> --json",
		"  - or run this cycle unbound (no `crw pabcd loop init --session`), which closes D on",
		"    checkOutput + exitCode alone.",
	}, "\n")}
}
