package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateTransitionRoot is a temporary world for the transition tests: HOME, CODEX_HOME and CRW_HOME point
// inside it, so no run can reach the real Codex home (the packet's real-state rule).
func orchestrateTransitionRoot(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, t.TempDir())
	}
	return t.TempDir()
}

func orchestrateTransitionPut(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// orchestrateTransitionSession seeds the session file verbatim, as the oracle's writeState would.
func orchestrateTransitionSession(t *testing.T, cwd, id, body string) {
	t.Helper()
	orchestrateTransitionPut(t, state.StatePath(cwd, id), body)
}

// orchestrateTransitionSeedPlanUnit seeds the minimal valid plan unit plan-gate.ts accepts.
func orchestrateTransitionSeedPlanUnit(t *testing.T, cwd string) string {
	t.Helper()
	unit := "devlog/_plan/000000_test-unit"
	orchestrateTransitionPut(t, filepath.Join(cwd, filepath.FromSlash(unit), "000_plan.md"), "# 000 - test plan\n")
	return unit
}

// orchestrateTransitionTry drives the pair the harness will: the read side resolves the session, the
// transition half mutates it. A read answer (help, status, a parse error, a guard refusal) is returned as it is.
func orchestrateTransitionTry(t *testing.T, cwd string, argv ...string) (CliResult, error) {
	t.Helper()
	parsed := ParseOrchestrateCliArgs(argv, cwd)
	read, err := RunOrchestrateRead(parsed, ReadEnv{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.Result != nil {
		return *read.Result, nil
	}
	return RunOrchestrateTransition(*parsed.Args, read.SessionID)
}

func orchestrateTransitionRun(t *testing.T, cwd string, argv ...string) CliResult {
	t.Helper()
	got, err := orchestrateTransitionTry(t, cwd, argv...)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	return got
}

// orchestrateTransitionLedger reads the PABCD ledger rows, newest last.
func orchestrateTransitionLedger(t *testing.T, cwd string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "ledger.jsonl"))
	if err != nil {
		return nil
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("ledger row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// orchestrateTransitionRowLine is one raw ledger line: JSON.stringify fixes a row's key order at build time.
func orchestrateTransitionRowLine(t *testing.T, cwd string, index int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if index >= len(lines) {
		t.Fatalf("row %d of %d", index, len(lines))
	}
	return lines[index]
}

// orchestrateTransitionRepo is a one-commit git repository, so a capture resolves.
func orchestrateTransitionRepo(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	root := t.TempDir()
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": root,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
	} {
		t.Setenv(name, value)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	orchestrateTransitionPut(t, filepath.Join(root, "tracked.txt"), "x\n")
	git("init", "-q", "-b", "main", ".")
	git("add", "tracked.txt")
	git("commit", "-qm", "init")
	return root
}

// orchestrateTransitionReadyInterview is readyInterview() of orchestrate-cli.test.ts: every dimension maxed, no
// open contradiction, one recorded scan, so readState derives flags.interview=true.
const orchestrateTransitionReadyInterview = `{"roundId":1,"dimensions":{"goal":{"level":"max","known":["x"],"unknown":[],"confidence":1},"constraint":{"level":"max","known":["x"],"unknown":[],"confidence":1},"success":{"level":"max","known":["x"],"unknown":[],"confidence":1},"ontology":{"level":"max","known":["x"],"unknown":[],"confidence":1}},"contradictions":[],"assumptions":[],"autoResolveCount":0,"consecutiveAutoResolves":0,"scanRounds":1,"lastScanRoundId":1}`

// TestOrchestrateTransitionReset ports "status renders phase; reset clears to IDLE" (:442): a reset from every
// phase is the same cleared-IDLE write as the human path, and a reset from rest is a recognised no-op.
func TestOrchestrateTransitionReset(t *testing.T) {
	for _, from := range []string{"P", "A", "B", "C", "D"} {
		t.Run(from, func(t *testing.T) {
			cwd := orchestrateTransitionRoot(t)
			id := "reset-" + from
			orchestrateTransitionSession(t, cwd, id, `{"phase":"`+from+`","orchestrationActive":true,"lastInjectedPhase":"`+from+
				`","stopBlockPhase":"`+from+`","stopBlockCount":3}`)
			got := orchestrateTransitionRun(t, cwd, "reset", "--session", id)
			if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current="+from+" -> IDLE (session "+id+")") {
				t.Fatalf("reset: %+v", got)
			}
			after := state.ReadState(cwd, id)
			if after.Phase != state.PhaseIdle || after.OrchestrationActive || after.LastInjectedPhase != nil ||
				after.StopBlockPhase != nil || after.StopBlockCount != 0 {
				t.Fatalf("cleared state: %+v", after)
			}
			rows := orchestrateTransitionLedger(t, cwd)
			if len(rows) != 1 || rows[0]["reason"] != "reset" || rows[0]["from"] != from || rows[0]["to"] != "IDLE" {
				t.Fatalf("reset ledger: %+v", rows)
			}
			shape := `","sessionId":"reset-` + from + `","from":"` + from + `","to":"IDLE","reason":"reset"}`
			if line := orchestrateTransitionRowLine(t, cwd, 0); !strings.Contains(line, shape) {
				t.Fatalf("reset row shape: %s", line)
			}
		})
	}
	t.Run("already-idle", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		id := "reset-idle"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
		got := orchestrateTransitionRun(t, cwd, "reset", "--session", id)
		if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current=IDLE session="+id+"; already IDLE") {
			t.Fatalf("no-op reset: %+v", got)
		}
		if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
			t.Fatalf("a no-op reset wrote a ledger row: %+v", rows)
		}
	})
}

// TestOrchestrateTransitionSourceRootRefusal ports the SOURCE-ROOT guard (:571-573): an unresolvable source
// binding refuses before any phase or goalplan write.
func TestOrchestrateTransitionSourceRootRefusal(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "source-root"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE","boundSourceRoot":"`+filepath.Join(cwd, "missing-worktree")+`"}`)
	got := orchestrateTransitionRun(t, cwd, "I", "--session", id)
	if got.Code != 1 || !strings.HasPrefix(got.Output, "orchestrate I: SOURCE-ROOT: ") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle || len(orchestrateTransitionLedger(t, cwd)) != 0 {
		t.Fatal("the refusal wrote state")
	}
}

// TestOrchestrateTransitionEntryEdgeBoundSourceRefusal ports #133 (:586-591): a goalplan-bound cycle whose
// source identity cannot be resolved is refused on the entry edges only, and the same workspace without a
// bound slug still enters P.
func TestOrchestrateTransitionEntryEdgeBoundSourceRefusal(t *testing.T) {
	cwd := orchestrateTransitionRoot(t) // not a git repository
	bound := "bound-entry"
	orchestrateTransitionSession(t, cwd, bound, `{"phase":"IDLE","slug":"`+bound+`"}`)
	got := orchestrateTransitionRun(t, cwd, "P", "--session", bound)
	if got.Code != 1 || !strings.Contains(got.Output, "no resolvable git source identity") ||
		!strings.HasSuffix(got.Output, "Nothing was written.") {
		t.Fatalf("bound entry: %+v", got)
	}
	if state.ReadState(cwd, bound).Phase != state.PhaseIdle {
		t.Fatal("the refusal wrote state")
	}
	open := "open-entry"
	orchestrateTransitionSession(t, cwd, open, `{"phase":"IDLE"}`)
	if got := orchestrateTransitionRun(t, cwd, "P", "--session", open); got.Code != 0 {
		t.Fatalf("unbound entry: %+v", got)
	}
}

// TestOrchestrateTransitionPlanGate ports "PLAN-GATE" (:285) and the minted binding (:1073-1074): P>A is
// refused without on-disk plan docs and mints the unit and epoch the edge validated.
func TestOrchestrateTransitionPlanGate(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "plan-gate"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P"}`)
	bare := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited the plan"}`)
	if bare.Code != 1 || !strings.Contains(bare.Output, "planUnit") {
		t.Fatalf("bare: %+v", bare)
	}
	ghost := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited","planUnit":"devlog/_plan/000000_missing"}`)
	if ghost.Code != 1 || !strings.Contains(ghost.Output, "does not exist") {
		t.Fatalf("ghost: %+v", ghost)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseP {
		t.Fatal("a refused P>A wrote state")
	}
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	ok := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited","planUnit":"`+unit+`"}`)
	if ok.Code != 0 {
		t.Fatalf("seeded: %+v", ok)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseA || after.PlanUnit == nil || *after.PlanUnit != unit ||
		after.PlanEpoch == nil || !strings.HasPrefix(*after.PlanEpoch, "e-") {
		t.Fatalf("binding: %+v", after)
	}
	if row := orchestrateTransitionLedger(t, cwd); len(row) != 1 || row[0]["reason"] != "cli" || row[0]["to"] != "A" {
		t.Fatalf("ledger: %+v", row)
	}
}

// TestOrchestrateTransitionWorkPhaseBinding ports "260714 wp4" (:306) and the fail-open of an unreadable plan.
func TestOrchestrateTransitionWorkPhaseBinding(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "wp-bind"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "binding test"})
	plan.Slug = id
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	missing := orchestrateTransitionRun(t, cwd, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"built it"}`)
	if missing.Code != 1 || !strings.Contains(missing.Output, "workPhaseId") {
		t.Fatalf("missing: %+v", missing)
	}
	wrong := orchestrateTransitionRun(t, cwd, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"built it","workPhaseId":"wp2"}`)
	if wrong.Code != 1 || !strings.Contains(wrong.Output, "LOOP-UNIT-CHAIN-01") {
		t.Fatalf("wrong: %+v", wrong)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseB {
		t.Fatal("a refused binding wrote state")
	}
	ok := orchestrateTransitionRun(t, cwd, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"built it","workPhaseId":"wp1"}`)
	if ok.Code != 0 {
		t.Fatalf("bound: %+v", ok)
	}
	if got := state.ReadState(cwd, id); got.Phase != state.PhaseC {
		t.Fatalf("phase: %+v", got)
	}
	t.Run("fail-open", func(t *testing.T) {
		open := "wp-open"
		orchestrateTransitionSession(t, cwd, open, `{"phase":"B","slug":"`+open+`"}`)
		orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "goalplans", open, "goalplan.json"), "not json\n")
		if got := orchestrateTransitionRun(t, cwd, "C", "--session", open, "--attest", `{"from":"B","to":"C","did":"built it"}`); got.Code != 0 {
			t.Fatalf("an unreadable goalplan must not block: %+v", got)
		}
	})
}

// TestOrchestrateTransitionInterviewGate ports the G20 I>P cases (:594-723).
func TestOrchestrateTransitionInterviewGate(t *testing.T) {
	t.Run("refused-without-flag", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		id := "i-to-p-denied"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I"}`)
		got := orchestrateTransitionRun(t, cwd, "P", "--session", id)
		if got.Code != 1 || !strings.Contains(strings.ToLower(got.Output), "interview") {
			t.Fatalf("denied: %+v", got)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseI {
			t.Fatal("a refused I>P wrote state")
		}
	})
	t.Run("ready-uses-the-normal-path", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		id := "i-to-p-ready"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I","interview":`+orchestrateTransitionReadyInterview+`}`)
		got := orchestrateTransitionRun(t, cwd, "P", "--session", id)
		if got.Code != 0 {
			t.Fatalf("ready: %+v", got)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseP {
			t.Fatal("a ready interview did not advance")
		}
		rows := orchestrateTransitionLedger(t, cwd)
		if len(rows) != 1 || rows[0]["override"] != nil || rows[0]["reason"] != "cli" {
			t.Fatalf("ready ledger: %+v", rows)
		}
	})
	t.Run("override-accepted", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		id := "i-to-p-override"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I"}`)
		got := orchestrateTransitionRun(t, cwd, "P", "--session", id, "--attest",
			`{"from":"I","to":"P","did":"interview done","override":true}`)
		if got.Code != 0 || !strings.Contains(got.Output, "orchestrate P: I → P (agent override, session "+id+")") {
			t.Fatalf("override: %+v", got)
		}
		after := state.ReadState(cwd, id)
		if after.Phase != state.PhaseP || after.Interview != nil || !after.OrchestrationActive {
			t.Fatalf("override state: %+v", after)
		}
		rows := orchestrateTransitionLedger(t, cwd)
		if len(rows) != 1 || rows[0]["actor"] != "agent" || rows[0]["override"] != true {
			t.Fatalf("override ledger: %+v", rows)
		}
		evidence := rows[0]["scanEvidence"].(map[string]any)
		if evidence["scanRounds"] != float64(0) || evidence["highContradictionCount"] != float64(0) {
			t.Fatalf("scan evidence: %+v", evidence)
		}
		shape := `","from":"I","to":"P","reason":"cli","actor":"agent","override":true,` +
			`"scanEvidence":{"scanRounds":0,"highContradictionCount":0},"evidence":"interview done"}`
		if line := orchestrateTransitionRowLine(t, cwd, 0); !strings.Contains(line, shape) {
			t.Fatalf("override row shape: %s", line)
		}
	})
	t.Run("override-refusals", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		for _, c := range []struct{ name, attest, want string }{
			{"empty-did", `{"from":"I","to":"P","did":"","override":true}`, "placeholder"},
			{"placeholder-did", `{"from":"I","to":"P","did":"done","override":true}`, "placeholder"},
			{"wrong-from-to", `{"from":"P","to":"A","did":"interview complete with evidence","override":true}`, "from/to must be I/P"},
		} {
			id := "override-" + c.name
			orchestrateTransitionSession(t, cwd, id, `{"phase":"I"}`)
			got := orchestrateTransitionRun(t, cwd, "P", "--session", id, "--attest", c.attest)
			if got.Code != 1 || !strings.Contains(got.Output, c.want) {
				t.Fatalf("%s: %+v", c.name, got)
			}
			if state.ReadState(cwd, id).Phase != state.PhaseI {
				t.Fatalf("%s wrote state", c.name)
			}
		}
	})
	t.Run("advise-block", func(t *testing.T) {
		cwd := orchestrateTransitionRoot(t)
		id := "i-to-p-advise"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I"}`)
		got := orchestrateTransitionRun(t, cwd, "P", "--session", id, "--attest", `{"from":"I","to":"P","did":"interview done"}`)
		if got.Code != 1 || !strings.Contains(got.Output, "interview soft-gate: ") ||
			!strings.Contains(got.Output, "Pass override:true in --attest to proceed.") {
			t.Fatalf("advise: %+v", got)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseI {
			t.Fatal("the advise-block wrote state")
		}
	})
}

// TestOrchestrateTransitionIllegalEdge ports the illegal-edge refusal (:432).
func TestOrchestrateTransitionIllegalEdge(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "illegal-edge"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	got := orchestrateTransitionRun(t, cwd, "C", "--session", id)
	if got.Code != 1 || !strings.Contains(got.Output, "orchestrate C: current=IDLE session="+id+"; ") ||
		!strings.Contains(got.Output, "illegal transition IDLE->C") {
		t.Fatalf("illegal edge: %+v", got)
	}
}

// TestOrchestrateTransitionOrdinaryWrite ports the ordinary edge write (:1066-1131): state, ledger, the render
// ledger, and the architect pointer P alone carries.
func TestOrchestrateTransitionOrdinaryWrite(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "ordinary"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE","checkEpoch":"c-stale","planUnit":"stale","planEpoch":"e-stale"}`)
	renderPath := filepath.Join(cwd, ".crw", "render-observations.jsonl")
	orchestrateTransitionPut(t, renderPath, `{"kind":"artifact-modified"}`+"\n")
	got := orchestrateTransitionRun(t, cwd, "P", "--session", id)
	if got.Code != 0 || !strings.Contains(got.Output, "orchestrate P: current=IDLE -> P (IDLE → P, session "+id+")") ||
		!strings.HasSuffix(got.Output, "[formal P: architect proposal -> main executable plan -> same-architect reflection before A (crw-pabcd phase-plan)]") {
		t.Fatalf("IDLE>P: %+v", got)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseP || !after.OrchestrationActive || after.LastInjectedPhase == nil || *after.LastInjectedPhase != state.PhaseP ||
		after.StopBlockPhase != nil || after.StopBlockCount != 0 || after.PhaseEntrySource != nil ||
		after.PlanUnit != nil || after.PlanEpoch != nil || after.CheckEpoch != nil {
		t.Fatalf("IDLE>P state: %+v", after)
	}
	raw, err := os.ReadFile(renderPath)
	if err != nil || len(raw) != 0 {
		t.Fatalf("entering P must clear the render ledger: %q %v", raw, err)
	}
	rows := orchestrateTransitionLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "IDLE" || rows[0]["to"] != "P" || rows[0]["reason"] != "cli" || rows[0]["evidence"] != nil {
		t.Fatalf("ledger: %+v", rows)
	}
	if line := orchestrateTransitionRowLine(t, cwd, 0); !strings.Contains(line, `","from":"IDLE","to":"P","reason":"cli"}`) {
		t.Fatalf("IDLE>P row shape: %s", line)
	}
	rawState, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"phase\": \"P\"", "\"orchestrationActive\": true", "\"lastInjectedPhase\": \"P\"",
		"\"stopBlockPhase\": null", "\"stopBlockCount\": 0", "\"phaseEntrySource\": null",
		"\"planUnit\": null", "\"planEpoch\": null", "\"checkEpoch\": null"} {
		if !strings.Contains(string(rawState), want) {
			t.Fatalf("state bytes missing %q:\n%s", want, rawState)
		}
	}
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	toA := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited","planUnit":"`+unit+`"}`)
	if toA.Code != 0 || strings.Contains(toA.Output, "architect") {
		t.Fatalf("P>A: %+v", toA)
	}
	if rows := orchestrateTransitionLedger(t, cwd); rows[len(rows)-1]["evidence"] != "audited" {
		t.Fatalf("P>A evidence: %+v", rows)
	}
	if line := orchestrateTransitionRowLine(t, cwd, 1); !strings.Contains(line, `","from":"P","to":"A","reason":"cli","evidence":"audited"}`) {
		t.Fatalf("P>A row shape: %s", line)
	}
	rawState, err = os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"phase\": \"A\"", "\"planUnit\": \"devlog/_plan/000000_test-unit\"", "\"checkEpoch\": null"} {
		if !strings.Contains(string(rawState), want) {
			t.Fatalf("P>A state bytes missing %q:\n%s", want, rawState)
		}
	}
	if !strings.Contains(string(rawState), "\"planEpoch\": \"e-") {
		t.Fatalf("P>A state has no minted epoch:\n%s", rawState)
	}
}

// TestOrchestrateTransitionBuildEntrySnapshot ports "entering B snapshots the source, and leaving B clears it" (:1074).
func TestOrchestrateTransitionBuildEntrySnapshot(t *testing.T) {
	cwd := orchestrateTransitionRepo(t)
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, t.TempDir())
	}
	id := "snapshot"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"A"}`)
	toB := orchestrateTransitionRun(t, cwd, "B", "--session", id, "--attest",
		`{"from":"A","to":"B","did":"audited","auditOutput":"VERDICT: PASS","auditVerdict":"pass"}`)
	if toB.Code != 0 {
		t.Fatalf("A>B: %+v", toB)
	}
	atB := state.ReadState(cwd, id)
	if atB.Phase != state.PhaseB || atB.PhaseEntrySource == nil || atB.PhaseEntrySource.Kind != "resolved" {
		t.Fatalf("entering B must snapshot the source: %+v", atB)
	}
	if atB.BoundSourceRoot != nil {
		t.Fatalf("an unbound session pins no root: %+v", atB.BoundSourceRoot)
	}
	orchestrateTransitionPut(t, filepath.Join(cwd, "work.ts"), "export const y = 2;\n")
	toC := orchestrateTransitionRun(t, cwd, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"implemented the slice"}`)
	if toC.Code != 0 {
		t.Fatalf("B>C: %+v", toC)
	}
	atC := state.ReadState(cwd, id)
	if atC.PhaseEntrySource != nil {
		t.Fatalf("a snapshot must not outlive its phase: %+v", atC.PhaseEntrySource)
	}
	if atC.CheckEpoch == nil || !strings.HasPrefix(*atC.CheckEpoch, "c-") {
		t.Fatalf("entering C mints a check epoch: %+v", atC.CheckEpoch)
	}
}

// TestOrchestrateTransitionCheckEpoch ports the :1075-1077 rule directly.
func TestOrchestrateTransitionCheckEpoch(t *testing.T) {
	kept := "c-existing"
	for _, c := range []struct {
		name      string
		from, to  state.Phase
		current   *string
		want      *string
		wantMints bool
	}{
		{"entering-c", state.PhaseB, state.PhaseC, nil, nil, true},
		{"staying-in-c", state.PhaseC, state.PhaseC, &kept, &kept, false},
		{"leaving-c", state.PhaseC, state.PhaseB, &kept, nil, false},
		{"other-edge", state.PhaseP, state.PhaseA, &kept, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := orchestrateTransitionCheckEpoch(c.from, c.to, c.current)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case c.wantMints:
				if got == nil || !strings.HasPrefix(*got, "c-") {
					t.Fatalf("mint: %v", got)
				}
			case c.want == nil:
				if got != nil {
					t.Fatalf("want nil, got %v", *got)
				}
			default:
				if got == nil || *got != *c.want {
					t.Fatalf("want %q, got %v", *c.want, got)
				}
			}
		})
	}
}

// TestOrchestrateTransitionSupersedesStaleRounds ports the stale-round supersede (:2603) and its held-lock fail-open.
func TestOrchestrateTransitionSupersedesStaleRounds(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id, slug := "replan", "replan-plan"
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "replan housekeeping"})
	plan.Slug, plan.ActiveWorkPhaseID = slug, new("wp1")
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ReviewRounds = []goalplan.ReviewRoundState{{
		RoundID: "r1", Purpose: goalplan.PurposePlanAudit, PlanPath: unit, PlanSha256: strings.Repeat("a", 64),
		Status: goalplan.ReviewInFlight, Lane: goalplan.ReviewLane{LaunchID: "r1-launch"}, OpenedAt: "2026-08-28T00:00:00.000Z",
		OwnerSessionID: id, WorkPhaseID: "wp1", PlanUnit: unit, PlanEpoch: "e-old",
	}}
	plan.ActivePlanAuditRoundID = new("r1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P","slug":"`+slug+`"}`)
	attestation := `{"from":"P","to":"A","did":"audited the plan","planUnit":"` + unit + `","workPhaseId":"wp1"}`
	if got := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", attestation); got.Code != 0 {
		t.Fatalf("P>A: %+v", got)
	}
	saved := goalplan.ReadGoalplan(cwd, slug)
	if saved == nil || len(saved.ReviewRounds) != 1 || saved.ReviewRounds[0].Status != goalplan.ReviewInconclusive {
		t.Fatalf("stale round: %+v", saved)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"))
	if err != nil || !strings.Contains(string(raw), `"event":"review_round_superseded"`) ||
		!strings.Contains(string(raw), `"roundId":"r1"`) {
		t.Fatalf("goalplan ledger: %s %v", raw, err)
	}
	// A held lock leaves the edge to proceed and the round untouched (fail-open housekeeping).
	held := "replan-held"
	plan2 := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "held lock"})
	plan2.Slug, plan2.ActiveWorkPhaseID = held, new("wp1")
	plan2.WorkPhases, plan2.ReviewRounds = plan.WorkPhases, plan.ReviewRounds
	if err := goalplan.WriteGoalplan(cwd, plan2); err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, held, `{"phase":"P","slug":"`+held+`"}`)
	lock := filepath.Join(cwd, ".crw", "goalplans", held, ".goalplan.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionPut(t, filepath.Join(lock, "owner.json"), "{\"pid\":4242}\n")
	if got := orchestrateTransitionRun(t, cwd, "A", "--session", held, "--attest", `{"from":"P","to":"A","did":"audited the plan","planUnit":"`+unit+`","workPhaseId":"wp1"}`); got.Code != 0 {
		t.Fatalf("a held lock must not block: %+v", got)
	}
	if state.ReadState(cwd, held).Phase != state.PhaseA {
		t.Fatal("the held-lock P>A did not advance")
	}
}

// TestOrchestrateTransitionDCloseNotPorted pins the split boundary: this issue refuses the D close.
func TestOrchestrateTransitionDCloseNotPorted(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "d-close"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"C","checkEpoch":"c-1"}`)
	got, err := orchestrateTransitionTry(t, cwd, "D", "--session", id, "--attest",
		`{"from":"C","to":"D","did":"verified","checkOutput":"tests passed","exitCode":0}`)
	if err == nil || err.Error() != "orchestrate D close is not ported yet" {
		t.Fatalf("D close: %+v %v", got, err)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC || len(orchestrateTransitionLedger(t, cwd)) != 0 {
		t.Fatal("the unported D close wrote state")
	}
}

// TestOrchestrateTransitionRefusesALossyStateRewrite is the port's data-loss guard on the session write: the
// oracle publishes the state the reader rebuilt, so a stored lone surrogate becomes U+FFFD; the port refuses
// and writes nothing, leaving the file, both ledgers and the goalplan as they were.
func TestOrchestrateTransitionRefusesALossyStateRewrite(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "lossy-state"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P","unverifiedSubagents":[{"agentId":"a","recordedAt":"2026-01-01T00:00:00.000Z","receiptClaimed":"\ud800"}]}`)
	before, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	if got := orchestrateTransitionRun(t, cwd, "reset", "--session", id); got.Code != 1 || !strings.Contains(got.Output, "refusing to overwrite it") {
		t.Fatalf("reset: %+v", got)
	}
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	toA := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited","planUnit":"`+unit+`"}`)
	if toA.Code != 1 || !strings.Contains(toA.Output, "refusing to overwrite it") {
		t.Fatalf("P>A: %+v", toA)
	}
	after, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("the refused write changed the session file:\n%s", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused write appended a ledger row: %+v", rows)
	}
}

// TestOrchestrateTransitionLeavesRealHomesAlone is the packet's real-state rule: HOME, CODEX_HOME and CRW_HOME
// point into temporary directories, and the ambient listings of the host's ~/.codex and ~/.crw are compared
// before and after; a difference is reported, never cleaned up by this test.
func TestOrchestrateTransitionLeavesRealHomesAlone(t *testing.T) {
	home := os.Getenv("HOME")
	before := orchestrateTransitionListing(t, home)
	cwd := orchestrateTransitionRoot(t)
	id := "real-home"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P"}`)
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	if got := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", `{"from":"P","to":"A","did":"audited","planUnit":"`+unit+`"}`); got.Code != 0 {
		t.Fatalf("P>A: %+v", got)
	}
	if got := orchestrateTransitionRun(t, cwd, "reset", "--session", id); got.Code != 0 {
		t.Fatalf("reset: %+v", got)
	}
	if after := orchestrateTransitionListing(t, home); before != after {
		t.Fatalf("the host's ~/.codex or ~/.crw changed: %q -> %q", before, after)
	}
}

// TestOrchestrateTransitionHoldsTheSessionLock is the port's fix for the oracle's unlocked transition: while a
// participating writer holds the lock the transition cannot publish (the lock gives up after about 250 ms), and
// the update that writer made is kept once the transition runs.
func TestOrchestrateTransitionHoldsTheSessionLock(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "lock-hold"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P"}`)
	unit := orchestrateTransitionSeedPlanUnit(t, cwd)
	args := OrchestrateCliArgs{Verb: fsm.VerbA, Cwd: cwd, Attest: &attest.Attestation{From: state.PhaseP, To: state.PhaseA, Did: "audited", PlanUnit: unit}}
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = state.WithSessionLock(cwd, id, func() error {
			s := state.ReadState(cwd, id)
			s.MemoryWriteGrant = true
			if err := state.WriteState(cwd, s); err != nil {
				t.Error(err)
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if got, err := RunOrchestrateTransition(args, id); err == nil {
		t.Fatalf("the transition published while another writer held the session lock: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseP {
		t.Fatal("a refused transition moved the session")
	}
	close(release)
	got, err := RunOrchestrateTransition(args, id)
	if err != nil || got.Code != 0 {
		t.Fatalf("P>A: %+v %v", got, err)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseA || !after.MemoryWriteGrant {
		t.Fatalf("the participating writer's update was lost: %+v", after)
	}
}

// orchestrateTransitionListing is the entry names under ~/.codex and ~/.crw, or "absent".
// TestOrchestrateTransitionRowBeforeState pins the commit order: a row that cannot be written leaves the
// session untouched, so the same verb can be retried (the oracle's state-first order advanced the FSM).
func TestOrchestrateTransitionRowBeforeState(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "row-order"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
	if err := os.MkdirAll(filepath.Join(cwd, ".crw", "ledger.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrateTransitionTry(t, cwd, "P", "--session", id); err == nil {
		t.Fatal("a transition whose row cannot be written reported success")
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the session moved although its row could not be written")
	}
}

func orchestrateTransitionListing(t *testing.T, home string) string {
	t.Helper()
	out := []string{}
	for _, name := range []string{".codex", ".crw"} {
		entries, err := os.ReadDir(filepath.Join(home, name))
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if err != nil {
			names = []string{"absent"}
		}
		out = append(out, name+"="+strings.Join(names, ","))
	}
	return strings.Join(out, ";")
}
