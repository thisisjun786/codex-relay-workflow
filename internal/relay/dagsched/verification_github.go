package dagsched

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The remote verifier's identity in a verification-record/1 (CRW-1594): runner "github-actions" carries the
// GitHub provenance member, with verifierKind and verifierRules naming the verifier and its rule version.
const (
	GitHubRunner        = "github-actions"
	GitHubVerifierKind  = "github-actions"
	GitHubVerifierRules = "github-actions/1"
)

// GitHubVerification is the github member of a remote verification record: the run the record rests on, the
// workflow that ran it, its attempts, the secret scan of the range the record covers and, for a reused
// record, the receipt it reuses.
type GitHubVerification struct {
	RunID            int64              `json:"runId"`
	URL              string             `json:"url"`
	RunHead          string             `json:"runHead"`
	Branch           string             `json:"branch"`
	Event            string             `json:"event"`
	WorkflowID       int64              `json:"workflowId"`
	WorkflowPath     string             `json:"workflowPath"`
	WorkflowDigest   string             `json:"workflowDigest"`
	Conclusion       string             `json:"conclusion"`
	Attempts         int                `json:"attempts"`
	FlakeRerunPassed bool               `json:"flake_rerun_passed"`
	Runs             []GitHubRunAttempt `json:"runs"`
	Scan             GitHubScan         `json:"scan"`
	ReusedFrom       *GitHubReusedFrom  `json:"reusedFrom,omitempty"`
}

// GitHubRunAttempt is one attempt of the run, with the jobs the run view answered for it.
type GitHubRunAttempt struct {
	Attempt    int               `json:"attempt"`
	Status     string            `json:"status"`
	Conclusion string            `json:"conclusion"`
	Jobs       []json.RawMessage `json:"jobs"`
}

// GitHubScan is the secret scan of the range the record covers: base..head, clean.
type GitHubScan struct {
	Base   string `json:"base"`
	Head   string `json:"head"`
	Result string `json:"result"`
}

// GitHubReusedFrom is the receipt a reused record was read from: its digest, the head it verified and its
// verifier kind.
type GitHubReusedFrom struct {
	Digest       string `json:"digest"`
	Head         string `json:"head"`
	VerifierKind string `json:"verifierKind"`
}

var (
	ghJudgeHex          = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ghJudgeSHA256       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	ghJudgeRepository   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	ghJudgeWorkflowFile = regexp.MustCompile(`^\.github/workflows/[^/\x00-\x1f]+$`)
	ghJudgeTerminal     = map[string]bool{"success": true, "failure": true, "cancelled": true, "skipped": true, "timed_out": true, "neutral": true}
)

// judgeGitHubRecord is JudgeVerificationRecord's remote branch: a record whose runner is github-actions. The
// same schema/digest/result/tree/base/ci/dependency/pins checks hold — a remote record is refused by a
// changed key like any other — but its runner environment is the configured workflow, not this host: it
// carries no measured OS/arch and no measured tool versions, so those checks do not apply. Its Pins are the
// commit's declared pins, exactly as a local record's.
func judgeGitHubRecord(record VerificationRecord, members map[string]json.RawMessage, want VerificationKeys) (VerificationRecord, error) {
	conflict := func(format string, args ...any) error {
		return refuse(contract.RefusalDispositionConflict, format, args...)
	}
	if record.VerifierKind != GitHubVerifierKind || record.VerifierRules != GitHubVerifierRules {
		return record, conflict("the verification record's verifier is %s/%s, not %s/%s", record.VerifierKind, record.VerifierRules, GitHubVerifierKind, GitHubVerifierRules)
	}
	gh := record.GitHub
	if gh == nil {
		return record, conflict("the verification record names runner github-actions but carries no github member")
	}
	if !ghJudgeRepository.MatchString(record.Repository) {
		return record, conflict("the verification record's repository %q is not OWNER/REPO", record.Repository)
	}
	if !ghJudgeHex.MatchString(record.HeadCommit) || !ghJudgeHex.MatchString(gh.RunHead) ||
		!ghJudgeHex.MatchString(gh.Scan.Base) || !ghJudgeHex.MatchString(gh.Scan.Head) {
		return record, conflict("the verification record names commits that are not full object IDs")
	}
	if gh.RunID <= 0 || gh.Branch == "" || gh.WorkflowID <= 0 {
		return record, conflict("the verification record's GitHub provenance is incomplete")
	}
	if !ghJudgeWorkflowFile.MatchString(gh.WorkflowPath) || strings.Contains(gh.WorkflowPath, "..") {
		return record, conflict("the verification record's workflow path %q is not a file below .github/workflows/", gh.WorkflowPath)
	}
	if !ghJudgeSHA256.MatchString(gh.WorkflowDigest) {
		return record, conflict("the verification record's workflow digest %q is not sha256:<hex>", gh.WorkflowDigest)
	}
	if want := fmt.Sprintf("https://github.com/%s/actions/runs/%d", record.Repository, gh.RunID); gh.URL != want {
		return record, conflict("the verification record's run URL %q is not %q", gh.URL, want)
	}
	if gh.Event != "workflow_dispatch" {
		return record, conflict("the verification record's run event is %q, not workflow_dispatch", gh.Event)
	}
	if gh.Attempts != 1 && gh.Attempts != 2 || len(gh.Runs) != gh.Attempts {
		return record, conflict("the verification record claims %d attempts in %d run rows", gh.Attempts, len(gh.Runs))
	}
	// the attempt rows are exactly attempts 1 and 2, completed, with nonempty uniquely named jobs of
	// recognized terminal verdicts; the last attempt is the run's conclusion.
	var merged []struct {
		name       string
		id         int64
		status     string
		conclusion string
		result     string
	}
	mergedIndex := map[string]int{}
	for i, run := range gh.Runs {
		if run.Attempt != i+1 {
			return record, conflict("the verification record's run %d is attempt %d, not %d", i, run.Attempt, i+1)
		}
		if run.Status != "completed" {
			return record, conflict("the verification record's run attempt %d is %s, not completed", run.Attempt, run.Status)
		}
		if !ghJudgeTerminal[run.Conclusion] {
			return record, conflict("the verification record's run attempt %d ended %q, not a recognized conclusion", run.Attempt, run.Conclusion)
		}
		if len(run.Jobs) == 0 {
			return record, conflict("the verification record's run attempt %d has no jobs", run.Attempt)
		}
		seen := map[string]bool{}
		for j, raw := range run.Jobs {
			var job struct {
				Name       string `json:"name"`
				DatabaseID int64  `json:"databaseId"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Result     string `json:"result"`
			}
			if err := json.Unmarshal(raw, &job); err != nil {
				return record, conflict("the verification record's job %d of attempt %d could not be read", j, run.Attempt)
			}
			if job.Name == "" {
				return record, conflict("the verification record's job %d of attempt %d has no name", j, run.Attempt)
			}
			if seen[job.Name] {
				return record, conflict("the verification record's attempt %d names job %q twice", run.Attempt, job.Name)
			}
			seen[job.Name] = true
			if job.DatabaseID <= 0 {
				return record, conflict("the verification record's job %q has no positive id", job.Name)
			}
			if job.Status != "completed" || !ghJudgeTerminal[job.Conclusion] ||
				(job.Result != "passed" && job.Result != "failed") {
				return record, conflict("the verification record's job %q is %s/%s/%s, not a completed terminal job", job.Name, job.Status, job.Conclusion, job.Result)
			}
			entry := struct {
				name       string
				id         int64
				status     string
				conclusion string
				result     string
			}{job.Name, job.DatabaseID, job.Status, job.Conclusion, job.Result}
			if at, ok := mergedIndex[job.Name]; ok {
				merged[at] = entry
			} else {
				mergedIndex[job.Name] = len(merged)
				merged = append(merged, entry)
			}
		}
	}
	last := gh.Runs[len(gh.Runs)-1]
	if last.Conclusion != "success" || gh.Conclusion != "success" {
		return record, conflict("the verification record's run conclusion is not success")
	}
	if len(gh.Runs) == 2 {
		if gh.Runs[0].Conclusion != "failure" || !gh.FlakeRerunPassed {
			return record, conflict("the verification record reran and passed but does not record the flake")
		}
	} else if gh.FlakeRerunPassed {
		return record, conflict("the verification record records a rerun it did not make")
	}
	if len(record.Jobs) == 0 {
		return record, conflict("the verification record has no jobs: a run without jobs did not verify the head")
	}
	// the top-level jobs are the latest-by-name merge of the attempts: every one completed, succeeded and
	// passed, with the attempt's own id and verdict — a formerly failed job missing from the retry stays
	// failed and cannot be hidden.
	seen := map[string]bool{}
	for i, raw := range record.Jobs {
		var job struct {
			Name       string `json:"name"`
			DatabaseID int64  `json:"databaseId"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Result     string `json:"result"`
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return record, conflict("the verification record's job %d could not be read", i)
		}
		if job.Name == "" || seen[job.Name] {
			return record, conflict("the verification record's job %d is unnamed or repeats a name", i)
		}
		seen[job.Name] = true
		if job.DatabaseID <= 0 || job.Status != "completed" || job.Conclusion != "success" || job.Result != "passed" {
			return record, conflict("the verification record's job %q is not a passed successful completed job", job.Name)
		}
		at, ok := mergedIndex[job.Name]
		if !ok {
			return record, conflict("the verification record's job %q is in no attempt", job.Name)
		}
		m := merged[at]
		if m.id != job.DatabaseID || m.status != job.Status || m.conclusion != job.Conclusion || m.result != job.Result {
			return record, conflict("the verification record's job %q does not match the attempt that produced it", job.Name)
		}
	}
	for _, m := range merged {
		if !seen[m.name] {
			return record, conflict("the verification record's jobs hide attempt job %q", m.name)
		}
	}
	if gh.Scan.Base != want.Base || gh.Scan.Head != record.HeadCommit || gh.Scan.Result != "clean" {
		return record, conflict("the verification record's secret scan does not cover the range it verifies cleanly")
	}
	if reused := gh.ReusedFrom; reused != nil {
		if reused.VerifierKind != GitHubVerifierKind || !ghJudgeSHA256.MatchString(reused.Digest) || reused.Head != gh.RunHead {
			return record, conflict("the verification record's reusedFrom is not the github-actions receipt of its run head")
		}
	} else if gh.RunHead != record.HeadCommit {
		return record, conflict("the verification record's run head %s is not the head it verifies %s and names no reusedFrom", gh.RunHead, record.HeadCommit)
	}
	// the remote record claims no measured host, tools or versions: the workflow definition is the
	// environment and the pins are the commit's declarations.
	if record.OS != "" || record.Arch != "" {
		return record, conflict("the verification record claims a measured host %s/%s: a remote record declares no host", record.OS, record.Arch)
	}
	if len(record.Tools) != 0 {
		return record, conflict("the verification record claims measured tools %v: a remote record names the commit's declared pins only", record.Tools)
	}
	if pins := members["pins"]; len(pins) == 0 || string(pins) == "null" {
		return record, conflict("the verification record's pins member is null: a record without the pins of its toolchain is not reusable")
	}
	if want.Pins == nil {
		return record, conflict("the pins the verified commit declares were not read, so the verification record's pins cannot be judged")
	}
	for _, name := range pinNames(want.Pins) {
		declared := want.Pins[name]
		if pin, ok := record.Pins[name]; !ok || pin != declared {
			return record, conflict("the verification record pins %s at %q but the verified commit declares %q: the record is not reusable", name, record.Pins[name], declared)
		}
	}
	for _, name := range pinNames(record.Pins) {
		if _, ok := want.Pins[name]; !ok {
			return record, conflict("the verification record pins %s, which the verified commit does not declare: the record is not reusable", name)
		}
	}
	if !strings.EqualFold(record.TreeHash, want.Tree) {
		return record, refuse(contract.RefusalRevisionMismatch, "the verification record names tree %s and the tree judged is %s", record.TreeHash, want.Tree)
	}
	if record.BaseCommit != want.Base {
		return record, conflict("the verification record covers base %s and the base is %s", record.BaseCommit, want.Base)
	}
	if record.CiDigest != want.CiDigest {
		return record, conflict("the verification record's ci.yml digest %s is not the commit's %s", record.CiDigest, want.CiDigest)
	}
	for _, file := range verificationDependencyFiles {
		if record.Dependencies[file] != want.Dependencies[file] {
			return record, conflict("the verification record's %s digest %q is not the commit's %q", file, record.Dependencies[file], want.Dependencies[file])
		}
	}
	return record, nil
}
