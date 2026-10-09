// prompt_dclose_ledger_test.go pins the two readings of the chat D-close's idempotence guards that
// decide whether a completion ledger row is written (CRW-1073): a key the stored row does not have
// is not JSON null (hook.ts:639-643 compares with ===, so undefined !== null), and a lone surrogate
// escape in a stored row is not the U+FFFD a current id holds (JSON.parse keeps it). Both
// losses are permanent: the row the close owed is skipped, the session rests in IDLE, and the same D
// request is then refused as IDLE->D.
package hook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseLedgerLines is a JSONL file as its non-empty raw lines.
func promptDcloseLedgerLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// promptDcloseLedgerTimestamp is the ts member of a row the close appended; the oracle's own rows
// differ from the port's only in it.
var promptDcloseLedgerTimestamp = regexp.MustCompile(`^\{"ts":"[0-9T:.\-]+Z",`)

// promptDcloseWithoutTimestamp is an appended ledger line without its ts, so the line can be
// compared with the oracle's byte for byte (/scratch/crw/oracle-review/notes/u02-oracle-repro.log).
func promptDcloseWithoutTimestamp(line string) string {
	return promptDcloseLedgerTimestamp.ReplaceAllString(line, "{")
}

// promptDcloseAllDoneRepo is a bound plan whose only work phase is done, the session in C with the
// epoch, and the receipt the close needs.
func promptDcloseAllDoneRepo(t *testing.T, slug, epoch string) (cwd, attest string) {
	t.Helper()
	cwd = promptDcloseRepo(t)
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "close an all-done chat cycle"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, epoch)
	return cwd, promptDcloseAttest("wp-finished", promptDcloseReceipt(t, cwd, "s1", epoch))
}

// TestPromptDcloseAllDoneKeyAbsentRowIsNotTheNullCloseRow is the U02-F01 case: a ledger row that has
// no closedWorkPhaseId key does not match the all-done close, whose key is null, so the close
// appends its own row (once) and keeps the old one. An explicit null row is the close already done.
func TestPromptDcloseAllDoneKeyAbsentRowIsNotTheNullCloseRow(t *testing.T) {
	const epoch = "c-u02"
	cases := []struct {
		name     string
		seed     string
		wantRows int
	}{
		{"absent closedWorkPhaseId key", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-u02"}`, 2},
		{"absent checkEpoch key with null closedWorkPhaseId", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","closedWorkPhaseId":null}`, 2},
		{"explicit null closedWorkPhaseId", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-u02","closedWorkPhaseId":null}`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, attest := promptDcloseAllDoneRepo(t, "chat-all-done-key", epoch)
			path := promptDclosePabcdLedgerPath(cwd)
			promptDcloseWrite(t, cwd, filepath.ToSlash(strings.TrimPrefix(path, cwd+string(filepath.Separator))), c.seed+"\n")
			answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
			if !strings.Contains(answer, "[crw: DONE]") {
				t.Fatalf("the close did not finish: %q", answer)
			}
			if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
				t.Errorf("the resting phase: %+v", s)
			}
			lines := promptDcloseLedgerLines(t, path)
			if len(lines) != c.wantRows {
				t.Fatalf("ledger lines = %d, want %d: %q", len(lines), c.wantRows, lines)
			}
			if lines[0] != c.seed {
				t.Errorf("the stored row changed: %q", lines[0])
			}
			if c.wantRows == 2 {
				// The oracle's dist appends exactly this line (ts aside, and the evidence the attest names) after the same seed.
				const oracle = `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","evidence":"ran the suite","checkEpoch":"c-u02","closedWorkPhaseId":null}`
				if got := promptDcloseWithoutTimestamp(lines[1]); got != oracle {
					t.Errorf("the appended line differs from the oracle's:\n got %q\nwant %q", got, oracle)
				}
				rows := promptOrchestrateLedger(t, cwd)
				added := rows[1]
				closed, has := added["closedWorkPhaseId"]
				if !has || closed != nil || added["checkEpoch"] != epoch || added["to"] != "IDLE" || added["from"] != "C" || added["reason"] != "done" {
					t.Errorf("the appended row: %+v", added)
				}
			}
			// The same close again, from the same session state, adds nothing.
			promptDcloseSeedState(t, cwd, "s1", "chat-all-done-key", epoch)
			if again := promptDcloseRun(t, cwd, "s1", "t2", attest); !strings.Contains(again, "[crw: DONE]") {
				t.Fatalf("the repeated close did not finish: %q", again)
			}
			if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
				t.Errorf("the resting phase after the repeated close: %+v", s)
			}
			if again := promptDcloseLedgerLines(t, path); len(again) != c.wantRows {
				t.Errorf("a repeated close changed the ledger: %q", again)
			}
		})
	}
}

// TestPromptDcloseCloseRowKeyAbsenceIsNotNull drives the matcher itself for the check epoch, where
// a null key cannot come from a close (a close without an epoch is refused).
func TestPromptDcloseCloseRowKeyAbsenceIsNotNull(t *testing.T) {
	cases := []struct {
		name  string
		row   string
		epoch *string
		want  bool
	}{
		{"absent checkEpoch is not null", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","closedWorkPhaseId":null}`, nil, false},
		{"explicit null checkEpoch is null", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":null,"closedWorkPhaseId":null}`, nil, true},
		{"absent checkEpoch is not a string", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","closedWorkPhaseId":null}`, promptDcloseStr("c"), false},
		{"absent closedWorkPhaseId is not null", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c"}`, promptDcloseStr("c"), false},
		{"explicit null closedWorkPhaseId", `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c","closedWorkPhaseId":null}`, promptDcloseStr("c"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			promptDcloseWrite(t, cwd, filepath.ToSlash(strings.TrimPrefix(promptDclosePabcdLedgerPath(cwd), cwd+string(filepath.Separator))), c.row+"\n")
			got, err := promptDcloseHasPabcdCloseRow(cwd, "s1", c.epoch, "")
			if err != nil || got != c.want {
				t.Errorf("promptDcloseHasPabcdCloseRow = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// TestPromptDcloseLoneSurrogateRowIsNotTheReplacementCharacterRow is the U02-F02 case: the ledgers
// hold "wp-\ud800" (a lone surrogate JSON.parse keeps) and the current id holds the real U+FFFD, so
// neither stored row is this close's row and both are written. A stored row that holds the
// real U+FFFD (as an escape or as the character) is the row, and nothing is added.
func TestPromptDcloseLoneSurrogateRowIsNotTheReplacementCharacterRow(t *testing.T) {
	const id = "wp-�"
	cases := []struct {
		name         string
		stored       string // the id as it is written in both stored rows
		wantGoalplan int
		wantPabcd    int
	}{
		{"lone surrogate escape", `wp-\ud800`, 2, 2},
		{"U+FFFD escape", `wp-\ufffd`, 1, 1},
		{"U+FFFD character", "wp-�", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			slug := "chat-surrogate"
			plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "chat dclose " + slug})
			plan.Slug = slug
			plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: id, Title: "first", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
			plan.ActiveWorkPhaseID = promptDcloseStr(id)
			if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
				t.Fatal(err)
			}
			promptDcloseSeedState(t, cwd, "s1", slug, "c-u02")
			attest := promptDcloseAttest(id, promptDcloseReceipt(t, cwd, "s1", "c-u02"))
			goalplanSeed := `{"event":"workphase_done","detail":"closed ` + c.stored + `"}`
			pabcdSeed := `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","checkEpoch":"c-u02","closedWorkPhaseId":"` + c.stored + `"}`
			gpPath := promptDcloseGoalplanLedgerPath(t, cwd, slug)
			pPath := promptDclosePabcdLedgerPath(cwd)
			for path, seed := range map[string]string{gpPath: goalplanSeed, pPath: pabcdSeed} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(seed+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
			if !strings.Contains(answer, "[crw: DONE]") {
				t.Fatalf("the close did not finish: %q", answer)
			}
			gp := promptDcloseLedgerLines(t, gpPath)
			pl := promptDcloseLedgerLines(t, pPath)
			if len(gp) != c.wantGoalplan || len(pl) != c.wantPabcd {
				t.Fatalf("goalplan lines %d (want %d): %q\npabcd lines %d (want %d): %q", len(gp), c.wantGoalplan, gp, len(pl), c.wantPabcd, pl)
			}
			if gp[0] != goalplanSeed || pl[0] != pabcdSeed {
				t.Errorf("a stored row changed:\n%q\n%q", gp[0], pl[0])
			}
			if c.wantPabcd == 2 {
				// The oracle's dist appends exactly these lines (ts aside, and the evidence the attest names) after the same seeds.
				wantGoalplan := `{"slug":"chat-surrogate","event":"workphase_done","detail":"closed wp-` + "\uFFFD" + `"}`
				wantPabcd := `{"sessionId":"s1","from":"C","to":"IDLE","reason":"done","evidence":"ran the suite","checkEpoch":"c-u02","closedWorkPhaseId":"wp-` + "\uFFFD" + `"}`
				if got := promptDcloseWithoutTimestamp(gp[1]); got != wantGoalplan {
					t.Errorf("the appended goalplan line:\n got %q\nwant %q", got, wantGoalplan)
				}
				if got := promptDcloseWithoutTimestamp(pl[1]); got != wantPabcd {
					t.Errorf("the appended PABCD line:\n got %q\nwant %q", got, wantPabcd)
				}
			}
		})
	}
}
