package interview

// CXC v0.2.40 rescan-coordinator.ts (3c1459ac): a signal for the main session,
// never a dispatcher, writer or Stop handler.

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// PendingInterviewWork is the pending QA pairs and tracker-only high contradictions.
type PendingInterviewWork struct {
	PendingQuestionIDs     []string `json:"pendingQuestionIds"`
	HighContradictionCount int      `json:"highContradictionCount"`
	Pending                bool     `json:"pending"`
}

// RescanDeps supplies the QA reader and optional goal override. The caller adapts
// ledger.ReadQaEvents by returning QaEvent.Raw: ledger imports interview, so this
// package cannot import it. A nil reader is unavailable QA evidence. GoalDBPath
// defaults to the real host path; GoalStatus, when supplied, takes precedence.
type RescanDeps struct {
	ReadQaEvents func(cwd, sessionID string) []json.RawMessage
	GoalStatus   func() host.GoalStatus
	GoalDBPath   string
}

// HasPendingInterviewWork matches (turnId, questionId), preserving first-asked
// order. It accepts *Tracker or a decoded JSON object without reconstructing its
// malformed contradictions into high sentinels. Active/unreadable goals suppress
// every signal before QA is read. It never reads scan evidence or writes state.
func HasPendingInterviewWork(cwd, sessionID string, tracker any, deps RescanDeps) PendingInterviewWork {
	out := PendingInterviewWork{PendingQuestionIDs: []string{}}
	status := rescanGoalStatus(sessionID, deps)
	if host.SuppressesInterview(status) {
		return out
	}
	answered, asked := map[string]bool{}, map[string]bool{}
	order := []string{}
	for _, raw := range rescanReadQA(cwd, sessionID, deps.ReadQaEvents) {
		var row map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber() // Unrelated overflowing numbers must not discard a QA pair.
		if decoder.Decode(&row) != nil {
			continue
		}
		if _, err := decoder.Token(); err != io.EOF {
			continue
		}
		turn, turnOK := row["turnId"].(string)
		question, questionOK := row["questionId"].(string)
		if !turnOK || !questionOK {
			continue
		}
		key := turn + "\x00" + question
		switch row["event"] {
		case "answer_recorded":
			answered[key] = true
		case "question_asked":
			if !asked[key] {
				order = append(order, key)
			}
			asked[key] = true
		}
	}
	for _, key := range order {
		if !answered[key] {
			out.PendingQuestionIDs = append(out.PendingQuestionIDs, key)
		}
	}
	switch t := tracker.(type) {
	case *Tracker:
		if t != nil {
			for _, c := range t.Contradictions {
				if c.Severity == SeverityHigh {
					out.HighContradictionCount++
				}
			}
		}
	case map[string]any:
		rows, _ := t["contradictions"].([]any)
		for _, c := range rows {
			if row, ok := c.(map[string]any); ok && row["severity"] == "high" {
				out.HighContradictionCount++
			}
		}
	}
	out.Pending = len(out.PendingQuestionIDs) > 0 || out.HighContradictionCount > 0
	return out
}

func rescanGoalStatus(sessionID string, deps RescanDeps) host.GoalStatus {
	if deps.GoalStatus != nil {
		return deps.GoalStatus()
	}
	path := deps.GoalDBPath
	if path == "" {
		var err error
		path, err = host.GoalsDBPath(os.LookupEnv)
		if err != nil {
			return host.GoalUnreadable
		}
	}
	return host.GoalActiveStatus(sessionID, path)
}

// The oracle catches a throwing QA reader and retains only the tracker signal.
func rescanReadQA(cwd, id string, read func(string, string) []json.RawMessage) (rows []json.RawMessage) {
	defer func() {
		if recover() != nil {
			rows = nil
		}
	}()
	if read != nil {
		return read(cwd, id)
	}
	return nil
}

// ComputeNextScanRound derives ONLY from scanRounds, floored, plus one. The
// result is a JS number, so huge counters retain IEEE-754 addition/rounding.
func ComputeNextScanRound(tracker any) float64 {
	var raw any
	switch t := tracker.(type) {
	case *Tracker:
		if t != nil {
			raw = t.ScanRounds
		}
	case map[string]any:
		raw = t["scanRounds"]
	}
	return rescanRound(raw) + 1
}

func rescanRound(v any) float64 {
	n, ok := number(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n == 0 {
		return 0
	}
	return math.Floor(n)
}
