package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// coordinatedDispatch is a controller operation. Every network mutation follows
// a durable intent. A reader can reconcile an intent, but cannot send it again.
func (g *ghVerify) coordinatedDispatch(ctx context.Context, wt string, req VerifyRequest, head, base, tree, ciDigest string,
	deps, pins map[string]string, workflow crwconfig.GitHubWorkflowInfo, workflowDigest string) ([]byte, string, *dispatchError) {
	fail := func(err error) ([]byte, string, *dispatchError) { return nil, "", &dispatchError{err: err} }
	identity := store.VerificationIdentity{Repository: strings.ToLower(g.repo), Head: head, WorkflowID: workflow.ID, WorkflowDigest: workflowDigest,
		Rules: dagsched.GitHubVerifierRules + "/controller-1", Inputs: map[string]string{}, Tools: pins}
	key, _, err := identity.Key()
	if err != nil {
		return fail(err)
	}
	branch, err := g.branchFor(ctx, wt, req, head)
	if err != nil {
		return fail(err)
	}
	branch += "-" + key
	if err = g.checkRemote(ctx, wt); err != nil {
		return fail(err)
	}
	flight, send, err := g.v.Coordinator.ClaimVerification(ctx, identity, branch, g.v.CILimit)
	if err != nil {
		return fail(err)
	}
	g.flight = &flight
	branch = flight.Ref
	// A foreign or moved ref is always a conflict. Never repair it by pushing.
	exists, err := g.immutableRef(ctx, wt, branch, head)
	if err != nil {
		return fail(err)
	}
	if send {
		if !exists {
			// Empty expected value is create-only CAS. It refuses an intervening
			// writer, even when an ordinary push could fast-forward that writer.
			// No existing ref is authorized to be replaced or force-updated.
			if _, err = g.git(ctx, wt, "push", "--no-follow-tags", "--force-with-lease=refs/heads/"+branch+":", "origin", head+":refs/heads/"+branch); err != nil {
				return fail(err)
			}
		}
		if exists, err = g.immutableRef(ctx, wt, branch, head); err != nil || !exists {
			return fail(errors.Join(err, errors.New("immutable CI ref was not published")))
		}
		if raw, e := g.reuseFlight(ctx, wt, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest); e != nil {
			return fail(e)
		} else if raw != nil {
			return g.flightRecord(ctx, wt, raw, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest)
		}
		// No run can predate this flight on its unique immutable ref. If one does,
		// reconcile it instead of issuing a second dispatch.
		entries, e := g.listRuns(ctx, wt, fmt.Sprint(workflow.ID), branch)
		if e != nil {
			return fail(e)
		}
		found := false
		for _, entry := range entries {
			if entry.HeadBranch == branch {
				found = true
			}
		}
		if !found {
			if _, err = g.gh(ctx, wt, "workflow", "run", fmt.Sprint(workflow.ID), "--repo", g.repo, "--ref", branch); err != nil {
				return fail(fmt.Errorf("dispatch_unknown: workflow %d may have started: %w", workflow.ID, err))
			}
		}
	} else if !exists {
		return fail(errors.New("dispatch_unknown: immutable CI ref is absent"))
	}
	if flight.State == "completed" {
		return g.flightRecord(ctx, wt, flight.Record, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest)
	}
	for flight.RunID == 0 {
		entries, e := g.listRuns(ctx, wt, fmt.Sprint(workflow.ID), branch)
		if e != nil {
			return fail(e)
		}
		var runID int64
		for _, entry := range entries {
			if entry.HeadBranch != branch {
				continue
			}
			if entry.HeadSHA != head || entry.Event != "workflow_dispatch" || entry.WorkflowDatabaseID != workflow.ID || entry.DatabaseID <= 0 {
				return fail(errors.New("verification run identity conflict"))
			}
			if runID != 0 && runID != entry.DatabaseID {
				return fail(errors.New("ambiguous verification dispatch: multiple runs"))
			}
			runID = entry.DatabaseID
		}
		if runID == 0 {
			if err := g.sleep(ctx); err != nil {
				return fail(fmt.Errorf("dispatch_unknown: waiting for the existing dispatch; it will not be repeated: %w", err))
			}
			// Listings can lag dispatch and another attachment can finish a
			// same-tree reuse while this caller waits. Read the durable state
			// again; neither observation grants another dispatch.
			flight, err = g.v.Coordinator.VerificationFlight(ctx, key)
			if err != nil {
				return fail(err)
			}
			g.flight = &flight
			if flight.State == "completed" {
				return g.flightRecord(ctx, wt, flight.Record, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest)
			}
			continue
		}
		if err = g.v.Coordinator.ObserveVerificationRun(ctx, key, runID); err != nil {
			return fail(err)
		}
		flight, err = g.v.Coordinator.VerificationFlight(ctx, key)
		if err != nil {
			return fail(err)
		}
		g.flight = &flight
	}
	attempts, passed, failures, err := g.awaitRun(ctx, wt, flight.RunID, head, branch, workflow)
	if err != nil {
		return fail(err)
	}
	record := g.buildRecord(base, head, tree, ciDigest, deps, pins, workflow, workflowDigest, branch, flight.RunID, attempts)
	record.Result = "fail"
	if passed {
		record.Result = "pass"
		record.GitHub.Conclusion = "success"
		record.GitHub.FlakeRerunPassed = len(attempts.runs) == 2
	}
	raw, err := dagsched.SealVerificationRecord(record)
	if err != nil {
		return fail(err)
	}
	if passed {
		if _, err = dagsched.JudgeVerificationRecord(raw, dagsched.VerificationKeys{Base: base, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}); err != nil {
			record.Result = "fail"
			record.GitHub.FlakeRerunPassed = false
			raw, _ = dagsched.SealVerificationRecord(record)
			return nil, "", &dispatchError{outcome: ghOutcomeOf(raw, record, failures), err: err}
		}
	}
	if exists, e := g.immutableRef(ctx, wt, branch, head); e != nil || !exists {
		return fail(errors.Join(e, errors.New("immutable CI ref changed during verification")))
	}
	raw, err = g.v.Coordinator.CompleteVerification(ctx, key, flight.RunID, raw)
	if err != nil {
		return fail(err)
	}
	return g.flightRecord(ctx, wt, raw, head, base, tree, ciDigest, deps, pins, workflow, workflowDigest)
}

func (g *ghVerify) immutableRef(ctx context.Context, wt, branch, head string) (bool, error) {
	out, err := g.git(ctx, wt, "ls-remote", "--refs", "origin", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	if out == "" {
		return false, nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[0] != head || fields[1] != "refs/heads/"+branch {
		return false, errors.New("immutable CI ref changed: refusing push and verification reuse")
	}
	return true, nil
}

// The shared run never supplies range authority. Re-seal only after the caller's
// fresh scans, and judge the original receipt with the existing tree/rules gate.
func (g *ghVerify) flightRecord(ctx context.Context, wt string, raw []byte, head, base, tree, ciDigest string, deps, pins map[string]string, workflow crwconfig.GitHubWorkflowInfo, digest string) ([]byte, string, *dispatchError) {
	fail := func(err error) ([]byte, string, *dispatchError) { return nil, "", &dispatchError{err: err} }
	rec, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil {
		return fail(err)
	}
	gh := rec.GitHub
	if exists, e := g.immutableRef(ctx, wt, g.flight.Ref, head); e != nil || !exists {
		return fail(errors.Join(e, errors.New("verification flight ref changed")))
	}
	if gh == nil || rec.HeadCommit != head || rec.TreeHash != tree || rec.CiDigest != ciDigest {
		return fail(errors.New("shared verification identity mismatch"))
	}
	if rec.Repository != g.repo || gh.WorkflowID != workflow.ID || gh.WorkflowPath != workflow.Path || gh.WorkflowDigest != digest || gh.RunID != g.flight.RunID {
		return fail(errors.New("shared verification provenance mismatch"))
	}
	if gh.ReusedFrom == nil && (gh.RunHead != head || gh.Branch != g.flight.Ref) {
		return fail(errors.New("original run differs from verification flight"))
	}
	if exists, e := g.immutableRef(ctx, wt, gh.Branch, gh.RunHead); e != nil || !exists {
		return fail(errors.Join(e, errors.New("original run ref changed")))
	}
	if rec.Result == "pass" {
		if _, err = dagsched.JudgeVerificationRecord(raw, dagsched.VerificationKeys{Base: rec.BaseCommit, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}); err != nil {
			return fail(err)
		}
	}
	if rec.BaseCommit != base {
		rec.BaseCommit = base
		gh.Scan = dagsched.GitHubScan{Base: base, Head: head, Result: "clean"}
		raw, err = dagsched.SealVerificationRecord(rec)
		if err != nil {
			return fail(err)
		}
	}
	return raw, "shared controller verification", nil
}

// retryFirst reads the first attempt saved before the unique rerun request.
func (g *ghVerify) retryFirst(ctx context.Context) (*ghRunView, error) {
	if g.flight == nil {
		return nil, nil
	}
	flight, err := g.v.Coordinator.VerificationFlight(ctx, g.flight.Key)
	if err != nil {
		return nil, err
	}
	g.flight = &flight
	if len(flight.FirstAttempt) == 0 {
		return nil, nil
	}
	var first ghRunView
	if err = json.Unmarshal(flight.FirstAttempt, &first); err != nil {
		return nil, err
	}
	// ghRunView's API-only Jobs field is restored separately.
	var saved struct {
		Jobs []ghJob `json:"saved_jobs"`
	}
	if err = json.Unmarshal(flight.FirstAttempt, &saved); err != nil {
		return nil, err
	}
	first.Jobs = saved.Jobs
	return &first, nil
}

// VerifyCandidate is the controller entry point for a collected external result.
// Registered repository policy survives collection; a worker cannot choose a
// command. The authority is refreshed after verification before returning PASS.
func (v GitHubActionsVerifier) VerifyCandidate(ctx context.Context, c executor.CollectedExecution, current func(context.Context) (executor.CollectedExecution, error)) (VerifyOutcome, error) {
	if v.Coordinator == nil || current == nil {
		return VerifyOutcome{}, errors.New("external verification requires controller coordination and current authority")
	}
	check := func() error {
		now, err := current(ctx)
		if err != nil {
			return err
		}
		if now.Admission.TaskDigest != c.Admission.TaskDigest || now.ExecutionID != c.ExecutionID || now.Receipt != c.Receipt || now.Candidate.Checkout != c.Candidate.Checkout || now.Candidate.Head != c.Candidate.Head || now.Candidate.Tree != c.Candidate.Tree {
			return errors.New("external verification authority changed")
		}
		return now.Inspect(ctx)
	}
	if err := check(); err != nil {
		return VerifyOutcome{}, err
	}
	if v.Tools.Repository != c.Admission.Task.Repository.ID {
		return VerifyOutcome{}, errors.New("verifier repository differs from controller enrollment")
	}
	commands, err := crwconfig.LoadRepositoryCommands(ctx, os.Getenv, c.Candidate.Checkout)
	if err != nil {
		return VerifyOutcome{}, err
	}
	out, err := (RepositoryVerifier{Commands: commands, Fallback: v}).Verify(ctx, VerifyRequest{Node: c.Admission.Task.Identity.Node, Worktree: c.Candidate.Checkout, Head: c.Candidate.Head, Base: c.Candidate.Base})
	if err != nil {
		return out, err
	}
	if err = check(); err != nil {
		return VerifyOutcome{}, err
	}
	return out, nil
}

func (g *ghVerify) reuseFlight(ctx context.Context, wt, head, base, tree, ciDigest string, deps, pins map[string]string, workflow crwconfig.GitHubWorkflowInfo, workflowDigest string) ([]byte, error) {
	records, err := g.v.Coordinator.CompletedVerificationRecords(ctx, strings.ToLower(g.repo))
	if err != nil {
		return nil, err
	}
	key := g.cacheKey(workflow, workflowDigest, tree, ciDigest, deps, pins)
	for _, raw := range records {
		rec, err := g.reusableRecord(ctx, wt, tree, key, head, dagsched.VerificationKeys{Base: base, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}, raw)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			continue
		}
		exists, err := g.immutableRef(ctx, wt, rec.GitHub.Branch, rec.GitHub.RunHead)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		rec.GitHub.ReusedFrom = &dagsched.GitHubReusedFrom{Digest: rec.Digest, Head: rec.HeadCommit, VerifierKind: dagsched.GitHubVerifierKind}
		rec.HeadCommit, rec.BaseCommit = head, base
		rec.GitHub.Scan = dagsched.GitHubScan{Head: head, Base: base, Result: "clean"}
		raw, err = dagsched.SealVerificationRecord(*rec)
		if err != nil {
			return nil, err
		}
		if _, err = dagsched.JudgeVerificationRecord(raw, dagsched.VerificationKeys{Base: base, Tree: tree, CiDigest: ciDigest, Dependencies: deps, Pins: pins}); err != nil {
			return nil, err
		}
		if err = g.v.Coordinator.ObserveVerificationRun(ctx, g.flight.Key, rec.GitHub.RunID); err != nil {
			return nil, err
		}
		raw, err = g.v.Coordinator.CompleteVerification(ctx, g.flight.Key, rec.GitHub.RunID, raw)
		if err != nil {
			return nil, err
		}
		flight, err := g.v.Coordinator.VerificationFlight(ctx, g.flight.Key)
		if err != nil {
			return nil, err
		}
		g.flight = &flight
		return raw, nil
	}
	return nil, nil
}
