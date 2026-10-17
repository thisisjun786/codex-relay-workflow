package executor

import (
	"context"
	"errors"
)

// CollectedExecution binds a controller checkout to authenticated collection.
// ExecutionID can be the current handover successor; Admission stays immutable.
type CollectedExecution struct {
	Admission   Admission          `json:"admission"`
	Candidate   CollectedCandidate `json:"candidate"`
	Receipt     Acknowledgement    `json:"receipt"`
	ExecutionID string             `json:"execution_id"`
}

func (c CollectedExecution) Inspect(ctx context.Context) error {
	if err := ValidateAdmission(c.Admission); err != nil {
		return err
	}
	r := c.Receipt
	if c.ExecutionID == "" || r.State != "received" || r.ExecutionID != c.Admission.Task.Identity.ExecutionID || r.TaskDigest != c.Admission.TaskDigest || !validDigest(r.ResultDigest) {
		return errors.New("candidate has no matching authenticated collection receipt")
	}
	if c.Candidate.Repository != c.Admission.Task.Repository.ID || c.Candidate.Base != c.Admission.Task.Repository.BaseCommit {
		return errors.New("candidate repository/base differs from assignment")
	}
	origin, err := candidateCommand(ctx, c.Candidate.Checkout, "remote", "get-url", "origin")
	if err != nil || origin != "https://github.com/"+c.Admission.Task.Repository.ID+".git" {
		return errors.New("collection repository identity changed")
	}
	copy := c.Candidate
	return InspectCandidate(ctx, c.Admission, Candidate{Head: c.Candidate.Head, Tree: c.Candidate.Tree, Branch: c.Admission.Task.Repository.TaskBranch}, &copy)
}
