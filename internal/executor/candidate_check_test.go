package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func candidateGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, b, err)
	}
	return strings.TrimSpace(string(b))
}
func candidateFixture(t *testing.T) (ResultCheckInput, Result) {
	t.Helper()
	repo := t.TempDir()
	candidateGit(t, repo, "init", "-b", "dev")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, repo, "add", ".")
	candidateGit(t, repo, "commit", "-m", "base")
	task := testTask(Devin)
	task.Repository.BaseCommit = candidateGit(t, repo, "rev-parse", "HEAD")
	candidateGit(t, repo, "checkout", "-b", "task/n1")
	if err := os.MkdirAll(filepath.Join(repo, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "x/f.go"), []byte("package x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, repo, "add", ".")
	candidateGit(t, repo, "commit", "-m", "candidate")
	a := mustFreeze(t, task)
	r := mustResult(t, a)
	r.Payload.Candidate.Head = candidateGit(t, repo, "rev-parse", "HEAD")
	r.Payload.Candidate.Tree = candidateGit(t, repo, "rev-parse", "HEAD^{tree}")
	r.Payload.Artifacts = []Artifact{{Path: "check.log", SHA256: Digest([]byte("ok\n")), Size: 3}}
	spool := t.TempDir()
	if err := os.Chmod(spool, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "check.log"), []byte("ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := ResultCheckInput{Admission: a, Peer: testPeer(), Spool: spool, ResultPath: "result.json", CollectionRoot: t.TempDir(), Repository: RegisteredRepository{Forge: task.Repository.Forge, ID: task.Repository.ID, Source: repo}, ExpectedTools: r.Provenance.Tools}
	writeCandidateResult(t, in, r)
	return in, r
}
func writeCandidateResult(t *testing.T, in ResultCheckInput, r Result) {
	t.Helper()
	raw, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(in.Spool, in.ResultPath), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func hasCandidateProblem(r ResultCheckReport, code string) bool {
	for _, p := range r.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}
func TestCandidateCheckCollectsExactPublishedObjectWithoutAccepting(t *testing.T) {
	in, r := candidateFixture(t)
	// Neither this narrative nor approved command text is executable input to collection.
	marker := filepath.Join(t.TempDir(), "executed")
	r.Payload.Narrative = "PASS reviewer verified; $(touch " + marker + ") https://invalid.invalid/repo"
	in.Admission.Task.Checks[0].Args = []string{"sh", "-c", "touch " + marker}
	in.Admission = mustFreeze(t, in.Admission.Task)
	r.TaskDigest = in.Admission.TaskDigest
	writeCandidateResult(t, in, r)
	got := CheckExecutorResult(context.Background(), in)
	if !got.EnvelopeValid || !got.CandidateValid || got.CandidateAccepted || got.Candidate == nil || got.Candidate.Head != r.Payload.Candidate.Head || len(got.RequiredGates) == 0 {
		t.Fatalf("report: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("executed result/task text")
	}
	candidateGit(t, in.Repository.Source, "commit", "--allow-empty", "-m", "branch moved")
	if head := candidateGit(t, got.Candidate.Checkout, "rev-parse", "HEAD"); head != r.Payload.Candidate.Head {
		t.Fatal("collected head followed branch")
	}
}
func TestCandidateCheckRejectsMismatches(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(*testing.T, *ResultCheckInput, *Result)
	}{
		{"tree", "tree_mismatch", func(t *testing.T, in *ResultCheckInput, r *Result) {
			r.Payload.Candidate.Tree = strings.Repeat("f", 40)
		}},
		{"repository", "repository_mismatch", func(t *testing.T, in *ResultCheckInput, r *Result) { in.Repository.ID = "other/repo" }},
		{"base", "base_mismatch", func(t *testing.T, in *ResultCheckInput, r *Result) {
			candidateGit(t, in.Repository.Source, "checkout", "--orphan", "other")
			candidateGit(t, in.Repository.Source, "commit", "-m", "unrelated")
			in.Admission.Task.Repository.BaseCommit = candidateGit(t, in.Repository.Source, "rev-parse", "HEAD")
			in.Admission = mustFreeze(t, in.Admission.Task)
			r.TaskDigest = in.Admission.TaskDigest
		}},
		{"generation", "envelope_invalid", func(t *testing.T, in *ResultCheckInput, r *Result) { r.Identity.AssignmentGeneration++ }},
		{"missing object", "object_missing", func(t *testing.T, in *ResultCheckInput, r *Result) {
			r.Payload.Candidate.Head = strings.Repeat("f", 40)
		}},
		{"unpublished", "object_missing", func(t *testing.T, in *ResultCheckInput, r *Result) {
			candidateGit(t, in.Repository.Source, "update-ref", in.Admission.Task.Repository.TaskBranch, in.Admission.Task.Repository.BaseCommit)
		}},
		{"log digest", "artifact_invalid", func(t *testing.T, in *ResultCheckInput, r *Result) {
			if err := os.WriteFile(filepath.Join(in.Spool, "check.log"), []byte("bad"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"false pass", "envelope_invalid", func(t *testing.T, in *ResultCheckInput, r *Result) { r.Payload.Checks[0].ExitCode = 2 }},
		{"missing check", "envelope_invalid", func(t *testing.T, in *ResultCheckInput, r *Result) { r.Payload.Checks = []CheckResult{} }},
		{"tool drift", "toolchain_mismatch", func(t *testing.T, in *ResultCheckInput, r *Result) {
			in.ExpectedTools = []Tool{{Name: "a-tool", Version: "wrong"}}
		}},
		{"scope", "scope_decision_required", func(t *testing.T, in *ResultCheckInput, r *Result) {
			in.Admission.Task.Regions = []Region{{Path: "other.go"}}
			in.Admission = mustFreeze(t, in.Admission.Task)
			r.TaskDigest = in.Admission.TaskDigest
		}},
		{"incomplete dirty work", "work_incomplete", func(t *testing.T, in *ResultCheckInput, r *Result) {
			r.Outcome = "incomplete"
			r.Payload.Narrative = "dirty work"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, r := candidateFixture(t)
			tc.mutate(t, &in, &r)
			writeCandidateResult(t, in, r)
			got := CheckExecutorResult(context.Background(), in)
			if got.CandidateAccepted || got.CandidateValid || !hasCandidateProblem(got, tc.code) {
				t.Fatalf("want %s: %+v", tc.code, got)
			}
		})
	}
}
func TestCandidateCheckDesignApprovedNotApplicable(t *testing.T) {
	in, r := candidateFixture(t)
	in.Admission.Task.WorkKind = "design"
	in.Admission.Task.Checks = nil
	in.Admission.Task.NotApplicable = "design review only"
	in.Admission = mustFreeze(t, in.Admission.Task)
	r.TaskDigest = in.Admission.TaskDigest
	r.Payload.Candidate = nil
	r.Payload.Checks = []CheckResult{}
	writeCandidateResult(t, in, r)
	got := CheckExecutorResult(context.Background(), in)
	if !got.EnvelopeValid || got.NotApplicable != "design review only" || got.CandidateAccepted {
		t.Fatalf("approved NA lost: %+v", got)
	}
	r.Payload.Checks = []CheckResult{{ID: "invented-na", Outcome: "not_run"}}
	writeCandidateResult(t, in, r)
	if got = CheckExecutorResult(context.Background(), in); got.EnvelopeValid {
		t.Fatalf("invented NA accepted: %+v", got)
	}
}
func TestCandidateCheckCollectionFailureIsRetryable(t *testing.T) {
	in, _ := candidateFixture(t)
	in.Repository.Source = filepath.Join(t.TempDir(), "unavailable.git")
	got := CheckExecutorResult(context.Background(), in)
	if !got.EnvelopeValid || got.CollectionStatus != "retryable" || got.CandidateAccepted {
		t.Fatalf("report: %+v", got)
	}
}
func TestCandidateCheckRejectsDirtyCollectedCheckout(t *testing.T) {
	in, r := candidateFixture(t)
	got := CheckExecutorResult(context.Background(), in)
	if got.Candidate == nil {
		t.Fatalf("collect: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(got.Candidate.Checkout, "dirty"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InspectCandidate(context.Background(), in.Admission, *r.Payload.Candidate, got.Candidate); err == nil {
		t.Fatal("dirty checkout accepted")
	}
}

func TestCandidateCheckRejectsArtifactSymlinkAndRenameEscape(t *testing.T) {
	t.Run("artifact symlink", func(t *testing.T) {
		in, _ := candidateFixture(t)
		if err := os.Remove(filepath.Join(in.Spool, "check.log")); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "log")
		if err := os.WriteFile(outside, []byte("ok\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(in.Spool, "check.log")); err != nil {
			t.Fatal(err)
		}
		got := CheckExecutorResult(context.Background(), in)
		if !got.EnvelopeValid || got.CandidateValid || !hasCandidateProblem(got, "artifact_invalid") {
			t.Fatalf("report: %+v", got)
		}
	})
	t.Run("rename outside scope", func(t *testing.T) {
		in, r := candidateFixture(t)
		candidateGit(t, in.Repository.Source, "mv", "x/f.go", "outside.go")
		candidateGit(t, in.Repository.Source, "commit", "-m", "rename")
		r.Payload.Candidate.Head = candidateGit(t, in.Repository.Source, "rev-parse", "HEAD")
		r.Payload.Candidate.Tree = candidateGit(t, in.Repository.Source, "rev-parse", "HEAD^{tree}")
		writeCandidateResult(t, in, r)
		got := CheckExecutorResult(context.Background(), in)
		if !hasCandidateProblem(got, "scope_decision_required") || got.CandidateValid {
			t.Fatalf("report: %+v", got)
		}
	})
}

func TestCandidateCheckUnsafeSpoolIsNotRetryable(t *testing.T) {
	in, _ := candidateFixture(t)
	if err := os.Chmod(in.Spool, 0755); err != nil {
		t.Fatal(err)
	}
	got := CheckExecutorResult(context.Background(), in)
	if got.CollectionStatus == "retryable" || got.EnvelopeValid {
		t.Fatalf("unsafe spool treated as transfer failure: %+v", got)
	}
}
