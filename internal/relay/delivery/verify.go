package delivery

import (
	"slices"
)

// The approval note of a resume response (settings.TaskSettings.approval_divergence). The rest of
// the guarded send's settings check - the mismatches that refuse a send and the roots-narrowing
// notes - is registry.TaskSettings', which the bridge adapter calls (internal/relay/adapter
// settings.go).

// ApprovalDiffersFromRecord is the note's code.
const ApprovalDiffersFromRecord = "approval_policy_differs_from_record"

// ApprovalDivergence is approval_divergence: a carried policy other than the recorded one.
func (t TaskSettings) ApprovalDivergence(response any) any {
	r, ok := response.(Obj)
	if !ok {
		return nil
	}
	observed, _ := get(r, "approvalPolicy")
	recorded, _ := get(t.Data, "approvalPolicy")
	p, isText := observed.(string)
	if observed == recorded || !isText || !slices.Contains(CarriedApprovalPolicies, p) {
		return nil
	}
	return Obj{{Key: "code", Value: ApprovalDiffersFromRecord}, {Key: "field", Value: "approvalPolicy"}, {Key: "recorded", Value: recorded}, {Key: "observed", Value: observed}}
}
