package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-815: the cli writers that rewrite the whole session state now ask the one shared judgement
// (state.RewriteKeepsStored) instead of two separate checks. CRW-786 already made all of them refuse a
// stored interview tracker longer than interview.MaxTrackerArray, so these cases pin that behaviour
// across the change: a tracker past the cap is refused, exit 1, and the file stays byte for byte.

// rewriteStoredCLITracker is a stored contradictions array of n valid records.
func rewriteStoredCLITracker(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"contradictionId":"k%d","severity":"high","summary":"s%d"}`, i, i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// rewriteStoredCLIBody is a session file at phase holding the resolvable record a1/t1 and a stored tracker
// one past the cap, so every writer under test has work to do.
func rewriteStoredCLIBody(phase string) string {
	return `{"phase":"` + phase + `","sessionId":"s1","interview":{"contradictions":` +
		rewriteStoredCLITracker(interview.MaxTrackerArray+1) + `,"assumptions":[]},"unverifiedSubagents":[` +
		`{"agentId":"a1","turnId":"t1","agentType":"worker","attempts":3,"receiptClaimed":"none","recordedAt":"recorded","resolvable":true}]}`
}

// rewriteStoredCLIWorkspace is a temporary world whose session file holds body.
func rewriteStoredCLIWorkspace(t *testing.T, sessionID, body string) string {
	t.Helper()
	cwd := t.TempDir()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, filepath.Join(cwd, name))
	}
	cliPut(t, state.StatePath(cwd, sessionID), body)
	cliPut(t, filepath.Join(cwd, ".crw/evidence/check.md"), "verified")
	return cwd
}

// TestCLIRefusesARewriteThatWouldDropALongInterviewTracker pins every cli writer against the shared
// judgement: a tracker past the cap is refused with the interview sentence and the file is unchanged.
func TestCLIRefusesARewriteThatWouldDropALongInterviewTracker(t *testing.T) {
	writers := []struct {
		name  string
		phase string
		run   func(t *testing.T, cwd, sessionID string) (string, int)
	}{
		{"memory allow-write", "P", func(_ *testing.T, cwd, sessionID string) (string, int) {
			return RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: sessionID, Cwd: cwd})
		}},
		{"evidence resolve", "P", func(_ *testing.T, cwd, sessionID string) (string, int) {
			return RunEvidenceCLI(EvidenceResolveArgs{Verb: "resolve", SessionID: sessionID, AgentID: "a1",
				Receipt: ".crw/evidence/check.md", Cwd: cwd})
		}},
		{"scan record", "P", func(t *testing.T, cwd, sessionID string) (string, int) {
			parsed := ParseScanCliArgs([]string{"record", "--session", sessionID}, cwd)
			if parsed.Error != "" {
				t.Fatal(parsed.Error)
			}
			res := RunScanCli(*parsed.Args)
			return res.Output, res.Code
		}},
		{"orchestrate reset", "P", func(t *testing.T, cwd, sessionID string) (string, int) {
			got := orchestrateTransitionRun(t, cwd, "reset", "--session", sessionID)
			return got.Output, got.Code
		}},
		{"orchestrate interview override", "I", func(t *testing.T, cwd, sessionID string) (string, int) {
			got := orchestrateTransitionRun(t, cwd, "P", "--session", sessionID,
				"--attest", `{"from":"I","to":"P","did":"interview done","override":true}`)
			return got.Output, got.Code
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			cwd := rewriteStoredCLIWorkspace(t, "s1", rewriteStoredCLIBody(w.phase))
			before, err := os.ReadFile(state.StatePath(cwd, "s1"))
			if err != nil {
				t.Fatal(err)
			}
			out, code := w.run(t, cwd, "s1")
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
