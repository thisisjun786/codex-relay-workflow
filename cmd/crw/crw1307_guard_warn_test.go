package main

import (
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// guardWarning is the warning of a guard's warn answer (CRW-1307): a PreToolUse answer with additionalContext only, which lets the
// call through, with either prefix (the record written or not: these entry tests mostly run outside a plugin; the harness tests pin
// which prefix goes with which). ok is false for any other answer.
func guardWarning(out string) (string, bool) {
	var a struct {
		Output map[string]any `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(out), &a) != nil || a.Output["hookEventName"] != "PreToolUse" || a.Output["permissionDecision"] != nil || len(a.Output) != 2 {
		return "", false
	}
	ctx, _ := a.Output["additionalContext"].(string)
	for _, prefix := range []string{harness.WarningPrefix, harness.WarningPrefixUnrecorded} {
		if strings.HasPrefix(ctx, prefix) {
			return strings.TrimPrefix(ctx, prefix), true
		}
	}
	return "", false
}
