package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A night batch is a decision of the manager and a run for it (docs/flow/script.md, the night-batch workflow). The manager selects
// the issues (`crw manage batch plan`, CRW-1040a) and records the decision: the selected issues, the token budget and the deadline
// of the run. `crw flow start night-batch --decision D` starts the run from that record, which has to be answered `run`.
//
// The decision is a record of the relay's user-decision table (kind policy, authority delegated-management, options run and skip),
// and the batch is the JSON document its context holds.

// NightBatchSchema names the document of a night batch decision.
const NightBatchSchema = "crw-night-batch/1"

// The options of a night batch decision.
const (
	NightRun  = "run"
	NightSkip = "skip"
)

// NightBatchWorkflow is the built-in workflow a night batch decision starts.
const NightBatchWorkflow = "night-batch"

// NightBatch is the decision: what the run works on and where it stops.
type NightBatch struct {
	Schema string `json:"schema"`
	// Label is the label the issues were selected by (crw manage batch plan --label).
	Label string `json:"label"`
	// Issues are the selected issues, by key.
	Issues []string `json:"issues"`
	// Plan and Milestone are the plan whose milestone holds the issues as nodes (a node carries its issue key).
	Plan      string `json:"plan"`
	Milestone string `json:"milestone"`
	// TokenBudget is the token ceiling of the run: the run pauses when it is spent. Deadline (RFC 3339) is the time it pauses at.
	TokenBudget int64  `json:"token_budget"`
	Deadline    string `json:"deadline"`
	// Cap is the number of executors at once (0: the project lane's default).
	Cap int `json:"cap,omitempty"`
	// Handoff is the crw-plan handoff the plan's current revision was compiled from (`crw manage batch decide --handoff FILE`): the
	// run pins its bytes and reads the compiled milestone from them (CRW-1277 a).
	Handoff *HandoffRef `json:"handoff"`
}

// Validate checks a decision document.
func (n NightBatch) Validate() error {
	switch {
	case n.Schema != NightBatchSchema:
		return fmt.Errorf("the document's schema is %q, not %s", n.Schema, NightBatchSchema)
	case len(n.Issues) == 0:
		return errors.New("the batch selects no issue")
	case strings.TrimSpace(n.Plan) == "" || strings.TrimSpace(n.Milestone) == "":
		return errors.New("the batch names no plan and milestone to run the issues as nodes of")
	case n.TokenBudget <= 0:
		return errors.New("the batch has no token budget (a night run without a ceiling is not started)")
	case n.Handoff == nil || !filepath.IsAbs(n.Handoff.Path) || !sha256Pattern.MatchString(n.Handoff.SHA256):
		return errors.New("the batch names no handoff {path, sha256}: the run reads the milestone it compiled from the crw-plan handoff (crw manage batch decide --handoff FILE)")
	}
	if _, err := time.Parse(time.RFC3339, n.Deadline); err != nil {
		return fmt.Errorf("the deadline %q is not an RFC 3339 time", n.Deadline)
	}
	seen := map[string]bool{}
	for _, key := range n.Issues {
		if strings.TrimSpace(key) == "" || seen[key] {
			return fmt.Errorf("the issue list holds a blank or repeated key %q", key)
		}
		seen[key] = true
	}
	return nil
}

// Args are the arguments of the night-batch workflow for this decision.
func (n NightBatch) Args(decision string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"decision": decision, "label": n.Label, "issues": n.Issues, "plan": n.Plan, "milestone": n.Milestone, "cap": n.Cap, "deadline": n.Deadline, "token_budget": n.TokenBudget})
	return raw
}

// NightBatchOf reads the night batch an answered decision record holds. The record has to be a policy decision answered with the
// option run, by an answer that names who gave it and how it arrived.
func NightBatchOf(rec decisions.Record) (NightBatch, error) {
	var n NightBatch
	if err := json.Unmarshal([]byte(rec.Context), &n); err != nil {
		return n, fmt.Errorf("the record %s holds no night batch document: %v", rec.DecisionID, err)
	}
	if err := n.Validate(); err != nil {
		return n, fmt.Errorf("the record %s: %v", rec.DecisionID, err)
	}
	switch {
	case rec.State != decisions.StateAnswered && rec.State != decisions.StateApplied:
		return n, fmt.Errorf("the record %s is %s; the manager answers it %q first", rec.DecisionID, rec.State, NightRun)
	case rec.AnswerText != NightRun:
		return n, fmt.Errorf("the record %s is answered %q, not %q", rec.DecisionID, rec.AnswerText, NightRun)
	case rec.AnsweredBy == "" || rec.AnsweredVia == "":
		return n, fmt.Errorf("the record %s names no one who answered it", rec.DecisionID)
	}
	return n, nil
}

// LoadNightBatch reads a decision from the store and the night batch it holds.
func LoadNightBatch(ctx context.Context, st *store.Store, decision string) (NightBatch, error) {
	rec, err := st.Get(ctx, decision)
	if err != nil {
		return NightBatch{}, err
	}
	return NightBatchOf(rec)
}
