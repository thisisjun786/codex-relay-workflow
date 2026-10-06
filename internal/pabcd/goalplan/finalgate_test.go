package goalplan

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// The 42 B tests of CXC v0.2.40 test/final-gate.test.ts:137-453, ported to Go. The oracle drives them through
// validateGoalplan; every fixture here is built so that only the final-gate checks can produce a reason, so the tests
// call finalGateReasons directly and assert on the joined reasons. Reason strings are the oracle's byte for byte.

var (
	finalGateHere      = SourceIdentity{Kind: source.KindResolved, CommitSha: "aaaaaaa", Dirty: false, CapturedAt: "2026-01-01T00:00:00.000Z"}
	finalGateElsewhere = SourceIdentity{Kind: source.KindResolved, CommitSha: "bbbbbbb", Dirty: false, CapturedAt: "2026-01-01T00:00:00.000Z"}
	finalGateNowhere   = SourceIdentity{Kind: source.KindUnavailable, CommitSha: "", Dirty: false, CapturedAt: "2026-01-01T00:00:00.000Z"}
)

func finalGateStr(s string) *string   { return &s }
func finalGateNum(v float64) *float64 { return &v }

// finalGateRoundFixture is round() (:39-50): a passing final_gate round bound to HERE.
func finalGateRoundFixture(over func(*ReviewRoundState)) ReviewRoundState {
	r := ReviewRoundState{
		RoundID: "r1", Purpose: PurposeFinalGate, PlanPath: "p.md", PlanSha256: strings.Repeat("c", 64),
		Status: ReviewApproved, Lane: ReviewLane{LaunchID: "r1-x", Verdict: VerdictPass, SourceIdentity: &finalGateHere},
		OpenedAt: "2026-01-01T00:00:00.000Z",
	}
	if over != nil {
		over(&r)
	}
	return r
}

// finalGateGateFixture is gate() (:52-63): an approved gate bound to HERE with a test receipt.
func finalGateGateFixture(over func(*FinalGateState)) FinalGateState {
	g := FinalGateState{
		Status: GateApproved, ReviewRoundID: finalGateStr("r1"), SourceIdentity: &finalGateHere,
		TestReceiptPath: finalGateStr(".codexclaw/evidence/test.json"), Verdict: VerdictPass,
		QaRequired: false, UpdatedAt: "2026-01-01T00:00:00.000Z",
	}
	if over != nil {
		over(&g)
	}
	return g
}

// finalGatePlanFixture is plan() (:66-78): a v1 plan whose v1 checks all pass, pinned to schemaVersion 1.
func finalGatePlanFixture(over func(*Goalplan)) *Goalplan {
	p := BuildGoalplan(NewGoalplanInput{Objective: "final gate fixture", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	p.SchemaVersion = finalGateNum(1)
	p.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{"c-1"}}}
	p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("done"), Status: CriterionMet, Surface: SurfaceLogic}}
	if over != nil {
		over(p)
	}
	return p
}

// finalGateReceipt is one readReceipt fixture: a bare identity, an identity with a manifest, or an error.
type finalGateReceipt struct {
	identity SourceIdentity
	manifest []gate.ArtifactDigest
	err      string
}

func finalGateReceiptIdentity(id SourceIdentity) finalGateReceipt {
	return finalGateReceipt{identity: id}
}

func finalGateReceiptManifest(id SourceIdentity, m []gate.ArtifactDigest) finalGateReceipt {
	return finalGateReceipt{identity: id, manifest: m}
}

func finalGateReceiptError(msg string) finalGateReceipt { return finalGateReceipt{err: msg} }

// finalGateCtx is ctx() (:82-98).
func finalGateCtx(cwd string, current SourceIdentity, receipts map[string]finalGateReceipt) *GoalplanValidationCtx {
	return &GoalplanValidationCtx{
		Cwd:                   cwd,
		CaptureSourceIdentity: func(string) SourceIdentity { return current },
		CompareSource: func(a, b SourceIdentity) source.Comparison {
			if a.Kind == source.KindUnavailable || b.Kind == source.KindUnavailable {
				return source.Comparison{Kind: source.ComparisonUnavailable, Reason: "no git"}
			}
			if a.CommitSha == b.CommitSha {
				return source.Comparison{Kind: source.ComparisonSame}
			}
			return source.Comparison{Kind: source.ComparisonDifferent, Detail: a.CommitSha + " vs " + b.CommitSha}
		},
		ReadReceipt: func(path string, kind gate.ReceiptKind) (GoalplanReceiptEvidence, error) {
			hit, ok := receipts[string(kind)+":"+path]
			if !ok {
				hit, ok = receipts[path]
			}
			if !ok {
				return GoalplanReceiptEvidence{SourceIdentity: finalGateHere}, nil
			}
			if hit.err != "" {
				return GoalplanReceiptEvidence{}, errors.New(hit.err)
			}
			return GoalplanReceiptEvidence{SourceIdentity: hit.identity, ArtifactManifest: hit.manifest}, nil
		},
	}
}

// finalGateJoined is reasons() (:104-106).
func finalGateJoined(p *Goalplan, ctx *GoalplanValidationCtx) string {
	return strings.Join(finalGateReasons(p, ctx), " | ")
}

func finalGateDesktopCriteria(ids []string, presented bool) []GoalplanCriterion {
	out := make([]GoalplanCriterion, 0, len(ids))
	for _, id := range ids {
		c := GoalplanCriterion{ID: id, Scenario: "D-PK-" + id, ExpectedEvidence: "bundle evidence",
			CapturedEvidence: finalGateStr("recorded"), Status: CriterionMet, Surface: SurfaceDesktop}
		if presented {
			c.Presented = PresentedNative
		}
		out = append(out, c)
	}
	return out
}

// finalGateDesktopPlan is desktopGate() (:115-123): a v2 desktop plan whose QA receipt is qa-receipt.json.
func finalGateDesktopPlan(ids []string, presented bool) *Goalplan {
	return finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		p.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: ids}}
		p.Criteria = finalGateDesktopCriteria(ids, presented)
		g := finalGateGateFixture(func(g *FinalGateState) { g.QaRequired = true; g.QaReceiptPath = finalGateStr("qa-receipt.json") })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
}

const finalGateDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// finalGateManifest is manifest() (:125-131).
func finalGateManifest(ids []string) []gate.ArtifactDigest {
	return []gate.ArtifactDigest{
		{Path: "D-PK-01/verdict.json", SHA256: finalGateDigest, Kind: gate.ArtifactVerdict, CriterionIDs: ids},
		{Path: "D-PK-01/artifact-identity.json", SHA256: finalGateDigest, Kind: gate.ArtifactIdentity, CriterionIDs: ids},
	}
}

// finalGateQACtx is qaCtx() (:133-135).
func finalGateQACtx(dir string, receipt finalGateReceipt) *GoalplanValidationCtx {
	return finalGateCtx(dir, finalGateHere, map[string]finalGateReceipt{"qa:qa-receipt.json": receipt})
}

// finalGatePlanPath is join(goalplanDir(dir, slug), "goalplan.json").
func finalGatePlanPath(dir, slug string) string {
	return filepath.Join(dir, crwdir.DirName, GoalplansSubdir, slug, GoalplanFile)
}

// finalGateMutate reads the written plan, applies mutate and writes it back, as the oracle's round-trip tests do.
func finalGateMutate(t *testing.T, dir, slug string, mutate func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(finalGatePlanPath(dir, slug))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalGatePlanPath(dir, slug), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// finalGateWriteMarker creates the plan directory and the schema-v2 marker, as the oracle's mkdirSync + writeFileSync do.
func finalGateWriteMarker(t *testing.T, dir, slug string) {
	t.Helper()
	marker, err := SchemaMarkerPath(dir, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("promoted"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFinalGateV1FinishedPlanStillPasses(t *testing.T) {
	if got := finalGateReasons(finalGatePlanFixture(nil), nil); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateV1UnknownSurfaceIsNotAV1Problem(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet}}
	})
	if got := finalGateReasons(p, nil); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateV2WithoutCtxRefuses(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	got := finalGateReasons(p, nil)
	if len(got) == 0 {
		t.Fatal("want a refusal, got none")
	}
	if !strings.Contains(strings.Join(got, " "), "without a validation context") {
		t.Fatalf("reasons = %#v", got)
	}
}

func TestFinalGateV2InOrderPasses(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if got := finalGateReasons(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateV2WithoutFinalGateIsASchemaViolation(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) { p.SchemaVersion = finalGateNum(2) })
	why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil))
	if !strings.Contains(why, "requires an approved finalGate") {
		t.Fatalf("reasons = %q", why)
	}
	if strings.Contains(why, "--lane") {
		t.Fatalf("the phantom flag came back: %q", why)
	}
}

func TestFinalGateV2RejectsGateNotApproved(t *testing.T) {
	for _, status := range []FinalGateStatus{GatePending, GateInFlight, GateInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			p := finalGatePlanFixture(func(p *Goalplan) {
				p.SchemaVersion = finalGateNum(2)
				g := finalGateGateFixture(func(g *FinalGateState) { g.Status = status })
				p.FinalGate = &g
				p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
			})
			if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "final gate is "+string(status)) {
				t.Fatalf("reasons = %q", why)
			}
		})
	}
}

func TestFinalGateV2RejectsCriterionWithoutSurface(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet}}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "no valid surface") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2UnknownSurfaceAfterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if err := WriteGoalplan(dir, p); err != nil {
		t.Fatal(err)
	}
	finalGateMutate(t, dir, p.Slug, func(m map[string]any) {
		m["criteria"].([]any)[0].(map[string]any)["surface"] = "api"
	})
	back := ReadGoalplan(dir, p.Slug)
	if back == nil {
		t.Fatal("read refused the plan")
	}
	if back.Criteria[0].Surface != "" {
		t.Fatalf("revive normalized an unknown surface to %q", back.Criteria[0].Surface)
	}
	if why := finalGateJoined(back, finalGateCtx(dir, finalGateHere, nil)); !strings.Contains(why, "no valid surface") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsPlanAuditRound(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) { r.Purpose = PurposePlanAudit })}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "plan audit cannot stand in") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsRoundNotApproved(t *testing.T) {
	for _, status := range []ReviewRoundStatus{ReviewChangesRequested, ReviewInconclusive, ReviewInFlight} {
		t.Run(string(status), func(t *testing.T) {
			p := finalGatePlanFixture(func(p *Goalplan) {
				p.SchemaVersion = finalGateNum(2)
				g := finalGateGateFixture(nil)
				p.FinalGate = &g
				p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) { r.Status = status })}
			})
			if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "is "+string(status)+", not approved") {
				t.Fatalf("reasons = %q", why)
			}
		})
	}
}

func TestFinalGateV2RejectsNoVerdictAndDisagreement(t *testing.T) {
	noVerdict := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) {
			r.Lane = ReviewLane{LaunchID: "x", SourceIdentity: &finalGateHere}
		})}
	})
	if why := finalGateJoined(noVerdict, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "recorded no verdict") {
		t.Fatalf("reasons = %q", why)
	}

	mismatch := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.Verdict = VerdictPass })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) {
			r.Lane = ReviewLane{LaunchID: "x", Verdict: VerdictNearPass, SourceIdentity: &finalGateHere}
		})}
	})
	if why := finalGateJoined(mismatch, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "but the final gate says") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2AcceptsMatchingNearPass(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.Verdict = VerdictNearPass })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) {
			r.Lane = ReviewLane{LaunchID: "x", Verdict: VerdictNearPass, SourceIdentity: &finalGateHere}
		})}
	})
	if got := finalGateReasons(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateV2RejectsMissingOrGhostReviewRoundID(t *testing.T) {
	none := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.ReviewRoundID = nil })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(none, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "no reviewRoundId") {
		t.Fatalf("reasons = %q", why)
	}

	ghost := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.ReviewRoundID = finalGateStr("r9") })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(ghost, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "not in the plan") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsGateWithoutSourceIdentity(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.SourceIdentity = nil })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "must record the tree it approved") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsApprovalAgainstDifferentTree(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.SourceIdentity = &finalGateElsewhere })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "final gate describes a different source") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsReviewerOnDifferentTree(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(func(r *ReviewRoundState) {
			r.Lane = ReviewLane{LaunchID: "x", Verdict: VerdictPass, SourceIdentity: &finalGateElsewhere}
		})}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "the reviewer describes a different source") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsReceiptOnDifferentTree(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	ctx := finalGateCtx(dir, finalGateHere, map[string]finalGateReceipt{"test:.codexclaw/evidence/test.json": finalGateReceiptIdentity(finalGateElsewhere)})
	if why := finalGateJoined(p, ctx); !strings.Contains(why, "test receipt describes a different source") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2ReportsRefusedReceipt(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	ctx := finalGateCtx(dir, finalGateHere, map[string]finalGateReceipt{"test:.codexclaw/evidence/test.json": finalGateReceiptError("kind mismatch")})
	if why := finalGateJoined(p, ctx); !strings.Contains(why, "test receipt is not usable") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2RejectsMissingTestReceiptPath(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.TestReceiptPath = nil })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "test receipt path is missing") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateWebCriterionDemandsQAReceiptAfterCursorGone(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		p.ActiveWorkPhaseID = nil
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: SurfaceWeb}}
		g := finalGateGateFixture(func(g *FinalGateState) { g.QaRequired = true; g.QaReceiptPath = nil })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "QA receipt path is missing") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateLogicOnlyPlanNeedsNoQAReceipt(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if got := finalGateReasons(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateDesktopRejectsLegacyQAReceipt(t *testing.T) {
	why := finalGateJoined(finalGateDesktopPlan([]string{"c-3"}, false), finalGateQACtx(t.TempDir(), finalGateReceiptIdentity(finalGateHere)))
	if !strings.Contains(why, "artifact-identity.json entry for desktop criterion c-3") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateDesktopRejectsVerdictOnlyManifest(t *testing.T) {
	receipt := finalGateReceiptManifest(finalGateHere, []gate.ArtifactDigest{finalGateManifest([]string{"c-3"})[0]})
	why := finalGateJoined(finalGateDesktopPlan([]string{"c-3"}, false), finalGateQACtx(t.TempDir(), receipt))
	if !strings.Contains(why, "artifact-identity.json entry for desktop criterion c-3") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateDesktopAcceptsMatchingIdentityEntry(t *testing.T) {
	receipt := finalGateReceiptManifest(finalGateHere, finalGateManifest([]string{"c-3"}))
	got := finalGateReasons(finalGateDesktopPlan([]string{"c-3"}, false), finalGateQACtx(t.TempDir(), receipt))
	if len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGatePresentedNativeDesktopNeedsNoManifest(t *testing.T) {
	got := finalGateReasons(finalGateDesktopPlan([]string{"c-3"}, true), finalGateQACtx(t.TempDir(), finalGateReceiptIdentity(finalGateHere)))
	if len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateChangedManifestFailsBeforeDesktopGate(t *testing.T) {
	why := finalGateJoined(finalGateDesktopPlan([]string{"c-3"}, false), finalGateQACtx(t.TempDir(), finalGateReceiptError("artifactManifest digest mismatch")))
	if !strings.Contains(why, "QA receipt is not usable: artifactManifest digest mismatch") {
		t.Fatalf("reasons = %q", why)
	}
	if strings.Contains(why, "no artifact-identity.json entry") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateUnrelatedIdentityCannotSatisfyAnotherCriterion(t *testing.T) {
	receipt := finalGateReceiptManifest(finalGateHere, finalGateManifest([]string{"c-3"}))
	why := finalGateJoined(finalGateDesktopPlan([]string{"c-3", "c-4"}, false), finalGateQACtx(t.TempDir(), receipt))
	if !strings.Contains(why, "desktop criterion c-4") {
		t.Fatalf("reasons = %q", why)
	}
	if strings.Contains(why, "desktop criterion c-3") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateEveryDesktopCriterionCoveredByOneIdentity(t *testing.T) {
	receipt := finalGateReceiptManifest(finalGateHere, finalGateManifest([]string{"c-3", "c-4"}))
	got := finalGateReasons(finalGateDesktopPlan([]string{"c-3", "c-4"}, false), finalGateQACtx(t.TempDir(), receipt))
	if len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateV1DesktopPlanDoesNotActivateManifestRequirement(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.Criteria = finalGateDesktopCriteria([]string{"c-3"}, false)
		p.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{"c-3"}}}
	})
	if got := finalGateReasons(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateWebCriterionAfterGateOpenedForcesReopen(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: SurfaceWeb}}
		g := finalGateGateFixture(func(g *FinalGateState) { g.QaRequired = false })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateHere, nil)); !strings.Contains(why, "criteria changed after the gate opened") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV2CannotBeCertifiedWithoutGit(t *testing.T) {
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) { g.SourceIdentity = &finalGateNowhere })
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, finalGateCtx(t.TempDir(), finalGateNowhere, nil)); !strings.Contains(why, "cannot be certified without git; use the v1 flow") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateV1UntouchedWhenGitUnavailable(t *testing.T) {
	if got := finalGateReasons(finalGatePlanFixture(nil), finalGateCtx(t.TempDir(), finalGateNowhere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateCaptureFailureBecomesAReason(t *testing.T) {
	dir := t.TempDir()
	broken := *finalGateCtx(dir, finalGateHere, nil)
	broken.CaptureSourceIdentity = func(string) SourceIdentity { panic(errors.New("git exploded")) }
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if why := finalGateJoined(p, &broken); !strings.Contains(why, "could not capture the current source identity: git exploded") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateMarkerPromotesAndRefusesDowngrade(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	finalGateWriteMarker(t, dir, p.Slug)
	if why := finalGateJoined(p, finalGateCtx(dir, finalGateHere, nil)); !strings.Contains(why, "restore \"schemaVersion\": 2") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGateMarkerAndPlanAgreeingAtV2(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	finalGateWriteMarker(t, dir, p.Slug)
	if got := finalGateReasons(p, finalGateCtx(dir, finalGateHere, nil)); len(got) != 0 {
		t.Fatalf("reasons = %#v, want none", got)
	}
}

func TestFinalGateNoMarkerAndNoSchemaVersionIsLegacy(t *testing.T) {
	if got := EffectiveSchemaVersion(finalGatePlanFixture(nil), false); got != 1 {
		t.Fatalf("EffectiveSchemaVersion = %v, want 1", got)
	}
	if got := EffectiveSchemaVersion(finalGatePlanFixture(nil), true); got != 2 {
		t.Fatalf("EffectiveSchemaVersion = %v, want 2", got)
	}
	if got := EffectiveSchemaVersion(finalGatePlanFixture(func(p *Goalplan) { p.SchemaVersion = finalGateNum(3) }), true); got != 3 {
		t.Fatalf("EffectiveSchemaVersion = %v, want 3", got)
	}
}

func TestFinalGateComputeQaRequiredScansWholePlan(t *testing.T) {
	if ComputeQaRequired(finalGatePlanFixture(nil)) {
		t.Fatal("a logic-only plan needs no QA")
	}
	web := finalGatePlanFixture(func(p *Goalplan) {
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: SurfaceTUI}}
	})
	if !ComputeQaRequired(web) {
		t.Fatal("a tui plan needs QA")
	}
}

func TestFinalGateComputeQaRequiredExactlyWebTUIAndDesktop(t *testing.T) {
	table := []struct {
		surface CriterionSurface
		want    bool
	}{
		{SurfaceLogic, false}, {SurfaceWeb, true}, {SurfaceTUI, true}, {SurfaceDesktop, true}, {CriterionSurface("api"), false},
	}
	for _, c := range table {
		p := finalGatePlanFixture(func(p *Goalplan) {
			p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: c.surface}}
		})
		if got := ComputeQaRequired(p); got != c.want {
			t.Fatalf("surface %q: ComputeQaRequired = %v, want %v", c.surface, got, c.want)
		}
	}
}

func TestFinalGateDesktopSurvivesRoundTripUnknownSurfaceDropped(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: SurfaceDesktop}}
	})
	if err := WriteGoalplan(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := ReadGoalplan(dir, p.Slug).Criteria[0].Surface; got != SurfaceDesktop {
		t.Fatalf("surface = %q, want desktop", got)
	}

	unknown := finalGatePlanFixture(func(p *Goalplan) {
		p.Slug = "unknown-surface"
		p.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "e", CapturedEvidence: finalGateStr("d"), Status: CriterionMet, Surface: CriterionSurface("native")}}
	})
	if err := WriteGoalplan(dir, unknown); err != nil {
		t.Fatal(err)
	}
	back := ReadGoalplan(dir, "unknown-surface")
	if back.Criteria[0].Surface != "" {
		t.Fatalf("surface = %q, want empty", back.Criteria[0].Surface)
	}
	v2 := *back
	v2.SchemaVersion = finalGateNum(2)
	g := finalGateGateFixture(nil)
	v2.FinalGate = &g
	v2.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	if why := finalGateJoined(&v2, finalGateCtx(dir, finalGateHere, nil)); !strings.Contains(why, "no valid surface (\"logic\" | \"web\" | \"tui\" | \"desktop\")") {
		t.Fatalf("reasons = %q", why)
	}
}

func TestFinalGatePresentedNativeSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.Criteria = finalGateDesktopCriteria([]string{"c-3"}, true)
		p.WorkPhases = []GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{"c-3"}}}
	})
	if err := WriteGoalplan(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := ReadGoalplan(dir, p.Slug).Criteria[0].Presented; got != PresentedNative {
		t.Fatalf("presented = %q, want native", got)
	}
	finalGateMutate(t, dir, p.Slug, func(m map[string]any) {
		m["criteria"].([]any)[0].(map[string]any)["presented"] = "web"
	})
	if got := ReadGoalplan(dir, p.Slug).Criteria[0].Presented; got != "" {
		t.Fatalf("presented = %q, want empty", got)
	}
}

func TestFinalGateSchemaVersionAndSurfaceSurviveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(func(g *FinalGateState) {
			g.QaRequired = true
			g.QaReceiptPath = finalGateStr(".codexclaw/evidence/qa.json")
		})
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if err := WriteGoalplan(dir, p); err != nil {
		t.Fatal(err)
	}
	back := ReadGoalplan(dir, p.Slug)
	if back.SchemaVersion == nil || *back.SchemaVersion != 2 {
		t.Fatalf("schemaVersion = %v, want 2", back.SchemaVersion)
	}
	if back.FinalGate == nil || !back.FinalGate.QaRequired {
		t.Fatalf("qaRequired = %#v, want true", back.FinalGate)
	}
	if back.FinalGate.QaReceiptPath == nil || *back.FinalGate.QaReceiptPath != ".codexclaw/evidence/qa.json" {
		t.Fatalf("qaReceiptPath = %#v", back.FinalGate.QaReceiptPath)
	}
	if back.FinalGate.SourceIdentity == nil || back.FinalGate.SourceIdentity.CommitSha != "aaaaaaa" {
		t.Fatalf("sourceIdentity = %#v", back.FinalGate.SourceIdentity)
	}
	if back.Criteria[0].Surface != SurfaceLogic {
		t.Fatalf("surface = %q, want logic", back.Criteria[0].Surface)
	}
}

func TestFinalGateMissingQaRequiredIsDroppedNotHalfTrusted(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})
	if err := WriteGoalplan(dir, p); err != nil {
		t.Fatal(err)
	}
	finalGateMutate(t, dir, p.Slug, func(m map[string]any) {
		delete(m["finalGate"].(map[string]any), "qaRequired")
	})
	back := ReadGoalplan(dir, p.Slug)
	if back.FinalGate != nil {
		t.Fatalf("finalGate = %#v, want nil", back.FinalGate)
	}
	if why := finalGateJoined(back, finalGateCtx(dir, finalGateHere, nil)); !strings.Contains(why, "requires an approved finalGate") {
		t.Fatalf("reasons = %q", why)
	}
}

// A callback that panics with an empty value is still a failed read: the oracle takes its catch arm on any throw, so empty
// panic text must not read as a success (Devin review finding on this pull request).
func TestFinalGateEmptyPanicIsStillAFailure(t *testing.T) {
	dir := t.TempDir()
	p := finalGatePlanFixture(func(p *Goalplan) {
		p.SchemaVersion = finalGateNum(2)
		g := finalGateGateFixture(nil)
		p.FinalGate = &g
		p.ReviewRounds = []ReviewRoundState{finalGateRoundFixture(nil)}
	})

	captureBroken := *finalGateCtx(dir, finalGateHere, nil)
	captureBroken.CaptureSourceIdentity = func(string) SourceIdentity { panic("") }
	if got := finalGateReasons(p, &captureBroken); len(got) != 1 || !strings.HasPrefix(got[0], "could not capture the current source identity: ") {
		t.Fatalf("reasons = %#v", got)
	}

	readBroken := *finalGateCtx(dir, finalGateHere, nil)
	readBroken.ReadReceipt = func(string, gate.ReceiptKind) (GoalplanReceiptEvidence, error) { panic("") }
	if got := finalGateReasons(p, &readBroken); len(got) != 1 || !strings.HasPrefix(got[0], "the test receipt could not be read: ") {
		t.Fatalf("reasons = %#v", got)
	}
}
