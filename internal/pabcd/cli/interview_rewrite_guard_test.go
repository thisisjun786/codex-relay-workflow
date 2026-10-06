package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The cli rewrite guard cliVerdictsIntact only judges unverifiedSubagents, while state.ReadStateStrict rebuilds the interview
// tracker through interview.ReconstructInterview: contradictions and assumptions are capped at interview.MaxTrackerArray
// (drop-oldest) and an ontology entity with no name is dropped. A rewrite that passes cliVerdictsIntact therefore publishes a
// tracker shorter than the stored one and loses those records for good. The hook's post-compact write already refuses this
// through state.RewriteKeepsInterview; these cases pin the same refusal on the cli writers, the orchestrate transition included.

// cliInterviewRefusal is the reason a cli writer gives when the rewrite would drop stored interview records. Each caller keeps
// its own refusal sentence frame (memory allow-write, evidence resolve, scan record, orchestrate) and takes this reason.
const cliInterviewRefusal = "session state holds interview records this command cannot rewrite without losing them; refusing to rewrite it"

// cliInterviewContradictions is a stored contradictions array of n valid records.
func cliInterviewContradictions(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"contradictionId":"k%d","severity":"high","summary":"s%d"}`, i, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// cliInterviewAssumptions is a stored assumptions array of n valid records.
func cliInterviewAssumptions(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"a%d","text":"t%d","recorded":true}`, i, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// cliInterviewStrings is a stored array of n strings, for the lists ReconstructInterview caps besides contradictions and
// assumptions: a dimension's known/unknown and an ontology entry's fields.
func cliInterviewStrings(prefix string, n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`"%s%d"`, prefix, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// cliInterviewStored is a session file holding the resolvable record a1/t1 and the given raw interview value. An empty
// interviewJSON omits the key entirely, so the file stores no tracker; "null" stores a null one.
func cliInterviewStored(t *testing.T, interviewJSON string) []byte {
	t.Helper()
	encoded, err := json.Marshal(cliVerdict("a1", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	var record any
	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"phase": "P", "unverifiedSubagents": []any{record}}
	if interviewJSON != "" {
		var value any
		if err := json.Unmarshal([]byte(interviewJSON), &value); err != nil {
			t.Fatal(err)
		}
		doc["interview"] = value
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// cliInterviewWorkspace is a temporary world: HOME, CODEX_HOME and CRW_HOME point inside it, so no run reaches the real Codex
// home, and the session file holds before.
func cliInterviewWorkspace(t *testing.T, sessionID string, before []byte) string {
	t.Helper()
	cwd := t.TempDir()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, filepath.Join(cwd, name))
	}
	cliPut(t, state.StatePath(cwd, sessionID), string(before))
	return cwd
}

// cliInterviewRecord is the stored resolvable record a1/t1 as raw JSON, so a test can build a whole document by hand.
func cliInterviewRecord(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(cliVerdict("a1", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cliInterviewRelationships is a stored ontology relationship array of n entries, each with a target.
func cliInterviewRelationships(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"to":"y%d","kind":"k"}`, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// cliInterviewWriter is one of the writers that check cliVerdictsIntact, ready to run against cwd.
func cliInterviewWriter(t *testing.T, name, cwd, sessionID string) func() (string, int) {
	t.Helper()
	cliPut(t, filepath.Join(cwd, ".crw/evidence/check.md"), "verified")
	switch name {
	case "memory allow-write":
		return func() (string, int) {
			return RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: sessionID, Cwd: cwd})
		}
	case "evidence resolve":
		return func() (string, int) {
			return RunEvidenceCLI(EvidenceResolveArgs{Verb: "resolve", SessionID: sessionID, AgentID: "a1",
				Receipt: ".crw/evidence/check.md", Cwd: cwd})
		}
	case "scan record":
		return func() (string, int) {
			parsed := ParseScanCliArgs([]string{"record", "--session", sessionID}, cwd)
			if parsed.Error != "" {
				t.Fatal(parsed.Error)
			}
			res := RunScanCli(*parsed.Args)
			return res.Output, res.Code
		}
	case "orchestrate reset":
		return func() (string, int) {
			got := orchestrateTransitionRun(t, cwd, "reset", "--session", sessionID)
			return got.Output, got.Code
		}
	case "orchestrate P>A":
		return func() (string, int) {
			unit := orchestrateTransitionSeedPlanUnit(t, cwd)
			got := orchestrateTransitionRun(t, cwd, "A", "--session", sessionID,
				"--attest", `{"from":"P","to":"A","did":"audited","planUnit":"`+unit+`"}`)
			return got.Output, got.Code
		}
	}
	t.Fatalf("unknown writer %q", name)
	return nil
}

// cliInterviewWriters is the writers that share cliVerdictsIntact, in the order the issue lists them plus the two orchestrate
// reproductions the coordinator added.
func cliInterviewWriters() []string {
	return []string{"memory allow-write", "evidence resolve", "scan record", "orchestrate reset", "orchestrate P>A"}
}

// TestCLIRefusesARewriteThatWouldDropInterviewRecords is the red-first half: a stored tracker the reader would cut or drop makes
// every writer refuse, exit 1, and leave the session file byte for byte as it was. On the baseline each writer succeeds and
// publishes the truncated rebuild.
func TestCLIRefusesARewriteThatWouldDropInterviewRecords(t *testing.T) {
	lossy := []struct{ name, interviewJSON string }{
		{name: "contradictions one past the cap", interviewJSON: `{"contradictions":` + cliInterviewContradictions(interview.MaxTrackerArray+1) + `,"assumptions":[]}`},
		{name: "assumptions one past the cap", interviewJSON: `{"contradictions":[],"assumptions":` + cliInterviewAssumptions(interview.MaxTrackerArray+1) + `}`},
		{name: "an unnamed ontology entry", interviewJSON: `{"contradictions":[],"assumptions":[],"ontologySchema":[{"fields":["f"],"relationships":[{"to":"y","kind":"k"}]}]}`},
		{name: "a dimension known list past the cap", interviewJSON: `{"contradictions":[],"assumptions":[],"dimensions":{"goal":{"known":` + cliInterviewStrings("k", interview.MaxTrackerArray+1) + `}}}`},
		{name: "a dimension unknown list past the cap", interviewJSON: `{"contradictions":[],"assumptions":[],"dimensions":{"goal":{"unknown":` + cliInterviewStrings("u", interview.MaxTrackerArray+1) + `}}}`},
		{name: "an ontology fields list past the cap", interviewJSON: `{"contradictions":[],"assumptions":[],"ontologySchema":[{"name":"n","fields":` + cliInterviewStrings("f", interview.MaxTrackerArray+1) + `}]}`},
		{name: "an ontology relationship list past the cap", interviewJSON: `{"contradictions":[],"assumptions":[],"ontologySchema":[{"name":"n","relationships":` + cliInterviewRelationships(interview.MaxTrackerArray+1) + `}]}`},
	}
	for _, c := range lossy {
		t.Run(c.name, func(t *testing.T) {
			for _, writer := range cliInterviewWriters() {
				t.Run(writer, func(t *testing.T) {
					before := cliInterviewStored(t, c.interviewJSON)
					cwd := cliInterviewWorkspace(t, "s1", before)
					before, err := os.ReadFile(state.StatePath(cwd, "s1"))
					if err != nil {
						t.Fatal(err)
					}
					out, code := cliInterviewWriter(t, writer, cwd, "s1")()
					if code != 1 || !strings.Contains(out, cliInterviewRefusal) {
						t.Fatalf("got %d %s", code, out)
					}
					after, err := os.ReadFile(state.StatePath(cwd, "s1"))
					if err != nil {
						t.Fatal(err)
					}
					if string(after) != string(before) {
						t.Fatalf("the refused write changed the session file:\n%s", after)
					}
				})
			}
		})
	}
}

// TestCLIRefusesARewriteWhenADuplicateInterviewKeyHoldsTheLoss is the duplicate-key case: the session reader keeps the LAST
// top-level interview value, so the guard must judge that one. A first null followed by a tracker one past the cap is the
// document a rewrite would truncate.
func TestCLIRefusesARewriteWhenADuplicateInterviewKeyHoldsTheLoss(t *testing.T) {
	for _, writer := range cliInterviewWriters() {
		t.Run(writer, func(t *testing.T) {
			raw := `{"phase":"P","unverifiedSubagents":[` + cliInterviewRecord(t) + `],"interview":null,"interview":{"contradictions":[],"assumptions":` + cliInterviewAssumptions(interview.MaxTrackerArray+1) + `}}`
			cwd := cliInterviewWorkspace(t, "s1", []byte(raw))
			before, err := os.ReadFile(state.StatePath(cwd, "s1"))
			if err != nil {
				t.Fatal(err)
			}
			out, code := cliInterviewWriter(t, writer, cwd, "s1")()
			if code != 1 || !strings.Contains(out, cliInterviewRefusal) {
				t.Fatalf("got %d %s", code, out)
			}
			after, err := os.ReadFile(state.StatePath(cwd, "s1"))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("the refused write changed the session file:\n%s", after)
			}
		})
	}
}

// TestCLIWritesAnInterviewTrackerItCanKeep is the other half: a tracker the reader keeps whole, an absent tracker and a null
// tracker all still write, so the guard refuses only a real loss.
func TestCLIWritesAnInterviewTrackerItCanKeep(t *testing.T) {
	keepable := []struct{ name, interviewJSON string }{
		{"a tracker within the cap", `{"contradictions":` + cliInterviewContradictions(2) + `,"assumptions":` + cliInterviewAssumptions(1) + `,"ontologySchema":[{"name":"n","relationships":[{"to":"y","kind":"k"}]}]}`},
		{"no tracker", ""},
		{"a null tracker", "null"},
	}
	for _, c := range keepable {
		t.Run(c.name, func(t *testing.T) {
			for _, writer := range cliInterviewWriters() {
				t.Run(writer, func(t *testing.T) {
					cwd := cliInterviewWorkspace(t, "s1", cliInterviewStored(t, c.interviewJSON))
					out, code := cliInterviewWriter(t, writer, cwd, "s1")()
					if code != 0 {
						t.Fatalf("got %d %s", code, out)
					}
				})
			}
		})
	}
}
