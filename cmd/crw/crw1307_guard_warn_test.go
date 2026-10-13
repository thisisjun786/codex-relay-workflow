package main

import (
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// guardWarning is the warning of a guard's warn answer (CRW-1307): a PreToolUse answer with additionalContext only, which lets the
// call through. ok is false for any other answer.
func guardWarning(out string) (string, bool) {
	var a struct {
		Output map[string]any `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(out), &a) != nil || a.Output["hookEventName"] != "PreToolUse" || a.Output["permissionDecision"] != nil || len(a.Output) != 2 {
		return "", false
	}
	ctx, _ := a.Output["additionalContext"].(string)
	if !strings.HasPrefix(ctx, harness.WarningPrefix) {
		return "", false
	}
	return strings.TrimPrefix(ctx, harness.WarningPrefix), true
}
