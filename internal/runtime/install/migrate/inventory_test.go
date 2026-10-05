package migrate

import (
	"errors"
	"strings"
	"testing"
)

// inventoryRowCase is one path docs/port-cxc/state-migration.md lists for one root, with the single row that must claim it.
type inventoryRowCase struct {
	scope  Scope
	path   string
	row    string // the pattern the table must claim the path with
	disp   Disposition
	reason string // a substring the row's reason must carry
	lock   bool
}

// inventoryCases covers every row of the document's five tables (docs/port-cxc/state-migration.md:19-108) with one listed path
// each, plus near-miss paths whose claiming row must not be the row of a neighbouring line. The Codex leaf-map transforms are
// CodexLeaf's and are tested beside it.
var inventoryCases = []inventoryRowCase{
	{ScopeProject, "sessions/rec-1.json.lock", "sessions/*.json.lock", "", "session lock", true},
	{ScopeProject, "goalplans/rec-1/.goalplan.lock", "goalplans/*/.goalplan.lock", "", "goalplan write lock", true},
	{ScopeProject, "dispatches/rec-1/d-1.json.lock", "dispatches/*/*.json.lock", "", "dispatch lock", true},
	{ScopeProject, ".gitignore", ".gitignore", DispSkip, "CRW publishes its own", false},
	{ScopeProject, "sessions/rec-1.json", "sessions/*.json", DispCopy, "", false},
	{ScopeProject, "ledger.jsonl", "ledger.jsonl", DispCopy, "", false},
	{ScopeProject, "interviews/rec-1.jsonl", "interviews/*.jsonl", DispCopy, "", false},
	{ScopeProject, "interview/freeze.json", "interview/freeze.json", DispCopy, "", false},
	{ScopeProject, "plan/rec-1/draft.md", "plan/*/**", DispCopy, "", false},
	{ScopeProject, "plan/rec-1/draft.tmp", "plan/*/**", DispCopy, "", false},
	{ScopeProject, "goalplans/rec-1/goalplan.json", "goalplans/*/goalplan.json", DispCopy, "", false},
	{ScopeProject, "goalplans/rec-1/ledger.jsonl", "goalplans/*/ledger.jsonl", DispCopy, "", false},
	{ScopeProject, "goalplans/rec-1/schema-v2.marker", "goalplans/*/schema-v2.marker", DispCopy, "", false},
	{ScopeProject, "evidence/rec-1/test-receipt.json", "evidence/**", DispCopy, "", false},
	{ScopeProject, "evidence-attempts/rec-1-a.json", "evidence-attempts/*.json", DispCopy, "", false},
	{ScopeProject, "evidence-unrecordable/rec-1-a-1.json", "evidence-unrecordable/*.json", DispCopy, "", false},
	{ScopeProject, "sources/rec-1.json", "sources/*.json", DispCopy, "", false},
	{ScopeProject, "metrics.jsonl", "metrics.jsonl", DispCopy, "", false},
	{ScopeProject, "objective-kind/rec-1.json", "objective-kind/*.json", DispCopy, "", false},
	{ScopeProject, "divergence/rec-1.mode.json", "divergence/*.mode.json", DispCopy, "", false},
	{ScopeProject, "divergence/candidates.jsonl", "divergence/candidates.jsonl", DispCopy, "", false},
	{ScopeProject, "render-observations.jsonl", "render-observations.jsonl", DispCopy, "", false},
	{ScopeProject, "dispatches/rec-1/d-1.json", "dispatches/*/*.json", DispCopy, "", false},
	{ScopeProject, "bg/j1.json", "bg/*.json", DispCopy, "", false},
	{ScopeProject, "bg/j1.out", "bg/*.out", DispCopy, "", false},
	{ScopeProject, "bg/j1.exit", "bg/*.exit", DispCopy, "", false},
	{ScopeProject, "bg/j2.out.helper.cjs", "bg/*.out.helper.cjs", DispSkip, "Node execution helper", false},
	{ScopeProject, "bg/disabled", "bg/disabled", DispCopy, "", false},
	{ScopeProject, "bg/enabled-at", "bg/enabled-at", DispCopy, "", false},
	{ScopeProject, "bg/ledger.jsonl", "bg/ledger.jsonl", DispCopy, "", false},
	{ScopeProject, "attest.json", "attest.json", DispCopy, "", false},
	{ScopeProject, "subagents.json", "subagents.json", DispSkip, "role layer", false},
	{ScopeProject, "worktree-guard/rec-1.json", "worktree-guard/*.json", DispSkip, "injection marker", false},
	{ScopeProject, "affordance-recovery/ab12.pending", "affordance-recovery/*.pending", DispSkip, "advisory", false},
	{ScopeProject, "friction.jsonl", "friction.jsonl", DispSkip, "dormant hook layer", false},
	{ScopeProject, "edit-shapes.jsonl", "edit-shapes.jsonl", DispSkip, "excluded from the port", false},
	{ScopeProject, "rules/one.md", "rules/*.md", DispSkip, "rule injection was not ported", false},
	{ScopeProject, "release/cand.tmp", "release/**", DispSkip, "no CRW state consumer", false},
	{ScopeProject, "traces/activations.jsonl", "traces/activations.jsonl", DispSkip, "trace layer", false},
	{ScopeProject, "cache/repomap/tags.v1/x", "cache/repomap/tags.v1/**", DispSkip, "diskcache", false},
	{ScopeProject, "bridge.db", "bridge.db", DispSkip, "never copied", false},
	{ScopeProject, "bridge.db-wal", "bridge.db-wal", DispSkip, "bridge store", false},
	{ScopeProject, "bridge.db-shm", "bridge.db-shm", DispSkip, "bridge store", false},
	{ScopeProject, "bridge-events.jsonl", "bridge-events.jsonl*", DispSkip, "event-log", false},
	{ScopeProject, "bridge-events.jsonl.1", "bridge-events.jsonl*", DispSkip, "event-log", false},
	{ScopeUser, "subagents.json", "subagents.json", DispCopy, "", false},
	{ScopeUser, "config.json", "config.json", DispCopy, "", false},
	{ScopeUser, "model-catalog.json", "model-catalog.json", DispSkip, "rebuilt", false},
	{ScopeUser, "recall/index.sqlite", "recall/**", DispSkip, "cold rebuild", false},
	{ScopeUser, "skill-cache/one.cache", "skill-cache/**", DispSkip, "fetched again", false},
	{ScopeUser, "serve.out.log", "serve.out.log", DispSkip, "literal fallback home", false},
	{ScopeUser, "serve.err.log", "serve.err.log", DispSkip, "literal fallback home", false},
	{ScopeUser, "serve.cmd", "serve.cmd", DispSkip, "literal fallback home", false},
	{ScopeUser, "venvs/repomap/py/bin/x", "venvs/repomap/**", DispSkip, "interpreter environment", false},
	{ScopeUser, "runtime/ast-grep/linux-x64/sg", "runtime/ast-grep/*/sg", DispSkip, "executable installation", false},
	{ScopeUser, "runtime/ast-grep/linux-x64/sg.exe", "runtime/ast-grep/*/sg.exe", DispSkip, "executable installation", false},
	{ScopeCodex, "agents/.architect-update.lock", "agents/.*-update.lock", "", "registration lock", true},
	{ScopeCodex, "config.toml", "config.toml", DispSkip, "neither rewrites configuration", false},
	{ScopeCodex, "config.toml.bak-2026", "config.toml.bak-*", DispSkip, "retrust backup", false},
	{ScopeCodex, "agents/architect.toml", "agents/*.toml", DispSkip, "registration owns", false},
	{ScopeCodex, "agents/architect.toml.backup-abcd", "agents/*.toml.backup-*", DispSkip, "registration backup", false},
	{ScopeCodex, "codexclaw/subagents.json", "codexclaw/subagents.json", DispSkip, "removed layer", false},
	{ScopeCodex, "codexclaw/hook-observations/aa/bb/cc.json", "codexclaw/hook-observations/**", DispSkip, "diagnostics", false},
	{ScopeCodex, "memories_1.sqlite", "memories_*.sqlite", DispSkip, "stays in place", false},
	{ScopeCodex, "memories_1.sqlite-wal", "memories_*.sqlite-wal", DispSkip, "sidecar", false},
	{ScopeCodex, "memories_1.sqlite-shm", "memories_*.sqlite-shm", DispSkip, "sidecar", false},
	{ScopeCodex, "memories_1.sqlite-journal", "memories_*.sqlite-journal", DispSkip, "sidecar", false},
	{ScopeCodex, "runtime/ast-grep/linux-x64/sg", "runtime/ast-grep/**", DispSkip, "outside migration", false},
}

// inventoryUnclaimed are paths no listed row may claim: an unknown child is reported by the walker, never matched here.
var inventoryUnclaimed = map[Scope][]string{
	ScopeProject: {"notes.txt", "sessions/extra.txt", "sessions/rec-1.json.tmp", "bg/j1.json.tmp-1-2"},
	ScopeUser:    {"notes.txt"},
	ScopeCodex:   {"other.txt"},
}

func inventoryTable(scope Scope) []inventoryRow {
	switch scope {
	case ScopeProject:
		return inventoryProjectRows
	case ScopeUser:
		return inventoryUserRows
	default:
		return inventoryCodexRows
	}
}

func TestInventoryRowsClaimTheirDocumentedPaths(t *testing.T) {
	for _, c := range inventoryCases {
		row := inventoryMatchRow(inventoryTable(c.scope), c.path)
		if row == nil {
			t.Errorf("%s %q is claimed by no row", c.scope, c.path)
			continue
		}
		if row.pattern != c.row {
			t.Errorf("%s %q is claimed by %q, want %q", c.scope, c.path, row.pattern, c.row)
		}
		if row.lock != c.lock || !c.lock && row.disp != c.disp {
			t.Errorf("%s %q lock=%v disp=%q, want lock=%v disp=%q", c.scope, c.path, row.lock, row.disp, c.lock, c.disp)
		}
		if c.reason != "" && !strings.Contains(row.reason, c.reason) {
			t.Errorf("%s %q reason = %q, want it to carry %q", c.scope, c.path, row.reason, c.reason)
		}
	}
}

// TestInventoryRowsAreExclusive proves a listed path is claimed by exactly one row (an over-matching row would show here) and
// that an unlisted path is claimed by none.
func TestInventoryRowsAreExclusive(t *testing.T) {
	for _, c := range inventoryCases {
		hits := 0
		for _, row := range inventoryTable(c.scope) {
			if inventoryMatch(row.pattern, c.path) {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("%s %q is matched by %d rows, want exactly 1", c.scope, c.path, hits)
		}
	}
	for scope, paths := range inventoryUnclaimed {
		for _, path := range paths {
			if row := inventoryMatchRow(inventoryTable(scope), path); row != nil {
				t.Errorf("%s %q is claimed by %q", scope, path, row.pattern)
			}
		}
	}
}

func TestInventoryRowShape(t *testing.T) {
	for _, table := range [][]inventoryRow{inventoryProjectRows, inventoryUserRows, inventoryCodexRows} {
		for _, row := range table {
			switch {
			case row.pattern == "":
				t.Error("a row carries no pattern")
			case row.lock && row.reason == "":
				t.Errorf("lock row %q carries no reason", row.pattern)
			case row.disp == DispSkip && row.reason == "":
				t.Errorf("skip row %q carries no reason", row.pattern)
			case !row.lock && row.disp != DispCopy && row.disp != DispTransform && row.disp != DispSkip:
				t.Errorf("row %q carries no disposition", row.pattern)
			}
		}
	}
}

func TestInventoryNameSupported(t *testing.T) {
	if err := inventoryNameSupported("x", "plan.tmp"); err != nil {
		t.Errorf("an ordinary name is refused: %v", err)
	}
	if err := inventoryNameSupported("x", strings.Repeat("a", 255)); err != nil {
		t.Errorf("a 255-byte name is refused: %v", err)
	}
	for _, name := range []string{strings.Repeat("a", 256), "bad\nname", string([]byte{0xff, 0xfe})} {
		var ref *RefusedError
		if err := inventoryNameSupported("x", name); !errors.As(err, &ref) || ref.Reason != ReasonUnsupported {
			t.Errorf("inventoryNameSupported(%q) = %v, want a %q refusal", name, err, ReasonUnsupported)
		}
	}
}

func TestInventoryMatchEdges(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"sessions/*.json", "sessions/rec-1.json", true},
		{"sessions/*.json", "sessions/rec-1.json.lock", false},
		{"sessions/*.json", "sessions/sub/rec-1.json", false},
		{"sessions/*.json", "rec-1.json", false},
		{"plan/*/**", "plan", false},
		{"plan/*/**", "plan/rec-1", true},
		{"plan/*/**", "plan/rec-1/sub/x.md", true},
		{"plan/*/**", "other/rec-1/x", false},
		{"memories_*.sqlite*", "memories_1.sqlite-wal", true},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "ab", false},
		{"bridge-events.jsonl*", "bridge-events.jsonl.1", true},
		{"", "", false},
	} {
		if got := inventoryMatch(c.pattern, c.path); got != c.want {
			t.Errorf("inventoryMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestInventoryContainers(t *testing.T) {
	for _, c := range []struct {
		pattern, dir string
		want         bool
	}{
		{"plan/*/**", "plan", true},
		{"plan/*/**", "plan/rec-1", true},
		{"plan/*/**", "plan/rec-1/sub", true},
		{"ledger.jsonl", "ledger.jsonl", false},
		{"goalplans/*/goalplan.json", "goalplans", true},
		{"goalplans/*/goalplan.json", "goalplans/rec-1", true},
		{"sessions/*.json.lock", "sessions", true},
		{"cache/repomap/tags.v1/**", "cache", true},
		{"", "", false},
	} {
		if got := inventoryUnder(c.pattern, c.dir); got != c.want {
			t.Errorf("inventoryUnder(%q, %q) = %v, want %v", c.pattern, c.dir, got, c.want)
		}
	}
	if !inventoryTraverses(inventoryProjectRows, "cache") || inventoryCopies(inventoryProjectRows, "cache") {
		t.Error("cache holds only skipped rows: it must be traversed and is not a copy")
	}
	if !inventoryCopies(inventoryProjectRows, "goalplans") || !inventoryCopies(inventoryProjectRows, "plan") {
		t.Error("a container of copied rows is a copy")
	}
	if !inventoryTraverses(inventoryCodexRows, "agents") || inventoryCopies(inventoryCodexRows, "agents") {
		t.Error("agents holds only skipped and lock rows: a lock row transfers nothing and is not a copy")
	}
	if inventoryTraverses(inventoryCodexRows, "other") {
		t.Error("an unlisted container is not traversed by any row")
	}
}
