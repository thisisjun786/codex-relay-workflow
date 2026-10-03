package goalplan

import (
	"encoding/json"
	"os"
)

type GoalplanReadDiagnostic struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Field  string `json:"field,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type GoalplanReadResult struct {
	Plan       *Goalplan               `json:"plan"`
	Diagnostic *GoalplanReadDiagnostic `json:"diagnostic"`
}

func ReadGoalplanDetailed(cwd, slug string) GoalplanReadResult {
	path, err := goalplanPath(cwd, slug)
	if err != nil {
		return GoalplanReadResult{Diagnostic: &GoalplanReadDiagnostic{Kind: "unreadable", Path: slug, Detail: err.Error()}}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return GoalplanReadResult{Diagnostic: &GoalplanReadDiagnostic{Kind: "absent", Path: path}}
	}
	var parsed any
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return GoalplanReadResult{Diagnostic: &GoalplanReadDiagnostic{Kind: "invalid-json", Path: path, Detail: err.Error()}}
	}
	return GoalplanReadResult{Plan: reviveGoalplan(parsed, &slug)}
}
func ReadGoalplan(cwd, slug string) *Goalplan { return ReadGoalplanDetailed(cwd, slug).Plan }
