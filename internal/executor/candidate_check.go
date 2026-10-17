package executor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RegisteredRepository is controller configuration. Source must never come from
// the result. Host checkout paths are not controller repository locations.
type RegisteredRepository struct{ Forge, ID, Source string }
type ResultCheckInput struct {
	Admission                         Admission
	Peer                              Peer
	Spool, ResultPath, CollectionRoot string
	Repository                        RegisteredRepository
	ExpectedTools                     []Tool
}
type CheckProblem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}
type RequiredGate struct {
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}
type CollectedCandidate struct {
	Repository    string   `json:"repository"`
	Checkout      string   `json:"checkout"`
	Head          string   `json:"head"`
	Tree          string   `json:"tree"`
	Base          string   `json:"base"`
	PublishedHead string   `json:"published_head"`
	ChangedPaths  []string `json:"changed_paths"`
}

// ResultCheckReport is an observation, never an acceptance receipt. Only the
// existing fenced acceptance path can write an accepted state.
type ResultCheckReport struct {
	EnvelopeValid     bool                `json:"envelope_valid"`
	CandidateValid    bool                `json:"candidate_valid"`
	CandidateAccepted bool                `json:"candidate_accepted"`
	CollectionStatus  string              `json:"collection_status"`
	ResultDigest      string              `json:"result_digest,omitempty"`
	Identity          Identity            `json:"identity"`
	Candidate         *CollectedCandidate `json:"candidate,omitempty"`
	NotApplicable     string              `json:"not_applicable,omitempty"`
	Problems          []CheckProblem      `json:"problems"`
	RequiredGates     []RequiredGate      `json:"required_gates"`
}

func (r *ResultCheckReport) Require(gate, reason string) {
	r.RequiredGates = append(r.RequiredGates, RequiredGate{gate, reason})
}
func (r *ResultCheckReport) problem(code string, err error) {
	r.Problems = append(r.Problems, CheckProblem{code, err.Error()})
}

// CheckExecutorResult reads bounded local delivery bytes and fetches only the
// assigned branch from controller configuration. It executes no task commands,
// result text, hooks, review claims or URLs found in artifacts.
func CheckExecutorResult(ctx context.Context, in ResultCheckInput) ResultCheckReport {
	out := ResultCheckReport{Identity: in.Admission.Task.Identity, CollectionStatus: "not_collected", Problems: []CheckProblem{}, RequiredGates: []RequiredGate{}}
	out.Require("acceptance", "existing dag-accept must fence the current owner and epoch; this check writes no acceptance")
	out.Require("authenticated_delivery", "local spool bytes require authenticated transport provenance before acceptance")
	out.Require("worker_cleanliness", "the result schema has no worker clean-worktree proof; committed output alone cannot prove no unfinished work")
	if err := ValidateAdmission(in.Admission); err != nil {
		out.problem("envelope_invalid", err)
		return out
	}
	if !validArtifactPath(in.ResultPath) {
		out.problem("envelope_invalid", errors.New("unsafe result path"))
		return out
	}
	fd, err := openSecureDir(in.Spool)
	if err != nil {
		out.problem("delivery_invalid", err)
		if errors.Is(err, os.ErrNotExist) {
			out.CollectionStatus = "retryable"
		}
		return out
	}
	defer unix.Close(fd)
	raw, err := readLeafFile(fd, in.ResultPath, DefaultLimits.EnvelopeBytes)
	if err != nil {
		out.problem("delivery_unavailable", err)
		if errors.Is(err, os.ErrNotExist) {
			out.CollectionStatus = "retryable"
		}
		return out
	}
	r, err := DecodeResult(in.Admission, raw, in.Peer, DefaultLimits)
	if err != nil {
		out.problem("envelope_invalid", err)
		return out
	}
	out.EnvelopeValid = true
	out.ResultDigest = Digest(raw)
	if err = validateArtifactsAt(fd, r, DefaultLimits); err != nil {
		out.problem("artifact_invalid", err)
		if errors.Is(err, os.ErrNotExist) {
			out.CollectionStatus = "retryable"
		}
		return out
	}
	if r.Outcome != "completed" {
		out.problem("work_incomplete", fmt.Errorf("terminal outcome is %s", r.Outcome))
		return out
	}
	if in.Repository.ID != in.Admission.Task.Repository.ID || in.Repository.Forge != in.Admission.Task.Repository.Forge || in.Repository.Source == "" {
		out.problem("repository_mismatch", errors.New("collection source is not the admitted registered repository"))
		return out
	}
	if len(in.ExpectedTools) == 0 {
		out.Require("toolchain", "no pinned tool manifest available from the enrolled host")
	}
	for _, expected := range in.ExpectedTools {
		found := false
		for _, observed := range r.Provenance.Tools {
			if observed.Name == expected.Name {
				found = observed.Version == expected.Version && observed.SHA256 == expected.SHA256
				break
			}
		}
		if !found {
			out.problem("toolchain_mismatch", fmt.Errorf("tool %s differs from the approved manifest", expected.Name))
			return out
		}
	}
	if r.Provenance.CPUMethod.Value == "" {
		out.Require("cpu_evidence", r.Provenance.CPUMethod.UnknownReason)
	}
	out.Require("review_or_unit_check", "a genuine current-criteria flow-journal verdict on the collected candidate is required")
	if in.Admission.Task.NotApplicable != "" {
		out.NotApplicable = in.Admission.Task.NotApplicable
	}
	if r.Payload.Candidate == nil {
		out.CandidateValid = true
		out.CollectionStatus = "not_applicable"
		return out
	}
	out.Require("verification-record/1", "a passing configured verifier record for the actual candidate is required")
	out.Require("pre_merge", "a current-criteria pre-merge judgment for the actual candidate is required")
	c, err := CollectCandidate(ctx, in.Admission, *r.Payload.Candidate, in.Repository, in.CollectionRoot)
	if err != nil {
		var e *CollectionError
		if errors.As(err, &e) {
			out.problem(e.Code, err)
			if e.Code == "scope_decision_required" {
				out.Require("scope_decision", e.Detail)
			}
			if e.Retryable {
				out.CollectionStatus = "retryable"
			} else {
				out.CollectionStatus = "invalid"
			}
		} else {
			out.problem("collection_failed", err)
			out.CollectionStatus = "retryable"
		}
		return out
	}
	out.Candidate = c
	out.CandidateValid = true
	out.CollectionStatus = "collected"
	return out
}
