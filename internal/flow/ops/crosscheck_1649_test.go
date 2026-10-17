package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// ccHistory adds the CrosscheckHistory seam (CRW-1649) to the stock stub relay: only relays that implement it supply a prior.
type ccHistory struct {
	Relay
	runs []DesignRun
	err  error
}

func (s *ccHistory) CrosscheckHistory(context.Context, string) ([]DesignRun, error) {
	return s.runs, s.err
}

// priorVerdict1649 is a recorded needs_changes verdict of a source run: one blocking finding the next run re-judges.
func priorVerdict1649(run, ms, plan, head string, round int) *CrosscheckVerdictRecord {
	return &CrosscheckVerdictRecord{
		Verdict: reviewadapter.VerdictNeedsChanges,
		Findings: []reviewadapter.Finding{{ID: "f_old", Severity: "P1", Impact: "criterion_unmet", File: "x.go", Line: 1,
			Text: "the prior finding text 1649", Kind: reviewadapter.KindEvidenceMissing,
			EvidenceQuestion: &reviewadapter.EvidenceQuestion{Question: "where?", FilesNeeded: []string{}}}},
		Carried: []string{}, FollowUps: []string{}, GuideRev: CrosscheckGuideRev,
		EngineFamily: "model", EngineFamilies: []string{"model"}, Model: "model-a",
		Round: round, Milestone: ms, Plan: plan, Head: head, Base: "base000", Run: run,
		AuthorFamilies: []string{}, Authors: []CrosscheckAuthor{}, Receipts: []CrosscheckReceipt{},
		Passes: []CrosscheckPass{{Pass: "A", Attempt: 1, State: "ok", Verdict: reviewadapter.VerdictNeedsChanges}},
	}
}

// pendingRun1649 is the journal of a run that stopped crosscheck_pending: the recorded crosscheck row followed by the
// escalation carrying its V as evidence.crosscheck.
func pendingRun1649(run string, v *CrosscheckVerdictRecord) DesignRun {
	out := CrosscheckOutcome{State: CrosscheckStateVerdict, Verdict: v, Questions: []CrosscheckQuestion{}}
	raw, _ := json.Marshal(out)
	ccRow := engine.Event{Seq: 1, RunID: run, Kind: engine.KindCall, CallID: "c_cc", Op: OpCrosscheck, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": v.Milestone, "state": CrosscheckStateVerdict, "result": string(raw)}}
	esc := engine.Event{Seq: 2, RunID: run, Kind: engine.KindFlow, Event: engine.EvEscalation,
		Data: map[string]any{"reason": "crosscheck_pending", "evidence": map[string]any{"crosscheck": v}}}
	return DesignRun{Run: run, Events: []engine.Event{ccRow, esc}}
}

func TestCrosscheck1649PriorOfPendingRunAccepted(t *testing.T) {
	for _, round := range []int{1, 2} {
		t.Run(map[int]string{1: "round1", 2: "round2"}[round], func(t *testing.T) {
			w := newCCWorld(t)
			v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", round)
			w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{pendingRun1649("run-0", v)}}
			out, row := w.crosscheck(ccHead, nil)
			if out.State != CrosscheckStateVerdict || out.Verdict == nil {
				t.Fatalf("outcome %+v", out)
			}
			if out.Prior == nil || out.Prior.Run != "run-0" {
				t.Fatalf("prior %+v", out.Prior)
			}
			if out.Verdict.PriorRun != "run-0" || out.Verdict.PriorHead != "head-old" || out.Verdict.PriorDigest == "" {
				t.Fatalf("V does not identify its prior: %+v", out.Verdict)
			}
			if row.Data["prior"] == nil {
				t.Fatalf("the crosscheck row does not record the prior")
			}
			if n := len(w.checker.requests()); n != 2 {
				t.Fatalf("%d passes ran", n)
			}
			for _, c := range w.checker.requests() {
				if !strings.Contains(c.Request.Prompt, "PRIOR FINDINGS") || !strings.Contains(c.Request.Prompt, "the prior finding text 1649") {
					t.Errorf("pass %s prompt lacks the prior findings section", c.Pass)
				}
			}
		})
	}
}

func TestCrosscheck1649ForgedPriorRefused(t *testing.T) {
	t.Run("supplied equals the recorded verdict", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", 1)
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{pendingRun1649("run-0", v)}}
		out, _ := w.crosscheck(ccHead, map[string]any{"prior": *v})
		if out.State != CrosscheckStateVerdict || out.Prior == nil || out.Prior.Run != "run-0" {
			t.Fatalf("the recorded prior supplied explicitly refused: %+v", out)
		}
	})
	t.Run("escalation naming another milestone is authenticated", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", 1)
		src := pendingRun1649("run-0", v)
		forged := *v
		forged.Milestone = "M2"
		src.Events[1].Data["evidence"] = map[string]any{"crosscheck": &forged}
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{src}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid {
			t.Fatalf("an escalation forged to another milestone passed silently: %+v", out)
		}
		if n := len(w.checker.requests()); n != 0 {
			t.Fatalf("%d passes ran on a refused prior", n)
		}
	})
	t.Run("supplied differs from recorded", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", 1)
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{pendingRun1649("run-0", v)}}
		forged := *v
		forged.Findings = append([]reviewadapter.Finding(nil), v.Findings...)
		forged.Findings[0].Text = "a different finding"
		out, _ := w.crosscheck(ccHead, map[string]any{"prior": forged})
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid {
			t.Fatalf("forged prior accepted: %+v", out)
		}
		if n := len(w.checker.requests()); n != 0 {
			t.Fatalf("%d passes ran on a refused prior", n)
		}
	})
	t.Run("supplied with no history", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-1", "M1", "plan-1", "head-old", 1)
		w.lane.d.Relay = &ccHistory{Relay: w.relay}
		out, _ := w.crosscheck(ccHead, map[string]any{"prior": v})
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid {
			t.Fatalf("prior without recorded pending accepted: %+v", out)
		}
		if n := len(w.checker.requests()); n != 0 {
			t.Fatalf("%d passes ran on a refused prior", n)
		}
	})
	t.Run("escalation evidence not the recorded verdict", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", 1)
		src := pendingRun1649("run-0", v)
		forged := *v
		forged.Findings = append([]reviewadapter.Finding(nil), v.Findings...)
		forged.Findings[0].Text = "the escalation lies about the verdict"
		src.Events[1].Data["evidence"] = map[string]any{"crosscheck": &forged}
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{src}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid {
			t.Fatalf("forged escalation evidence accepted: %+v", out)
		}
		if n := len(w.checker.requests()); n != 0 {
			t.Fatalf("%d passes ran on a refused prior", n)
		}
	})
}

func TestCrosscheck1649PriorScopeAndReaders(t *testing.T) {
	t.Run("different milestone ignored", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M2", "plan-1", "head-old", 1)
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{pendingRun1649("run-0", v)}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateVerdict || out.Prior != nil || out.Verdict.PriorRun != "" {
			t.Fatalf("another milestone's pending verdict carried: %+v", out)
		}
	})
	t.Run("other repository ignored", func(t *testing.T) {
		w := newCCWorld(t)
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{{Run: "run-0", Reason: ReuseRepoMismatch, Detail: "the run's repository is /other"}}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateVerdict || out.Prior != nil {
			t.Fatalf("another repository's run carried: %+v", out)
		}
	})
	t.Run("older pending behind newer nonpending ignored", func(t *testing.T) {
		w := newCCWorld(t)
		v := priorVerdict1649("run-0", "M1", "plan-1", "head-old", 1)
		newer := DesignRun{Run: "run-2", Events: []engine.Event{
			{Seq: 1, RunID: "run-2", Kind: engine.KindFlow, Event: engine.EvEscalation, Data: map[string]any{"reason": "unit_check_failed"}},
		}}
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{newer, pendingRun1649("run-0", v)}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateVerdict || out.Prior != nil {
			t.Fatalf("a pending run behind a newer nonpending one was used: %+v", out)
		}
	})
	t.Run("unreadable immediate prior refused", func(t *testing.T) {
		w := newCCWorld(t)
		w.lane.d.Relay = &ccHistory{Relay: w.relay, runs: []DesignRun{{Run: "run-0", Reason: ReuseSourceUnreadable, Detail: "journal corrupt"}}}
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid {
			t.Fatalf("unreadable prior run passed over silently: %+v", out)
		}
		if n := len(w.checker.requests()); n != 0 {
			t.Fatalf("%d passes ran on a refused prior", n)
		}
	})
	t.Run("no reader no prior", func(t *testing.T) {
		w := newCCWorld(t)
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateVerdict || out.Prior != nil {
			t.Fatalf("a prior appeared without a history reader: %+v", out)
		}
	})
}

type ccIntegrationHistory1649 struct {
	*ccHistory
	sources []DesignRun
}

func (r *ccIntegrationHistory1649) IntegrationHistory(context.Context, string) ([]DesignRun, error) {
	return r.sources, nil
}

type ccReceiptChecker1649 func(context.Context, reviewadapter.Request) (reviewadapter.Result, error)

func (f ccReceiptChecker1649) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	return f(ctx, req)
}

// The accepted unit-check receipt is the input seam here: session assignment/acceptance has its own tests.
// Exercise real crosscheck materialization and consumption together with the pending prior and an inherited land receipt.
func TestCrosscheck1649CorrectionReceiptAndPriorReachBothPasses(t *testing.T) {
	dir := t.TempDir()
	check := func() error {
		cmd := exec.Command("test", "-s", "correction.txt")
		cmd.Dir = dir
		return cmd.Run()
	}
	err := check()
	var red *exec.ExitError
	if !errors.As(err, &red) || red.ExitCode() != 1 {
		t.Fatalf("RED must be an executed check returning 1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "correction.txt"), []byte("corrected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("GREEN: %v", err)
	}
	body := "actual-red-green-1649: test -s correction.txt; red_exit=1; green_exit=0\n" +
		"failing source: correction.txt absent; corrected overlay: correction.txt = corrected\\n\n"
	t.Log(body)
	cp, sum := receiptCopy(t, body)
	const landBody = "inherited land receipt 1649\n"
	landCopy, landSum := receiptCopy(t, landBody)
	for _, hasCorrection := range []bool{false, true} {
		t.Run(fmt.Sprintf("correction_receipt_%t", hasCorrection), func(t *testing.T) {
			w := newCCWorld(t, "A", "C")
			p := priorVerdict1649("run-pending", "M1", "plan-1", ccHead, 2)
			p.Findings[0].Text = "red-to-green receipt required"
			p.Findings[0].File = "correction.txt"
			p.Findings[0].ID = reviewadapter.FindingID(p.Findings[0].File, p.Findings[0].Text)
			pending := pendingRun1649("run-pending", p)
			source := engine.NewMemoryJournal()
			source.Append(srcRow(OpRelease, "source-release", map[string]any{
				"kind": "land", "released": true, "node": "A", "plan": "plan-1", "milestone": "M1",
				"head": ccHead, "tree": "tree-A", "base": "base-A", "slice_digest": "slice-A", "criteria_digest": "criteria-A",
			}))
			source.Append(srcRow(OpUnitCheck, "source-check", map[string]any{
				"role": "unit-check", "node": "A", "state": "ok", "commit": ccHead,
				"receipt": map[string]any{"state": "accepted", "sha256": landSum, "copy": landCopy},
			}))
			source.Append(srcRow(OpAccept, "source-accept", map[string]any{
				"node": "A", "plan": "plan-1", "head": ccHead, "acceptance_id": "accept-A", "reviewer_call": "source-check",
			}))
			history := &ccIntegrationHistory1649{
				ccHistory: &ccHistory{Relay: w.relay, runs: []DesignRun{pending}},
				sources:   []DesignRun{{Run: "source-run", Events: source.Events()}},
			}
			w.lane.d.Relay = history // no DesignHistory fallback: use the new integration-history interface
			// The landing proof is the input to receipt inheritance; land proof itself is tested in integration_reuse_1649_test.go.
			w.reuseRow(t, ReuseIntegratedResult{
				Reused: true, Reason: ReuseIntegratedReused, Plan: "plan-1", Milestone: "M1", Node: "A", Head: ccHead,
				AcceptanceID: "accept-A", SourceRun: "source-run", SourceAcceptCall: "source-accept",
				SourceKind: "land", SourceReleaseCall: "source-release",
			})
			if hasCorrection {
				w.receiptRow("C", map[string]any{"state": "accepted", "sha256": sum, "copy": cp})
			} else {
				w.receiptRow("C", map[string]any{"state": ReceiptNotAssigned})
			}
			// Only marshal the history data, not the embedded Relay implementation.
			before, _ := json.Marshal([]any{history.sources, history.runs})
			file := filepath.Join(w.landRoot, "crosscheck", "M1-r1-receipts.md")
			var mu sync.Mutex
			var calls []reviewadapter.Request
			w.lane.d.Crosscheck.Checker = ccReceiptChecker1649(func(_ context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
				mu.Lock()
				calls = append(calls, req)
				mu.Unlock()
				materialized, err := os.ReadFile(file)
				if err != nil {
					return reviewadapter.Result{}, fmt.Errorf("read the supplied receipts: %w", err)
				}
				hasPrior := strings.Contains(req.Prompt, p.Findings[0].Text)
				if !hasPrior || !strings.Contains(req.Prompt, file) || !strings.Contains(string(materialized), landBody) {
					t.Errorf("a pass did not get prior, receipt-file reference and inherited land receipt")
				}
				rawFindings, _ := json.Marshal(p.Findings)
				var currentFindings []reviewadapter.Finding
				if err := json.Unmarshal(rawFindings, &currentFindings); err != nil {
					return reviewadapter.Result{}, err
				}
				result := reviewadapter.Result{
					State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges,
					Findings:     currentFindings,
					EngineFamily: profileFamily(req.Profile), Model: req.Profile.Model, Effort: req.Profile.Effort,
					GuideRev: req.GuideRev,
				}
				if hasPrior && strings.Contains(string(materialized), body) {
					result.Verdict = reviewadapter.VerdictVerified
					result.Findings = []reviewadapter.Finding{}
				}
				return result, nil
			})
			out, _ := w.crosscheck(ccHead, nil)
			want := reviewadapter.VerdictNeedsChanges
			if hasCorrection {
				want = reviewadapter.VerdictVerified
			}
			if out.Verdict == nil || out.Verdict.Verdict != want || out.Prior == nil || out.Verdict.PriorRun != p.Run {
				t.Fatalf("prior/receipt judgment: %+v, want %s", out, want)
			}
			if len(calls) != 2 {
				t.Fatalf("passes = %d, want 2", len(calls))
			}
			if len(out.Verdict.Receipts) != 2 || out.Verdict.Receipts[0].Node != "A" || out.Verdict.Receipts[0].SHA256 != landSum {
				t.Fatalf("inherited land receipt: %+v", out.Verdict.Receipts)
			}
			if hasCorrection && (out.Verdict.Receipts[1].Node != "C" || out.Verdict.Receipts[1].SHA256 != sum) {
				t.Fatalf("correction receipt: %+v", out.Verdict.Receipts)
			}
			after, _ := json.Marshal([]any{history.sources, history.runs})
			if !bytes.Equal(before, after) {
				t.Fatal("a source journal changed")
			}
		})
	}
}
