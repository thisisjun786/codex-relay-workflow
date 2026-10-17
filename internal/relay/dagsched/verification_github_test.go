package dagsched

import (
	"encoding/json"
	"strings"
	"testing"
)

// ghRemoteRecord is a passing remote verification receipt of CRW-1594: runner github-actions, the remote
// provenance complete, every job passed, the scan of the current range clean and the declared pins carried
// (remote Tools is empty: the remote workflow is the environment, not a measurement of the host).
func ghRemoteRecord(base, head, tree string, deps, pins map[string]string) VerificationRecord {
	job, _ := json.Marshal(map[string]any{
		"name": "build", "status": "completed", "conclusion": "success", "result": "passed", "databaseId": 101,
		"url":   "https://github.com/owner/repo/actions/runs/6/job/101",
		"steps": []map[string]any{{"name": "test", "number": 1, "status": "completed", "conclusion": "success", "result": "passed"}},
	})
	return VerificationRecord{
		Runner:        GitHubRunner,
		Repository:    "owner/repo",
		BaseCommit:    base,
		HeadCommit:    head,
		TreeHash:      tree,
		CiDigest:      "sha256:" + strings.Repeat("a", 64),
		Tools:         map[string]string{},
		Pins:          pins,
		GoFlags:       "",
		GoEnv:         "",
		PinMismatch:   []string{},
		Dependencies:  deps,
		OS:            "",
		Arch:          "",
		Result:        "pass",
		Jobs:          []json.RawMessage{job},
		VerifierKind:  GitHubVerifierKind,
		VerifierRules: GitHubVerifierRules,
		GitHub: &GitHubVerification{
			RunID:          6,
			URL:            "https://github.com/owner/repo/actions/runs/6",
			RunHead:        head,
			Branch:         "ci/verify-abcdef12",
			Event:          "workflow_dispatch",
			WorkflowID:     7,
			WorkflowPath:   ".github/workflows/ci.yml",
			WorkflowDigest: "sha256:" + strings.Repeat("f", 64),
			Conclusion:     "success",
			Attempts:       1,
			Runs: []GitHubRunAttempt{{Attempt: 1, Status: "completed", Conclusion: "success",
				Jobs: []json.RawMessage{job}}},
			Scan: GitHubScan{Base: base, Head: head, Result: "clean"},
		},
	}
}

func ghWant(base string, deps, pins map[string]string) VerificationKeys {
	return VerificationKeys{Tree: "tree-1", Base: base, CiDigest: "sha256:" + strings.Repeat("a", 64), Dependencies: deps, Pins: pins,
		OS: "windows", Arch: "arm64"}
}

func ghDeps() map[string]string {
	return map[string]string{"go.sum": "sha256:" + strings.Repeat("5", 64), "web/package-lock.json": "sha256:" + strings.Repeat("6", 64)}
}

// CRW-1594: a sealed remote receipt is accepted for the commit's keys — tree, base, ci.yml, dependencies and
// declared pins — on a host of any platform: a remote record carries no measured OS/arch or tool versions.
func TestJudgeVerificationRecordAcceptsARemoteReceipt(t *testing.T) {
	base, head := strings.Repeat("b", 40), strings.Repeat("c", 40)
	record := ghRemoteRecord(base, head, "tree-1", ghDeps(), map[string]string{"go": "1.27"})
	raw, err := SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{"go": "1.27"})); err != nil {
		t.Fatalf("a sealed remote receipt: %v", err)
	}
	// and a second acceptance on a different caller platform is the same answer
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{"go": "1.27"})); err != nil {
		t.Fatalf("platform independence: %v", err)
	}
}

// CRW-1594: a remote receipt whose run head is not the head it verifies is accepted only through a
// well-formed reusedFrom: a nonempty source digest and head, and the source head is the run's head.
func TestJudgeVerificationRecordAcceptsAReusedRemoteReceipt(t *testing.T) {
	base, head, runHead := strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	record := ghRemoteRecord(base, head, "tree-1", ghDeps(), map[string]string{})
	record.GitHub.RunHead = runHead
	record.GitHub.ReusedFrom = &GitHubReusedFrom{Digest: "sha256:" + strings.Repeat("b", 64), Head: runHead, VerifierKind: "github-actions"}
	raw, err := SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{})); err != nil {
		t.Fatalf("a well-formed reusedFrom: %v", err)
	}
	// a run head that is not the verified head and no provenance for it: refused
	record.GitHub.ReusedFrom = nil
	raw, err = SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{})); err == nil {
		t.Error("a foreign run head without reusedFrom was accepted")
	}
	// a reusedFrom that names a different head: refused
	record.GitHub.ReusedFrom = &GitHubReusedFrom{Digest: "sha256:" + strings.Repeat("b", 64), Head: head, VerifierKind: "github-actions"}
	raw, err = SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{})); err == nil {
		t.Error("a reusedFrom that names another head was accepted")
	}
	// a reusedFrom without a digest: refused
	record.GitHub.ReusedFrom = &GitHubReusedFrom{Head: runHead, VerifierKind: "github-actions"}
	raw, err = SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), map[string]string{})); err == nil {
		t.Error("a digestless reusedFrom was accepted")
	}
}

// CRW-1594: the remote receipt is refused like any other when its contents fail: a tampered digest, missing
// or wrong provenance, missing or non-passing jobs, a stale scan range, pins that are not the commit's
// declarations, or a runner/kind combination this branch does not cover.
func TestJudgeVerificationRecordRefusesRemoteForgery(t *testing.T) {
	base, head := strings.Repeat("b", 40), strings.Repeat("c", 40)
	pins := map[string]string{"go": "1.27"}
	want := ghWant(base, ghDeps(), pins)
	seal := func(record VerificationRecord) []byte {
		raw, err := SealVerificationRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	good := ghRemoteRecord(base, head, "tree-1", ghDeps(), pins)

	// a tampered digest
	raw := seal(good)
	raw[len(raw)-3] ^= 1
	if _, err := JudgeVerificationRecord(raw, want); err == nil {
		t.Error("tampered bytes accepted")
	}
	raw = seal(good)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["result"] = "fail" // digest no longer seals the contents
	forged, _ := json.Marshal(doc)
	if _, err := JudgeVerificationRecord(forged, want); err == nil {
		t.Error("a digest that does not cover the contents accepted")
	}

	cases := map[string]func(r *VerificationRecord){
		"no github provenance":  func(r *VerificationRecord) { r.GitHub = nil },
		"another verifier kind": func(r *VerificationRecord) { r.VerifierKind = "local" },
		"another rules version": func(r *VerificationRecord) { r.VerifierRules = "github-actions/2" },
		"a wrong URL":           func(r *VerificationRecord) { r.GitHub.URL = "https://example.com/runs/6" },
		"a wrong event":         func(r *VerificationRecord) { r.GitHub.Event = "push" },
		"a failed conclusion":   func(r *VerificationRecord) { r.GitHub.Conclusion = "failure" },
		"too many attempts":     func(r *VerificationRecord) { r.GitHub.Attempts = 3 },
		"no jobs":               func(r *VerificationRecord) { r.Jobs = nil },
		"a failed job": func(r *VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "result": "failed", "conclusion": "failure"})
			r.Jobs = []json.RawMessage{job}
		},
		"a job that hides its conclusion": func(r *VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "result": "passed"})
			r.Jobs = []json.RawMessage{job}
		},
		"a stale scan base":                 func(r *VerificationRecord) { r.GitHub.Scan.Base = strings.Repeat("9", 40) },
		"a stale scan head":                 func(r *VerificationRecord) { r.GitHub.Scan.Head = strings.Repeat("8", 40) },
		"a dirty scan":                      func(r *VerificationRecord) { r.GitHub.Scan.Result = "findings" },
		"a pin the commit does not declare": func(r *VerificationRecord) { r.Pins = map[string]string{"go": "1.28"} },
		"a missing declared pin":            func(r *VerificationRecord) { r.Pins = nil },
		"a measured tool claim":             func(r *VerificationRecord) { r.Tools = map[string]string{"go": "9.9"} },
		"a run history of no rows":          func(r *VerificationRecord) { r.GitHub.Runs = nil },
		"an attempt count that lies": func(r *VerificationRecord) {
			r.GitHub.Attempts = 1
			r.GitHub.Runs = append(r.GitHub.Runs, r.GitHub.Runs[0])
		},
		"an attempt out of order":    func(r *VerificationRecord) { r.GitHub.Runs[0].Attempt = 3 },
		"a run that never completed": func(r *VerificationRecord) { r.GitHub.Runs[0].Status = "in_progress" },
		"a run of no jobs":           func(r *VerificationRecord) { r.GitHub.Runs[0].Jobs = nil },
		"a job name used twice in an attempt": func(r *VerificationRecord) {
			r.GitHub.Runs[0].Jobs = append(r.GitHub.Runs[0].Jobs, r.GitHub.Runs[0].Jobs[0])
		},
		"a job that never completed": func(r *VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "databaseId": 101, "status": "in_progress", "conclusion": "success", "result": "passed"})
			r.GitHub.Runs[0].Jobs = []json.RawMessage{job}
		},
		"an unrecognized job conclusion": func(r *VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "databaseId": 101, "status": "completed", "conclusion": "mystery", "result": "passed"})
			r.GitHub.Runs[0].Jobs = []json.RawMessage{job}
		},
		"a job with no id": func(r *VerificationRecord) {
			job, _ := json.Marshal(map[string]any{"name": "build", "status": "completed", "conclusion": "success", "result": "passed"})
			r.GitHub.Runs[0].Jobs = []json.RawMessage{job}
		},
		"a rerun nobody recorded": func(r *VerificationRecord) {
			r.GitHub.Attempts = 2
			failed := GitHubRunAttempt{Attempt: 1, Status: "completed", Conclusion: "failure", Jobs: r.GitHub.Runs[0].Jobs}
			r.GitHub.Runs = []GitHubRunAttempt{failed, {Attempt: 2, Status: "completed", Conclusion: "success", Jobs: r.GitHub.Runs[0].Jobs}}
		},
		"a rerun flake nobody recorded": func(r *VerificationRecord) {
			r.GitHub.FlakeRerunPassed = true
		},
		"a job hidden from the top level": func(r *VerificationRecord) {
			other, _ := json.Marshal(map[string]any{"name": "lint", "databaseId": 102, "status": "completed", "conclusion": "failure", "result": "failed"})
			r.GitHub.Runs[0].Jobs = append(r.GitHub.Runs[0].Jobs, other)
		},
		"a claimed os":           func(r *VerificationRecord) { r.OS = "linux" },
		"a claimed arch":         func(r *VerificationRecord) { r.Arch = "amd64" },
		"a malformed repository": func(r *VerificationRecord) { r.Repository = "not/a/repo" },
		"a short run head":       func(r *VerificationRecord) { r.GitHub.RunHead = "abc123" },
		"a bad workflow path":    func(r *VerificationRecord) { r.GitHub.WorkflowPath = "ci.yml" },
		"a bad workflow digest":  func(r *VerificationRecord) { r.GitHub.WorkflowDigest = "md5:ff" },
		"an unknown reusedFrom verifier": func(r *VerificationRecord) {
			r.GitHub.ReusedFrom = &GitHubReusedFrom{Digest: "sha256:" + strings.Repeat("b", 64), Head: r.GitHub.RunHead, VerifierKind: "local"}
		},
		"a different tree":      func(r *VerificationRecord) { r.TreeHash = "other" },
		"a different base":      func(r *VerificationRecord) { r.BaseCommit = strings.Repeat("7", 40) },
		"a different ci digest": func(r *VerificationRecord) { r.CiDigest = "sha256:" + strings.Repeat("0", 64) },
		"a different dependency": func(r *VerificationRecord) {
			r.Dependencies = map[string]string{"go.sum": "sha256:" + strings.Repeat("0", 64), "web/package-lock.json": "sha256:" + strings.Repeat("6", 64)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			record := ghRemoteRecord(base, head, "tree-1", ghDeps(), pins)
			mutate(&record)
			if _, err := JudgeVerificationRecord(seal(record), want); err == nil {
				t.Errorf("%s accepted", name)
			}
		})
	}
	// an unknown runner is not the remote branch, whatever its kind says
	record := ghRemoteRecord(base, head, "tree-1", ghDeps(), pins)
	record.Runner = "local"
	if _, err := JudgeVerificationRecord(seal(record), want); err == nil {
		t.Error("a local runner with remote provenance selected the remote branch")
	}
	// the local checks still measure the host for a local record
	local := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: base, HeadCommit: head, TreeHash: "tree-1",
		CiDigest: "sha256:" + strings.Repeat("a", 64), Tools: pins, Pins: pins, PinMismatch: []string{}, Dependencies: ghDeps(),
		OS: "linux", Arch: "amd64", Result: "pass", Jobs: []json.RawMessage{}}
	raw = func() []byte {
		b, err := SealVerificationRecord(local)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()
	if _, err := JudgeVerificationRecord(raw, ghWant(base, ghDeps(), pins)); err == nil {
		t.Error("a local record skipped the host platform check")
	}
}
