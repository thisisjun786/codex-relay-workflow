package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attHas reports whether the report holds an entry for the item, kind and field (an empty field matches any).
func attHas(got []Attention, item, kind, field string) bool {
	for _, e := range got {
		if e.Item == item && e.Kind == kind && (field == "" || e.Field == field) {
			return true
		}
	}
	return false
}

func TestAttentionReportsTheListedFields(t *testing.T) {
	old := "old/.codexclaw"
	_, r, p := apPlan(t, map[string]string{
		"sessions/rec-1.json":              "{\"phase\":\"B\",\"planUnit\":\"" + old + "/plan/s\"}",
		"attest.json":                      "{\"planUnit\":\"plan/s\",\"planPaths\":[\"" + old + "/plan/s/000_plan.md\"],\"testReceiptPath\":\"" + old + "/evidence/r/test-receipt.json\"}",
		"goalplans/s/goalplan.json":        "{\"finalGate\":{\"testReceiptPath\":\"" + old + "/evidence/r/test-receipt.json\",\"qaReceiptPath\":\"q\"},\"reviewRounds\":[{\"planPath\":\"plan/s/x.md\",\"planSha256\":\"deadbeef\"}]}",
		"sources/rec-1.json":               "{\"nativeCwd\":\"/w\",\"sourceRoot\":\"" + old + "/w\",\"commonDir\":\"/w/.git\",\"gitDir\":\"/w/.git/w\"}",
		"interview/freeze.json":            "{\"planFiles\":[{\"path\":\"a.md\"},{\"path\":\"b.md\"}],\"planHash\":\"cafe\"}",
		"divergence/candidates.jsonl":      "{\"worktree\":\"" + old + "/wt\"}\n{\"sourceUrls\":[\"" + old + "/u\"]}\n",
		"render-observations.jsonl":        "{\"screenshotPath\":\"" + old + "/s.png\"}\n",
		"evidence/rec-1/test-receipt.json": "{\"sourceIdentity\":{\"sourceRoot\":\"" + old + "/w\"},\"generatedPaths\":[\"" + old + "/g\"],\"artifactRefs\":[\"v.json\"]}",
	}, nil)
	got := attention(r, p)
	for _, want := range []struct{ item, kind, field string }{
		{"sessions/rec-1.json", AttentionOldRoot, "planUnit"},
		{"sessions/rec-1.json", AttentionFreshness, "phase"},
		{"attest.json", AttentionOldRoot, "planPaths.*"},
		{"attest.json", AttentionOldRoot, "testReceiptPath"},
		{"goalplans/s/goalplan.json", AttentionOldRoot, "finalGate.testReceiptPath"},
		{"goalplans/s/goalplan.json", AttentionPathHash, "reviewRounds.*.planSha256"},
		{"sources/rec-1.json", AttentionOldRoot, "sourceRoot"},
		{"interview/freeze.json", AttentionPathHash, "planHash"},
		{"divergence/candidates.jsonl", AttentionOldRoot, "worktree"},
		{"divergence/candidates.jsonl", AttentionOldRoot, "sourceUrls.*"},
		{"render-observations.jsonl", AttentionOldRoot, "screenshotPath"},
		{"evidence/rec-1/test-receipt.json", AttentionOldRoot, "sourceIdentity.sourceRoot"},
		{"evidence/rec-1/test-receipt.json", AttentionOldRoot, "generatedPaths.*"},
	} {
		if !attHas(got, want.item, want.kind, want.field) {
			t.Errorf("no %s entry for %s field %s", want.kind, want.item, want.field)
		}
	}
	// An unrelated value is not reported, and the plan's own relative paths are not old-root references.
	if attHas(got, "goalplans/s/goalplan.json", AttentionOldRoot, "reviewRounds.*.planPath") {
		t.Error("a plan-relative path must not be reported as an old-root reference")
	}
}

func TestAttentionReportsTheFreezeOrderCase(t *testing.T) {
	_, r, p := apPlan(t, map[string]string{
		"interview/freeze.json": "{\"planFiles\":[{\"path\":\"a.md\"},{\"path\":\"caf\u00e9.md\"}],\"planHash\":\"cafe\"}",
	}, nil)
	got := attention(r, p)
	if !attHas(got, "interview/freeze.json", AttentionFreezeOrder, "planFiles.*.path") {
		t.Fatalf("no freeze-order entry: %v", got)
	}
	for _, e := range got {
		if e.Kind == AttentionFreezeOrder && e.Value != "cafe" {
			t.Errorf("the recorded hash must be reported unchanged, got %q", e.Value)
		}
	}
	_, r2, p2 := apPlan(t, map[string]string{
		"interview/freeze.json": "{\"planFiles\":[{\"path\":\"caf\u00e9.md\"}],\"planHash\":\"cafe\"}",
	}, nil)
	if attHas(attention(r2, p2), "interview/freeze.json", AttentionFreezeOrder, "") {
		t.Error("a one-file freeze must not raise the ordering case")
	}
}

func TestAttentionReportsOnlyBAndCSessions(t *testing.T) {
	for phase, want := range map[string]bool{"B": true, "C": true, "P": false, "IDLE": false} {
		_, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": "{\"phase\":\"" + phase + "\"}"}, nil)
		if got := attHas(attention(r, p), "sessions/rec-1.json", AttentionFreshness, ""); got != want {
			t.Errorf("phase %s: freshness = %v, want %v", phase, got, want)
		}
	}
}

func TestAttentionReportsARecordItCannotDecode(t *testing.T) {
	// The record is a JSONL row: a session file that is not JSON is refused by the preflight before this report runs
	// (CRW-682), so the undecodable case moves to a ledger whose reader skips a damaged line.
	_, r, p := apPlan(t, map[string]string{"divergence/candidates.jsonl": "not json\n"}, nil)
	got := attention(r, p)
	if len(got) != 1 || got[0].Kind != "" || !strings.Contains(got[0].Detail, "cannot be decoded") {
		t.Fatalf("got %v, want one undecoded entry", got)
	}
}

func TestAttentionLeavesEveryRecordAlone(t *testing.T) {
	body := "{\"worktree\":\"old/.codexclaw/wt\"}\n"
	ws, r, p := apPlan(t, map[string]string{"divergence/candidates.jsonl": body, "sessions/a.json": "{\"phase\":\"P\"}"}, nil)
	src := filepath.Join(ws, ProjectSourceName, "divergence", "candidates.jsonl")
	before := get(t, src)
	if len(attention(r, p)) == 0 {
		t.Fatal("the divergence ledger must be read")
	}
	if after := get(t, src); after != before {
		t.Error("attention changed the source record")
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
		t.Errorf("attention must write nothing: %v", err)
	}
}
func TestAttentionReportsTheInstallManifestTableKeys(t *testing.T) {
	base := isolate(t)
	home := filepath.Join(base, "codex")
	mkdirs(t, home)
	put(t, filepath.Join(home, installSource), "{\"tableKeys\":{\"a.b\":{\"priorValue\":\"old/.codexclaw/x\",\"appliedValue\":\"old/.codexclaw/y\"}}}", 0o644)
	r, err := Open(Options{Scope: ScopeCodex, CodexHome: home})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	got := attention(r, p)
	if !attHas(got, installSource, AttentionOldRoot, "tableKeys.*.priorValue") || !attHas(got, installSource, AttentionOldRoot, "tableKeys.*.appliedValue") {
		t.Fatalf("the manifest table rows must be scanned: %v", got)
	}
}

func TestAttentionBoundsWhatItReads(t *testing.T) {
	// The session keeps a real phase so the preflight accepts it (CRW-682); P raises no freshness entry of its own.
	_, r, p := apPlan(t, map[string]string{"sessions/rec-1.json": "{\"phase\":\"P\",\"note\":\"" + strings.Repeat("x", attentionReadCap) + "\"}"}, nil)
	got := attention(r, p)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "larger than") {
		t.Fatalf("got %v, want one entry naming the size bound", got)
	}
}
