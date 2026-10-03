// Package evidence is a stub: every function answers its zero value, so the tests of this commit fail by assertion.
package evidence

import "time"

const (
	MaxAttempts    = 0
	Subdir         = ""
	AttemptsSubdir = ""
)

type Payload struct{ AgentType, AgentID, TurnID, LastAssistantMessage string }

type MarkerWriter func(cwd, sessionID, agentID string) error

type lockFunc func(cwd, sessionID string, fn func() error) error

func GatedAgentTypes() []string                                  { return nil }
func IsGatedAgentType(string) bool                               { return false }
func ExtractReceiptPath(string) (string, bool)                   { return "", false }
func HasValidReceipt(cwd, receiptPath string) bool               { return false }
func TranscriptHasContextPressure(string) bool                   { return false }
func ReadAttempts(cwd, sessionID, agentID, turnID string) int    { return 0 }
func ClearAttempts(cwd, sessionID, agentID, turnID string)       {}
func HasTombstone(cwd, sessionID string, p Payload) bool         { return false }
func attemptsPath(cwd, sessionID, agentID, turnID string) string { return "" }
func tupleDigest(agentID, turnID string) string                  { return "" }

func WriteAttempts(cwd, sessionID, agentID string, attempts int, turnID string) bool { return false }

func RecordTombstone(cwd, sessionID string, p Payload, attempts int, marker MarkerWriter) bool {
	return false
}

func recordTombstone(cwd, sessionID string, p Payload, attempts int, now time.Time, lock lockFunc, marker MarkerWriter) bool {
	return false
}
