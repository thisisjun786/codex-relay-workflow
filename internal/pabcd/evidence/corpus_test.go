package evidence

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The attempt files and the tombstone the CXC v0.2.40 oracle left in the expect.tree of the SubagentStop fixtures of
// contract/fixtures/cxc are goldens. The hook leg that produced them is not ported here, so this test replays the oracle's gate
// (subagent-evidence.ts:476-531) from the units of this package: the same payloads, the same given tree under .crw, and the
// same decisions and files. It is parity evidence for the units; it is not a replayer claim on those fixtures.

type fixture struct {
	Given struct{ Files, Symlinks map[string]string }
	Run   struct {
		Steps []struct{ Stdin map[string]any }
	}
	Expect struct {
		Steps []map[string]any
		Tree  map[string]struct {
			Type string
			JSON json.RawMessage
		}
	}
}

// gate is the oracle's runSubagentStopGate after its two scoping checks, for an executor: the decision it prints.
func gate(cwd, sessionID string, p Payload) string {
	if receipt, ok := ExtractReceiptPath(p.LastAssistantMessage); ok && HasValidReceipt(cwd, receipt) {
		ClearAttempts(cwd, sessionID, p.AgentID, p.TurnID)
		return ""
	}
	if HasTombstone(cwd, sessionID, p) {
		return ""
	}
	attempts := ReadAttempts(cwd, sessionID, p.AgentID, p.TurnID)
	if attempts >= MaxAttempts {
		RecordTombstone(cwd, sessionID, p, attempts, nil)
		return ""
	}
	if !WriteAttempts(cwd, sessionID, p.AgentID, attempts+1, p.TurnID) {
		RecordTombstone(cwd, sessionID, p, attempts, nil)
		return ""
	}
	return "block"
}

func TestCorpusFixtureTrees(t *testing.T) {
	names := []string{"context_pressure_in_message_still_blocks", "context_pressure_releases", "empty_receipt_blocks", "receipt_outside_evidence_root_blocks",
		"symlinked_receipt_blocks", "three_attempts_then_release", "valid_receipt_passes"}
	isoTime := regexp.MustCompile("^\\d{4}-\\d\\d-\\d\\dT\\d\\d:\\d\\d:\\d\\d\\.\\d{3}Z$")
	for _, name := range names {
		raw, err := os.ReadFile("../../../contract/fixtures/cxc/hook__subagent-stop-verifying-evidence__" + name + ".json")
		must(t, err)
		var fx fixture
		must(t, json.Unmarshal(raw, &fx))
		cwd := t.TempDir()
		rename := strings.NewReplacer("ws/", "", ".codexclaw", ".crw", "$"+"{WS}", cwd)
		for path, content := range fx.Given.Files {
			put(t, filepath.Join(cwd, rename.Replace(path)), []byte(content))
		}
		for path, target := range fx.Given.Symlinks {
			must(t, os.MkdirAll(filepath.Dir(filepath.Join(cwd, rename.Replace(path))), 0o777))
			must(t, os.Symlink(rename.Replace(target), filepath.Join(cwd, rename.Replace(path))))
		}
		for i, step := range fx.Run.Steps {
			str := func(key string) string { s, _ := step.Stdin[key].(string); return rename.Replace(s) }
			p := Payload{AgentType: str("agent_type"), AgentID: str("agent_id"), TurnID: str("turn_id"), LastAssistantMessage: str("last_assistant_message")}
			want := map[string]string{"json": "block", "empty": ""}[fx.Expect.Steps[i]["stdout_form"].(string)]
			if got := gate(cwd, str("session_id"), p); got != want {
				t.Errorf("%s step %d: decision %q, want %q", name, i, got, want)
			}
		}
		wantFiles := []string{}
		for path, entry := range fx.Expect.Tree {
			dir, file := filepath.Split(path)
			if dir != "ws/.codexclaw/evidence-attempts/" {
				continue
			}
			var compact bytes.Buffer
			must(t, json.Compact(&compact, entry.JSON))
			got, err := os.ReadFile(filepath.Join(cwd, ".crw", AttemptsSubdir, file))
			if err != nil || string(got) != compact.String()+"\n" {
				t.Errorf("%s: %s is %q (%v), want %q", name, file, got, err, compact.String()+"\n")
			}
			wantFiles = append(wantFiles, file)
		}
		slices.Sort(wantFiles)
		if got := attemptsDir(cwd); !slices.Equal(got, wantFiles) && len(got)+len(wantFiles) > 0 {
			t.Errorf("%s: counter files %v, want %v", name, got, wantFiles)
		}
		if entry, ok := fx.Expect.Tree["ws/.codexclaw/sessions/rec-s1.json"]; ok && entry.Type == "file" {
			var wantState struct{ UnverifiedSubagents []map[string]any }
			must(t, json.Unmarshal(entry.JSON, &wantState))
			got := state.ReadState(cwd, "rec-s1").UnverifiedSubagents
			if len(got) != len(wantState.UnverifiedSubagents) {
				t.Fatalf("%s: %d tombstones, want %d", name, len(got), len(wantState.UnverifiedSubagents))
			}
			for i, w := range wantState.UnverifiedSubagents {
				e := got[i]
				if e.AgentID != w["agentId"] || e.TurnID != w["turnId"] || e.AgentType != w["agentType"] || e.Attempts != w["attempts"] || e.ReceiptClaimed != w["receiptClaimed"] || e.Resolvable != w["resolvable"] || !isoTime.MatchString(e.RecordedAt) {
					t.Errorf("%s: tombstone %+v, want %v", name, e, w)
				}
			}
		}
	}
}
