package delivery

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The decision reply (CRW-394), tests first: this stub refuses every call, so the tests that describe the contract fail by assertion
// against it. The next commit writes the rule.

// Decision kinds, and the outcome and generation reason a reply carries.
const (
	DecisionReply         = "decision_reply"
	DecisionAnswer        = "answer"
	DecisionStop          = "stop"
	DecisionSplitApproval = "split_approval"
	DecisionScopeChange   = "scope_change"
)

// MalformedReceipt is the reason a request that does not say what it must is refused with.
const MalformedReceipt = string(contract.RefusalMalformedReceipt)

// DecisionRequest is what decision-reply is asked.
type DecisionRequest struct{ EventID, Decision, Turn, Note, CriteriaDigest string }

var errDecisionStub = errors.New("decision reply is not implemented")

// RecordDecision is not written yet.
func (a *Ack) RecordDecision(ctx context.Context, req DecisionRequest) (Obj, error) {
	return nil, errDecisionStub
}

// DecisionsOf is not written yet.
func (a *Ack) DecisionsOf(ctx context.Context, rid string) (Obj, error) { return nil, errDecisionStub }

func cmdDecisionReply(c *cliRun) (any, error) { return nil, errDecisionStub }

func cmdDecisionShow(c *cliRun) (any, error) { return nil, errDecisionStub }

