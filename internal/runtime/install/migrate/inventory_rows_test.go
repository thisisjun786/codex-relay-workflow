package migrate

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// invTree writes entries below base: a key ending in "/" is a directory, every other key a file with mode 0644.
func invTree(t *testing.T, base string, entries map[string]string) {
	t.Helper()
	for rel, data := range entries {
		p := filepath.Join(base, rel)
		if strings.HasSuffix(rel, "/") {
			mkdirs(t, p)
			continue
		}
		put(t, p, data, 0o644)
	}
}

// invClassify opens the roots o names and classifies them; the roots close with the test.
func invClassify(t *testing.T, o Options) (*Plan, error) {
	t.Helper()
	r, err := Open(o)
	if err != nil {
		t.Fatalf("Open(%+v) = %v", o, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return classify(r)
}

// invWant returns the item of one scope and source path, failing when it is absent or carries another disposition.
func invWant(t *testing.T, p *Plan, scope Scope, source string, disp Disposition) Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Scope == scope && it.Source == source {
			if it.Disposition != disp {
				t.Fatalf("item %s %q is %q (%s), want %q", scope, source, it.Disposition, it.Reason, disp)
			}
			return it
		}
	}
	t.Fatalf("no item for %s %q; items: %v", scope, source, invSources(p))
	return Item{}
}

func invSources(p *Plan) []string {
	out := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, string(it.Scope)+":"+it.Source)
	}
	return out
}

func invBg(id, status string) string {
	return `{"id":"` + id + `","cwd":"/ws","command":["x"],"status":"` + status + `"}`
}

func invDispatch(id, record, attempt string) string {
	return `{"version":1,"sessionId":"rec-1","id":"` + id + `","role":"reviewer","candidates":[],"attempts":[{"id":"a-1","status":"` + attempt + `"}],"status":"` + record + `"}`
}

const invStamp = "2026-01-01T00-00-00-000Z"

// invProjectEntries is one entry for every row of the design's project tables (retained and skipped) plus one unknown file.
func invProjectEntries() map[string]string {
	return map[string]string{
		".gitignore":                           "cxc ignore text\n",
		"sessions/rec-1.json":                  `{"phase":"B"}`,
		"ledger.jsonl":                         `{"event":"created"}`,
		"interviews/rec-1.jsonl":               `{"q":1}`,
		"interview/freeze.json":                "{}",
		"plan/rec-1/draft.md":                  "plan bytes",
		"plan/rec-1/draft.tmp":                 "ordinary plan data",
		"goalplans/rec-1/goalplan.json":        "{}",
		"goalplans/rec-1/ledger.jsonl":         "{}",
		"goalplans/rec-1/schema-v2.marker":     "v2",
		"evidence/rec-1/test-receipt.json":     "{}",
		"evidence/rec-1/screenshot.png":        "png",
		"evidence-attempts/rec-1-a.json":       "{}",
		"evidence-unrecordable/rec-1-a-1.json": "{}",
		"sources/rec-1.json":                   "{}",
		"metrics.jsonl":                        "{}",
		"objective-kind/rec-1.json":            "{}",
		"divergence/rec-1.mode.json":           "{}",
		"divergence/candidates.jsonl":          "{}",
		"render-observations.jsonl":            "{}",
		"dispatches/rec-1/d-1.json":            invDispatch("d-1", "complete", "complete"),
		"bg/j1.json":                           invBg("j1", "complete"),
		"bg/j1.out":                            "out",
		"bg/j1.exit":                           "0",
		"bg/disabled":                          "",
		"bg/enabled-at":                        invStamp,
		"bg/ledger.jsonl":                      "{}",
		"bg/j2.out.helper.cjs":                 "node helper",
		"attest.json":                          "{}",
		"subagents.json":                       "{}",
		"worktree-guard/rec-1.json":            "{}",
		"affordance-recovery/ab12.pending":     "{}",
		"friction.jsonl":                       "{}",
		"edit-shapes.jsonl":                    "{}",
		"rules/one.md":                         "rule",
		"release/cand.tmp":                     "release",
		"traces/activations.jsonl":             "{}",
		"cache/repomap/tags.v1/x":              "tag",
		"bridge.db":                            "db",
		"bridge.db-wal":                        "wal",
		"bridge.db-shm":                        "shm",
		"bridge-events.jsonl":                  "{}",
		"bridge-events.jsonl.1":                "{}",
		"notes.txt":                            "unknown",
	}
}

var invProjectCopies = []string{
	"sessions/rec-1.json", "ledger.jsonl", "interviews/rec-1.jsonl", "interview/freeze.json",
	"plan", "plan/rec-1", "plan/rec-1/draft.md", "plan/rec-1/draft.tmp",
	"goalplans", "goalplans/rec-1", "goalplans/rec-1/goalplan.json", "goalplans/rec-1/ledger.jsonl",
	"goalplans/rec-1/schema-v2.marker", "evidence", "evidence/rec-1", "evidence/rec-1/test-receipt.json",
	"evidence/rec-1/screenshot.png", "evidence-attempts/rec-1-a.json",
	"evidence-unrecordable/rec-1-a-1.json", "sources/rec-1.json", "metrics.jsonl",
	"objective-kind/rec-1.json", "divergence/rec-1.mode.json", "divergence/candidates.jsonl",
	"render-observations.jsonl", "dispatches", "dispatches/rec-1", "dispatches/rec-1/d-1.json",
	"bg", "bg/j1.json", "bg/j1.out", "bg/j1.exit", "bg/disabled", "bg/enabled-at", "bg/ledger.jsonl",
	"attest.json",
}

var invProjectSkips = []string{
	".gitignore", "bg/j2.out.helper.cjs", "subagents.json", "worktree-guard/rec-1.json",
	"affordance-recovery/ab12.pending", "friction.jsonl", "edit-shapes.jsonl", "rules/one.md",
	"release", "traces/activations.jsonl", "cache/repomap/tags.v1",
	"bridge.db", "bridge.db-wal", "bridge.db-shm", "bridge-events.jsonl", "bridge-events.jsonl.1",
	"notes.txt",
}

func TestInventoryRetainedRows(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	src := filepath.Join(ws, ProjectSourceName)
	invTree(t, src, invProjectEntries())
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	invWant(t, p, ScopeProject, ".", DispTransform)
	for _, s := range invProjectCopies {
		it := invWant(t, p, ScopeProject, s, DispCopy)
		if it.Destination != s {
			t.Errorf("copy item %q dest = %q, want the same path", s, it.Destination)
		}
	}
	for _, s := range invProjectSkips {
		invWant(t, p, ScopeProject, s, DispSkip)
	}
	root := invWant(t, p, ScopeProject, ".", DispTransform)
	if root.Destination != "." || root.Mode.Perm() != 0o755 {
		t.Errorf("root item = %+v, want destination . and the source mode", root)
	}
	ledger := invWant(t, p, ScopeProject, "ledger.jsonl", DispCopy)
	if want := sha256.Sum256([]byte(`{"event":"created"}`)); ledger.Digest != want || ledger.Size != 19 || ledger.Mode.Perm() != 0o644 {
		t.Errorf("ledger item = size %d digest %x mode %v", ledger.Size, ledger.Digest, ledger.Mode)
	}
}

func TestInventorySkippedRowsReasons(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), invProjectEntries())
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	for _, s := range invProjectSkips {
		it := invWant(t, p, ScopeProject, s, DispSkip)
		if strings.TrimSpace(it.Reason) == "" {
			t.Errorf("skip item %q carries no reason", s)
		}
	}
	if got := invWant(t, p, ScopeProject, "notes.txt", DispSkip).Reason; !strings.Contains(got, "not in the inventory") {
		t.Errorf("unknown child reason = %q", got)
	}
	if got := invWant(t, p, ScopeProject, "subagents.json", DispSkip).Reason; !strings.Contains(got, "role layer") {
		t.Errorf("subagents.json reason = %q, want the removed project role layer", got)
	}
}

func TestInventoryUserRows(t *testing.T) {
	base := isolate(t)
	u := filepath.Join(base, "eu")
	mkdirs(t, filepath.Join(base, "ev"))
	invTree(t, u, map[string]string{
		"subagents.json":                    "{}",
		"config.json":                       "{}",
		"model-catalog.json":                "{}",
		"recall/index.sqlite":               "db",
		"recall/hit.json":                   "{}",
		"skill-cache/one.cache":             "c",
		"serve.out.log":                     "log",
		"serve.err.log":                     "log",
		"serve.cmd":                         "cmd",
		"venvs/repomap/py/bin/x":            "py",
		"runtime/ast-grep/linux-x64/sg":     "sg",
		"runtime/ast-grep/linux-x64/sg.exe": "sg",
		"notes.txt":                         "unknown",
	})
	p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: u, ToHome: filepath.Join(base, "ev")})
	must(t, err)
	for _, s := range []string{"subagents.json", "config.json"} {
		if it := invWant(t, p, ScopeUser, s, DispCopy); it.Destination != s {
			t.Errorf("user copy %q dest = %q", s, it.Destination)
		}
	}
	for _, s := range []string{"model-catalog.json", "recall", "skill-cache", "serve.out.log", "serve.err.log",
		"serve.cmd", "venvs", "venvs/repomap", "runtime", "runtime/ast-grep", "runtime/ast-grep/linux-x64",
		"runtime/ast-grep/linux-x64/sg", "runtime/ast-grep/linux-x64/sg.exe", "notes.txt"} {
		invWant(t, p, ScopeUser, s, DispSkip)
	}
}

func TestInventoryUserFallbackHome(t *testing.T) {
	base := isolate(t)
	u := filepath.Join(base, "eu")
	mkdirs(t, u)
	literal := filepath.Join(os.Getenv("HOME"), ".codexclaw")
	invTree(t, literal, map[string]string{"serve.out.log": "log", "serve.err.log": "log", "keep.json": "{}"})
	p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: u, ToHome: filepath.Join(base, "ev")})
	must(t, err)
	for _, name := range []string{"serve.out.log", "serve.err.log"} {
		it := invWant(t, p, ScopeUser, filepath.Join(literal, name), DispSkip)
		if it.Destination != "" {
			t.Errorf("fallback item %q has destination %q; it is reported, never copied", name, it.Destination)
		}
	}
	for _, it := range p.Items {
		if strings.HasSuffix(it.Source, "keep.json") && it.Disposition == DispCopy {
			t.Errorf("the literal fallback home was copied: %+v", it)
		}
	}
}

func TestInventoryCodexRows(t *testing.T) {
	base := isolate(t)
	c := filepath.Join(base, "ec")
	invTree(t, c, map[string]string{
		".codexclaw-install.json":                    "{}",
		"codexclaw-self-heal.json":                   "{}",
		"config.toml.codexclaw-" + invStamp + ".bak": "bak",
		"config.toml":                                "cfg",
		"config.toml.bak-2026":                       "bak",
		"agents/architect.toml":                      "role",
		"agents/executor.toml":                       "role",
		"agents/architect.toml.backup-abcd":          "bak",
		"codexclaw/subagents.json":                   "{}",
		"codexclaw/hook-observations/aa/bb/cc.json":  "{}",
		"memories_1.sqlite":                          "db",
		"memories_1.sqlite-wal":                      "wal",
		"runtime/ast-grep/linux-x64/sg":              "sg",
		"other.txt":                                  "unknown",
	})
	p, err := invClassify(t, Options{Scope: ScopeCodex, CodexHome: c})
	must(t, err)
	transforms := map[string]string{
		".codexclaw-install.json":                    ".crw-install.json",
		"codexclaw-self-heal.json":                   "crw-self-heal.json",
		"config.toml.codexclaw-" + invStamp + ".bak": "config.toml.crw-" + invStamp + ".bak",
	}
	for src, dst := range transforms {
		if it := invWant(t, p, ScopeCodex, src, DispTransform); it.Destination != dst {
			t.Errorf("codex transform %q dest = %q, want %q", src, it.Destination, dst)
		}
	}
	for _, s := range []string{"config.toml", "config.toml.bak-2026", "agents/architect.toml", "agents/executor.toml",
		"agents/architect.toml.backup-abcd", "codexclaw/subagents.json", "codexclaw/hook-observations",
		"memories_1.sqlite", "memories_1.sqlite-wal", "runtime/ast-grep", "other.txt"} {
		invWant(t, p, ScopeCodex, s, DispSkip)
	}
}

// invRun is a 26-character run of crwdir/atomic.go's base32 alphabet, the random part of a CRW atomic-publish temporary
// (internal/pabcd/crwdir/atomic.go:92 publishes through "." + final + "." + rand.Text() + ".tmp").
const invRun = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// TestInventoryEvidenceProducerTemps proves the evidence/** producer-temporary rule of c1: a file whose name exactly matches a
// producer's temporary shape is reported and skipped with inventoryReasonIntermediate, while any other .tmp name in evidence/**
// (and any .tmp name in plan/**) is ordinary data and is copied.
func TestInventoryEvidenceProducerTemps(t *testing.T) {
	base := isolate(t)
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	producer := ".test-receipt.json." + invRun + ".tmp"
	invTree(t, filepath.Join(ws, ProjectSourceName), map[string]string{
		"evidence/s1/" + producer: "half a write",
		"evidence/s1/notes.tmp":   "ordinary evidence data",
		"plan/u/draft.tmp":        "ordinary plan data",
		"sessions/rec-1.json":     "{}",
	})
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	if it := invWant(t, p, ScopeProject, "evidence/s1/"+producer, DispSkip); it.Reason != inventoryReasonIntermediate {
		t.Errorf("evidence producer temporary reason = %q, want %q", it.Reason, inventoryReasonIntermediate)
	}
	if it := invWant(t, p, ScopeProject, "evidence/s1/notes.tmp", DispCopy); it.Destination != "evidence/s1/notes.tmp" {
		t.Errorf("a user .tmp in evidence is ordinary data; got %+v", it)
	}
	if it := invWant(t, p, ScopeProject, "plan/u/draft.tmp", DispCopy); it.Destination != "plan/u/draft.tmp" {
		t.Errorf("a user .tmp in plan is ordinary data; got %+v", it)
	}
}
