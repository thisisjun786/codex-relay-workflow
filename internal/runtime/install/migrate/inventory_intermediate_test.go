package migrate

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The producer temporaries of docs/port-cxc/state-migration.md:105, with a valid pid, millisecond and uuid.
const (
	invPID    = "4242"
	invMillis = "1760000000000"
	invUUID   = "12345678-1234-1234-1234-123456789abc"
)

// invProjectProducerTemps is one temporary per producer row 105 names that writes inside the project root, keyed by its
// root-relative path: sessions (state.ts:387,625), goalplans (goalplan.ts:932), sources (session-source.ts:188),
// evidence-attempts and evidence-unrecordable (subagent-evidence.ts:222,359), bg (store.ts:53, spawn.ts:50) and the root
// subagents.json (store.ts:256).
func invProjectProducerTemps() map[string]string {
	return map[string]string{
		"sessions/rec-1.json." + invPID + "." + invUUID + ".tmp":              "half a write",
		"sessions/rec-1.json." + invPID + "." + invMillis + ".tmp":            "half a write",
		"goalplans/rec-1/goalplan.json." + invPID + "." + invMillis + ".tmp":  "half a write",
		"sources/rec-1.json." + invUUID + ".tmp":                              "half a write",
		"evidence-attempts/rec-1-a.json." + invPID + "." + invMillis + ".tmp": "half a write",
		"evidence-unrecordable/.probe-123-456":                                "",
		"bg/j1.json.tmp-1-2":                                                  "half a write",
		"bg/j1.exit.tmp":                                                      "0",
		"subagents.json." + invUUID + ".tmp":                                  "half a write",
	}
}

// TestInventoryProducerIntermediates proves c1: every producer row 105 names, matched only inside its own directory, is
// skipped and reported with inventoryReasonIntermediate.
func TestInventoryProducerIntermediates(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), invProjectProducerTemps())
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	for rel := range invProjectProducerTemps() {
		if it := invWant(t, p, ScopeProject, rel, DispSkip); it.Reason != inventoryReasonIntermediate {
			t.Errorf("producer temporary %q reason = %q, want %q", rel, it.Reason, inventoryReasonIntermediate)
		}
	}
}

// invBoundaryPlan classifies a project root holding the boundary names of c2.
func invBoundaryPlan(t *testing.T) *Plan {
	t.Helper()
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{
		"plan/rec-1/draft.tmp":                    "ordinary plan data",
		"evidence/x/.123.1760000000000.tmp":       "ordinary evidence data",
		"evidence/x/a.json.123.1760000000000.tmp": "half a write",
		"bg/j1.json.tmp-x-2":                      "a shape mismatch",
		"sessions/notes.tmp":                      "a bare .tmp outside a producer shape",
	})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	return p
}

// TestInventoryUserTmpIsCopied proves c2: an ordinary .tmp name in the user plan tree, and a name in evidence whose final part
// before the pid is empty, are copied as ordinary data.
func TestInventoryUserTmpIsCopied(t *testing.T) {
	p := invBoundaryPlan(t)
	if it := invWant(t, p, ScopeProject, "plan/rec-1/draft.tmp", DispCopy); it.Destination != "plan/rec-1/draft.tmp" {
		t.Errorf("a user .tmp in plan is ordinary data; got %+v", it)
	}
	if it := invWant(t, p, ScopeProject, "evidence/x/.123.1760000000000.tmp", DispCopy); it.Destination != "evidence/x/.123.1760000000000.tmp" {
		t.Errorf("an empty-final .tmp in evidence is ordinary data; got %+v", it)
	}
}

// TestInventoryProducerTmpInEvidenceIsSkipped proves c2: a producer .tmp in evidence with a non-empty final part is still
// skipped as an intermediate.
func TestInventoryProducerTmpInEvidenceIsSkipped(t *testing.T) {
	p := invBoundaryPlan(t)
	if it := invWant(t, p, ScopeProject, "evidence/x/a.json.123.1760000000000.tmp", DispSkip); it.Reason != inventoryReasonIntermediate {
		t.Errorf("a producer .tmp in evidence reason = %q, want %q", it.Reason, inventoryReasonIntermediate)
	}
}

// TestInventoryNonProducerTmpStaysUnregistered proves c1's directory scoping: a .tmp name whose shape is not a producer's, or
// that sits outside a producer's directory, is reported as an unregistered path, never as a producer intermediate.
func TestInventoryNonProducerTmpStaysUnregistered(t *testing.T) {
	p := invBoundaryPlan(t)
	for _, rel := range []string{"bg/j1.json.tmp-x-2", "sessions/notes.tmp"} {
		if it := invWant(t, p, ScopeProject, rel, DispSkip); !strings.Contains(it.Reason, "not in the inventory") {
			t.Errorf("%q must stay unregistered, not be an intermediate; got %q", rel, it.Reason)
		}
	}
}

// TestInventoryBgRecordWithTmpInItsIDIsJudged proves the Codex P2 fix: a durable bg record whose id holds ".tmp-" reaches its
// bg row and its record judge instead of being skipped as an intermediate.
func TestInventoryBgRecordWithTmpInItsIDIsJudged(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{
		"bg/job.tmp-live.json": invBg("job.tmp-live", "complete"),
	})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	if it := invWant(t, p, ScopeProject, "bg/job.tmp-live.json", DispCopy); it.Destination != "bg/job.tmp-live.json" {
		t.Errorf("a complete bg record whose id holds .tmp- must copy; got %+v", it)
	}

	base = isolate(t)
	ws = filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{
		"bg/job.tmp-live.json": invBg("job.tmp-live", "running"),
	})
	_, err = invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	var ref *RefusedError
	if !errors.As(err, &ref) || ref.Reason != ReasonActive {
		t.Fatalf("a running bg record whose id holds .tmp- must refuse ReasonActive; got %v", err)
	}
}

// TestInventoryUserProducerIntermediates proves the user-root producers: subagents.json (store.ts:256) and model-catalog.json
// (live-catalog.ts:86), each <final>.<uuid>.tmp.
func TestInventoryUserProducerIntermediates(t *testing.T) {
	base := isolate(t)
	u := filepath.Join(base, "eu")
	mkdirs(t, filepath.Join(base, "ev"))
	invTree(t, u, map[string]string{
		"subagents.json." + invUUID + ".tmp":     "half a write",
		"model-catalog.json." + invUUID + ".tmp": "half a write",
	})
	p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: u, ToHome: filepath.Join(base, "ev")})
	must(t, err)
	for _, rel := range []string{"subagents.json." + invUUID + ".tmp", "model-catalog.json." + invUUID + ".tmp"} {
		if it := invWant(t, p, ScopeUser, rel, DispSkip); it.Reason != inventoryReasonIntermediate {
			t.Errorf("user producer temporary %q reason = %q, want %q", rel, it.Reason, inventoryReasonIntermediate)
		}
	}
}

// TestInventoryCodexProducerIntermediates proves the Codex-home producers: the self-heal marker and install manifest
// (self-heal.ts:275,310), hook-trust's .config.toml.tmp-<pid>-<ms> (hook-trust.ts:363) and role-registration's
// .<role>-<uuid>.tmp (role-registration.ts:88).
func TestInventoryCodexProducerIntermediates(t *testing.T) {
	base := isolate(t)
	c := filepath.Join(base, "ec")
	invTree(t, c, map[string]string{
		selfHealSource + tempSuffix:                 "half a write",
		".config.toml.tmp-123-456":                  "half a write",
		"agents/.architect-" + invUUID + tempSuffix: "half a write",
	})
	p, err := invClassify(t, Options{Scope: ScopeCodex, CodexHome: c})
	must(t, err)
	for _, rel := range []string{selfHealSource + tempSuffix, ".config.toml.tmp-123-456", "agents/.architect-" + invUUID + tempSuffix} {
		if it := invWant(t, p, ScopeCodex, rel, DispSkip); it.Reason != inventoryReasonIntermediate {
			t.Errorf("codex producer temporary %q reason = %q, want %q", rel, it.Reason, inventoryReasonIntermediate)
		}
	}
}

// TestInventoryProducerTempFinalName proves the P1 4 fix: the evidence rule's pid-millisecond and uuid shapes match only when
// the final-name part is non-empty.
func TestInventoryProducerTempFinalName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{".123.1760000000000.tmp", false},
		{"a.json.123.1760000000000.tmp", true},
		{"." + invUUID + ".tmp", false},
		{"a.json." + invUUID + ".tmp", true},
		{".test-receipt.json." + invRun + ".tmp", true},
		{tempSuffix, false},
		{"a.json", false},
	} {
		if got := classifyProducerTemp(c.name); got != c.want {
			t.Errorf("classifyProducerTemp(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
