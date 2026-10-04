package interview

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

type PendingInterviewWork struct {
	PendingQuestionIDs     []string `json:"pendingQuestionIds"`
	HighContradictionCount int      `json:"highContradictionCount"`
	Pending                bool     `json:"pending"`
}
type RescanDeps struct {
	ReadQaEvents func(string, string) []json.RawMessage
	GoalStatus   func() host.GoalStatus
	GoalDBPath   string
}

func HasPendingInterviewWork(cwd, sessionID string, tracker any, deps RescanDeps) PendingInterviewWork {
	return PendingInterviewWork{HighContradictionCount: -1}
}
func ComputeNextScanRound(tracker any) float64 { return 0 }
