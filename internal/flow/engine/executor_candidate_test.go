package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func reviewCandidate(t *testing.T) executor.CollectedExecution {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=fixture", "GIT_COMMITTER_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-b", "dev")
	git("commit", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "candidate")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	task := enTask("n1", "check the controller criteria", Pair{Model: "synthetic", Effort: "high"})
	task.Identity.CriteriaDigest = delivery.SetDigest(externalReviewCriteria())
	task.Repository.ID = "owner/repo"
	task.Repository.BaseCommit = base
	task.Regions = []executor.Region{{Repository: "owner/repo", Kind: "tree", Path: "."}}
	a, err := executor.Freeze(task)
	if err != nil {
		t.Fatal(err)
	}
	return executor.CollectedExecution{Admission: a, ExecutionID: task.Identity.ExecutionID, Candidate: executor.CollectedCandidate{Repository: task.Repository.ID, Checkout: dir, Head: git("rev-parse", "HEAD"), Tree: git("rev-parse", "HEAD^{tree}"), Base: base}, Receipt: executor.Acknowledgement{State: "received", ExecutionID: task.Identity.ExecutionID, TaskDigest: a.TaskDigest, ResultDigest: executor.Digest([]byte("result"))}}
}
func TestExternalCandidateReviewIsActualEngineCall(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "owner changed during review"}[stale], func(t *testing.T) {
			h := newHarness(t)
			bound := reviewCandidate(t)
			current := true
			h.cfg.ExecutorCandidate = func(context.Context, SnapshotRequest) (*ExecutorCandidateReview, error) {
				if !current {
					return nil, errors.New("owner epoch changed")
				}
				return &ExecutorCandidateReview{CollectedExecution: bound, Criteria: externalReviewCriteria()}, nil
			}
			h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
				if spec.Cwd != bound.Candidate.Checkout || spec.Head != bound.Candidate.Head {
					t.Errorf("review did not use collection: %+v", spec)
				}
				return ThreadInfo{Pair: spec.Pair, Cwd: spec.Cwd}, nil
			}
			h.fake.Handler = func(req FakeRequest) FakeReply {
				if !strings.Contains(req.Prompt, "current acceptance title") {
					t.Error("review received only the criteria digest, not current criteria text")
				}
				if !strings.Contains(req.Prompt, bound.Admission.Task.Identity.CriteriaDigest) {
					t.Error("review did not receive current criteria")
				}
				if stale {
					current = false
				}
				return FakeReply{}
			}
			res := h.run(`return await agent.call('review it', {agentType:'reviewer',node:'n1',schema:VERDICT})`)
			if res.Status != RunCompleted {
				t.Fatalf("run: %+v", res)
			}
			n := 0
			for _, ev := range h.journal.Events() {
				if ev.Kind == KindCall && ev.Event == EvCompleted {
					n++
					if ev.Data["execution_id"] != bound.ExecutionID || ev.Data["head"] != bound.Candidate.Head || ev.Data["criteria_digest"] != bound.Admission.Task.Identity.CriteriaDigest {
						t.Fatalf("unbound genuine call: %+v", ev)
					}
				}
			}
			if stale && n != 0 || !stale && n != 1 {
				t.Fatalf("completed reviews %d (stale=%t)", n, stale)
			}
			if len(h.fake.Requests()) != 2 {
				t.Fatalf("expected actual thread and turn: %+v", h.fake.Requests())
			}
		})
	}
}

func externalReviewCriteria() []delivery.Criterion {
	return []delivery.Criterion{{ID: "criterion", Title: "current acceptance title", Required: true}}
}

// CRW-1667: the replay key of a candidate review binds the collected execution,
// not only its visible packet. A handover (same checkout, head, prompt and
// criteria, another execution) must open a fresh review on resume, while an
// unchanged collection keeps replaying.
func TestCRW1667CandidateReviewReplayKey(t *testing.T) {
	const body = `return await agent.call('review it', {agentType:'reviewer',node:'n1',schema:VERDICT,label:'review'})`
	cases := []struct {
		name   string
		change func(t *testing.T, bound *executor.CollectedExecution)
		fresh  bool
	}{
		{"same collection", func(t *testing.T, bound *executor.CollectedExecution) {}, false},
		{"handover", func(t *testing.T, bound *executor.CollectedExecution) {
			bound.ExecutionID = "exec-handover" // the controller's successor only; Admission and Receipt stay immutable
		}, true},
		{"task digest", func(t *testing.T, bound *executor.CollectedExecution) {
			task := bound.Admission.Task
			task.Identity.Revision = "r1"
			a, err := executor.Freeze(task)
			if err != nil {
				t.Fatal(err)
			}
			bound.Admission = a
			bound.Receipt.TaskDigest = a.TaskDigest
		}, true},
		{"receipt result digest", func(t *testing.T, bound *executor.CollectedExecution) {
			bound.Receipt.ResultDigest = executor.Digest([]byte("second authenticated result"))
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			bound := reviewCandidate(t)
			h.cfg.ExecutorCandidate = func(context.Context, SnapshotRequest) (*ExecutorCandidateReview, error) {
				return &ExecutorCandidateReview{CollectedExecution: bound, Criteria: externalReviewCriteria()}, nil
			}
			h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
				if spec.Cwd != bound.Candidate.Checkout || spec.Head != bound.Candidate.Head {
					t.Errorf("review did not use collection: %+v", spec)
				}
				return ThreadInfo{Pair: spec.Pair, Cwd: spec.Cwd}, nil
			}
			res := h.run(body)
			if res.Status != RunCompleted {
				t.Fatalf("run: %+v", res)
			}
			if got := value[map[string]any](t, res); got["status"] != "completed" {
				t.Fatalf("call: %v", got)
			}
			if n := len(h.fake.Requests()); n != 2 {
				t.Fatalf("first review used %d requests, want 2", n)
			}
			rowsOf := func(label string) (preps, comps []Event) {
				for _, ev := range h.callRows(label) {
					switch ev.Event {
					case EvPrepared:
						preps = append(preps, ev)
					case EvCompleted:
						comps = append(comps, ev)
					}
				}
				return preps, comps
			}
			preps, comps := rowsOf("review")
			if len(preps) != 1 || len(comps) != 1 {
				t.Fatalf("first review rows: %d prepared, %d completed", len(preps), len(comps))
			}
			tc.change(t, &bound)
			if err := bound.Inspect(context.Background()); err != nil {
				t.Fatalf("the mutated fixture is not a valid collection: %v", err)
			}
			h.cfg.Resume = &ResumeOptions{}
			res = h.run(body)
			if res.Status != RunCompleted {
				t.Fatalf("resumed run: %+v", res)
			}
			wantRequests, wantComps := 2, 1
			if tc.fresh {
				wantRequests, wantComps = 4, 2
			}
			if n := len(h.fake.Requests()); n != wantRequests {
				t.Fatalf("%s: %d requests, want %d", tc.name, n, wantRequests)
			}
			preps, comps = rowsOf("review")
			if len(preps) != wantComps || len(comps) != wantComps {
				t.Fatalf("%s: %d prepared, %d completed rows, want %d each", tc.name, len(preps), len(comps), wantComps)
			}
			if tc.fresh {
				last := comps[len(comps)-1]
				if last.Data["execution_id"] != bound.ExecutionID || last.Data["executor_result_digest"] != bound.Receipt.ResultDigest {
					t.Fatalf("the fresh review is not bound to the current collection: %+v", last.Data)
				}
				lastPrep, firstPrep := preps[len(preps)-1], preps[0]
				if lastPrep.Data["input_digest"] == firstPrep.Data["input_digest"] {
					t.Fatalf("the replay key did not change with the collection")
				}
				for _, k := range []string{"prompt_digest", "head", "cwd", "criteria_digest"} {
					if lastPrep.Data[k] != firstPrep.Data[k] {
						t.Errorf("visible input %s changed: %v vs %v", k, lastPrep.Data[k], firstPrep.Data[k])
					}
				}
			}
			// An unchanged collection replays again: no more requests or rows.
			res = h.run(body)
			if res.Status != RunCompleted {
				t.Fatalf("second resume: %+v", res)
			}
			if n := len(h.fake.Requests()); n != wantRequests {
				t.Fatalf("%s: unchanged collection made %d requests, want %d", tc.name, n, wantRequests)
			}
			if preps, comps = rowsOf("review"); len(preps) != wantComps || len(comps) != wantComps {
				t.Fatalf("%s: unchanged collection wrote rows %d/%d, want %d", tc.name, len(preps), len(comps), wantComps)
			}
		})
	}
}

// A review that never carried a candidate keeps the pre-CRW-1667 digest bytes,
// so a journal a run before the change wrote still replays.
func TestCRW1667OrdinaryReviewKeepsLegacyDigest(t *testing.T) {
	const body = `return await agent.call('review it', {agentType:'reviewer',node:'n1',schema:VERDICT,label:'review'})`
	h := newHarness(t)
	res := h.run(body)
	if res.Status != RunCompleted {
		t.Fatalf("run: %+v", res)
	}
	var preps, comps []Event
	for _, ev := range h.callRows("review") {
		switch ev.Event {
		case EvPrepared:
			preps = append(preps, ev)
		case EvCompleted:
			comps = append(comps, ev)
		}
	}
	if len(preps) != 1 || len(comps) != 1 {
		t.Fatalf("rows %d/%d", len(preps), len(comps))
	}
	prep := preps[0]
	legacy := digestJSON(mustJSON(map[string]any{
		"prompt": rowStr(prep.Data, "prompt_digest"), "schema": rowStr(prep.Data, "schema_digest"),
		"role": rowStr(prep.Data, "role"), "model": rowStr(prep.Data, "model"), "effort": rowStr(prep.Data, "effort"),
		"node": rowStr(prep.Data, "node"), "step": rowStr(prep.Data, "step_decision"),
		"head": rowStr(prep.Data, "head"), "cwd": rowStr(prep.Data, "cwd"),
		"context": prep.Data["context"], "joins": "",
	}))
	if prep.Data["input_digest"] != legacy {
		t.Fatalf("ordinary review digest changed: %v, want %s", prep.Data["input_digest"], legacy)
	}
	h.cfg.Resume = &ResumeOptions{}
	if res = h.run(body); res.Status != RunCompleted {
		t.Fatalf("resumed run: %+v", res)
	}
	if n := len(h.fake.Requests()); n != 2 {
		t.Fatalf("resume made %d requests, want 2", n)
	}
	comps = comps[:0]
	for _, ev := range h.callRows("review") {
		if ev.Event == EvCompleted {
			comps = append(comps, ev)
		}
	}
	if len(comps) != 1 {
		t.Fatalf("resume wrote %d completed rows, want the replayed one", len(comps))
	}
}
