package ledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testdata/oracle-ledger.json holds what the CXC v0.2.40 oracle's interview-ledger.js answered for each case, recorded once by
// testdata/record-ledger.mjs under Node 24 (no Node runs here): ParseQuestions and ParseAnswers over payload shapes, the ledger
// text captureInterviewAnswers leaves, and ReadQaEvents and DimensionsBackedByAnswers over hand-written ledgers. The Go port
// must agree with every case, except the two the repaired append changes (changedCases). The second test replays the ledger
// half of the hook fixtures of contract/fixtures/cxc that only capture.

type oracleCase struct {
	ID, Kind                string
	ToolInput, ToolResponse json.RawMessage
	Answers                 map[string][]string
	Questions               []ParsedQuestion
	SessionID, TurnID       string
	Turns                   []string
	Rounds                  []struct{ ToolInput, ToolResponse json.RawMessage }
	PreLedger, Ledger       *string
	Written                 [][]string
	Files, EventIDs, Backed []string
}

// changedCases are the oracle cases that append to a final line without a line feed: the oracle joins the new row to it and
// loses both, the port keeps the old line and starts the new rows on their own (data loss, known-defects: port: fixed).
func changedCases() map[string]string {
	return map[string]string{
		"capture_unterminated_valid_row": "the oracle joins the new row to a final line without a line feed, so neither is read; the port writes a line feed first",
		"capture_unterminated_tail":      "the oracle joins the new row to a partial final line, so the row is lost; the port writes a line feed first",
	}
}

func decodeRaw(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func TestEveryRecordedOracleCaseAgrees(t *testing.T) {
	data, err := os.ReadFile("testdata/oracle-ledger.json")
	var cases []oracleCase
	if err != nil || json.Unmarshal(data, &cases) != nil || len(cases) != 105 {
		t.Fatalf("recorded cases: %d, %v", len(cases), err)
	}
	changed := 0
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			switch c.Kind {
			case "parse":
				if len(c.ToolResponse) > 0 {
					wantEq(t, "answers", ParseAnswers(decodeRaw(t, c.ToolResponse)), c.Answers)
				} else {
					wantEq(t, "questions", ParseQuestions(decodeRaw(t, c.ToolInput)), c.Questions)
				}
			case "capture":
				if _, ok := changedCases()[c.ID]; ok {
					changed++
				}
				replayCapture(t, c)
			case "read":
				cwd := t.TempDir()
				if c.Ledger != nil {
					writeLedger(t, cwd, "s", *c.Ledger)
				}
				wantEq(t, "event ids", ids(ReadQaEvents(cwd, "s")), c.EventIDs)
				backed := []string{}
				for d := range DimensionsBackedByAnswers(cwd, "s") {
					backed = append(backed, string(d))
				}
				sort.Strings(backed)
				wantEq(t, "backed", backed, c.Backed)
			default:
				t.Fatalf("unknown kind %q", c.Kind)
			}
		})
	}
	if changed != len(changedCases()) {
		t.Fatalf("replayed %d of the %d intentionally changed cases", changed, len(changedCases()))
	}
}

func replayCapture(t *testing.T, c oracleCase) {
	t.Helper()
	cwd := t.TempDir()
	if c.PreLedger != nil {
		writeLedger(t, cwd, c.SessionID, *c.PreLedger)
	}
	written := [][]string{}
	for i, r := range c.Rounds {
		turn := c.TurnID
		if c.Turns != nil {
			turn = c.Turns[i]
		}
		res := capture(CaptureInput{Cwd: cwd, SessionID: c.SessionID, TurnID: turn, ToolInput: decodeRaw(t, r.ToolInput), ToolResponse: decodeRaw(t, r.ToolResponse)}, fixed)
		written = append(written, ids(res.Written))
	}
	wantEq(t, "written", written, c.Written)
	var files []string
	entries, _ := os.ReadDir(filepath.Join(cwd, ".crw", "interviews"))
	for _, e := range entries {
		files = append(files, e.Name())
	}
	wantEq(t, "files", files, c.Files)
	if c.Ledger == nil {
		return
	}
	want := *c.Ledger
	if reason, ok := changedCases()[c.ID]; ok {
		t.Logf("intentionally changed: %s", reason)
		want = *c.PreLedger + "\n" + strings.TrimPrefix(want, *c.PreLedger)
	}
	wantEq(t, "ledger text", readLedger(t, cwd, c.SessionID), want)
}

func TestTheCapturingHookFixturesLeaveTheOraclesLedger(t *testing.T) {
	prefix := "hook__post-tool-use-capturing-interview-answers__"
	names := []string{"baseline_empty_workspace", "content_block_response", "partial_and_malformed_rounds", "phase_i_reinjects_rescan_directive",
		"replay_is_idempotent", "string_payloads_captured", "active_goal_captures_silently", "unreadable_goal_db_captures_silently"}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contract", "fixtures", "cxc", prefix+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Run struct {
				Steps []struct{ Stdin json.RawMessage }
			}
			Expect struct {
				Tree map[string]struct{ JSONL []map[string]any }
			}
		}
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		cwd := t.TempDir()
		quoted, _ := json.Marshal(cwd)
		for _, step := range fx.Run.Steps {
			in := decodeRaw(t, json.RawMessage(strings.ReplaceAll(string(step.Stdin), "\"$"+"{WS}\"", string(quoted)))).(map[string]any)
			session, _ := in["session_id"].(string)
			turn, _ := in["turn_id"].(string)
			capture(CaptureInput{Cwd: cwd, SessionID: session, TurnID: turn, ToolInput: in["tool_input"], ToolResponse: in["tool_response"]}, fixed)
		}
		got, want := map[string][]map[string]any{}, map[string][]map[string]any{}
		dir := filepath.Join(cwd, ".crw", "interviews")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			got[e.Name()] = rowsOf(t, filepath.Join(dir, e.Name()))
		}
		for path, e := range fx.Expect.Tree {
			if file, ok := strings.CutPrefix(path, "ws/.codexclaw/interviews/"); ok {
				want[file] = e.JSONL
			}
		}
		wantEq(t, name, got, want)
	}
}

// rowsOf reads a ledger as the fixture records it: each row an object, its ts masked.
func rowsOf(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("%s: %q: %v", path, line, err)
		}
		if _, ok := row["ts"].(string); ok {
			row["ts"] = "<TS>"
		}
		rows = append(rows, row)
	}
	return rows
}
