package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// B-class scan-cli.test.ts:84-169,196-338,359-488 and
// interview-readiness.test.ts:61-167, plus independently recorded edge cases.
func TestScanRecordOracle(t *testing.T) {
	data := scanRecordRead(t, "testdata/scan_record/oracle.json")
	var cases []struct {
		ID, Classification, Reason string
		Actions                    []struct {
			Kind, ID, Question, Turn string
			Argv, Answers            []string
			Value, Row               json.RawMessage
		}
		Expect json.RawMessage
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 30 {
		t.Fatal("recorded cases missing")
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			cwd := scanRecordWorkspace(t)
			if c.Classification == "intentionally-changed" {
				if c.Reason == "" {
					t.Fatal("missing parity reason")
				}
				original := []byte(`{"phase":"I","interview":`)
				if c.ID == "unverified-record-data-loss" {
					s := state.DefaultState("s1", "")
					encoded, _ := state.Encode(s)
					original = bytes.Replace(encoded, []byte(`"unverifiedSubagents": []`), []byte(`"unverifiedSubagents": [null]`), 1)
				}
				scanRecordPut(t, state.StatePath(cwd, "s1"), original)
				ledgerPath := filepath.Join(cwd, ".crw", "interviews", "s1.jsonl")
				scanRecordPut(t, ledgerPath, []byte("prior record\n"))
				res := scanRecordInvoke(t, cwd)
				if res.Code != 1 || !strings.Contains(res.Output, "refusing") {
					t.Fatalf("refusal: %+v", res)
				}
				if !bytes.Equal(scanRecordRead(t, state.StatePath(cwd, "s1")), original) || string(scanRecordRead(t, ledgerPath)) != "prior record\n" {
					t.Fatal("existing bytes lost")
				}
				return
			}
			results := []CliResult{}
			for _, a := range c.Actions {
				switch a.Kind {
				case "scan":
					results = append(results, scanRecordInvoke(t, cwd, a.Argv...))
				case "help":
					results = append(results, RunScanCli(*ParseScanCliArgs([]string{"help"}, cwd).Args))
				case "qa":
					ledger.CaptureInterviewAnswers(ledger.CaptureInput{Cwd: cwd, SessionID: "s1", TurnID: a.Turn,
						ToolInput:    map[string]any{"questions": []any{map[string]any{"id": a.ID, "question": a.Question}}},
						ToolResponse: map[string]any{"answers": map[string]any{a.ID: map[string]any{"answers": scanRecordTestAny(a.Answers)}}}})
				case "raw":
					p := filepath.Join(cwd, ".crw", "interviews", "s1.jsonl")
					if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
					if err != nil {
						t.Fatal(err)
					}
					var compact bytes.Buffer
					if err := json.Compact(&compact, a.Row); err != nil {
						t.Fatal(err)
					}
					_, err = f.Write(append(compact.Bytes(), '\n'))
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						t.Fatalf("append: %v %v", err, closeErr)
					}
				case "patch":
					s := state.ReadState(cwd, "s1")
					encoded, _ := json.Marshal(s.Interview)
					var fields, patch map[string]any
					if err := json.Unmarshal(encoded, &fields); err != nil {
						t.Fatal(err)
					}
					if fields == nil {
						t.Fatal("previous scan did not create the tracker")
					}
					if err := json.Unmarshal(a.Value, &patch); err != nil {
						t.Fatal(err)
					}
					for key, value := range patch {
						fields[key] = value
					}
					s.Interview = interview.ReconstructInterview(fields)
					if err := state.WriteState(cwd, s); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unknown action %s", a.Kind)
				}
			}
			rows := []map[string]any{}
			orders := [][]string{}
			for _, e := range state.ReadInterviewEvents(cwd, "s1") {
				var row map[string]any
				if err := json.Unmarshal(e.Raw, &row); err != nil {
					t.Fatal(err)
				}
				delete(row, "ts")
				rows = append(rows, row)
				order := []string{}
				for _, pair := range e.Map {
					order = append(order, pair.QuestionID)
				}
				orders = append(orders, order)
			}
			tracker := state.ReadState(cwd, "s1").Interview
			got := map[string]any{"results": results, "tracker": tracker, "rows": rows, "mapOrders": orders,
				"gate":      interview.EvaluateInterviewGate(tracker, &interview.GateEvidence{BackedDimensions: ledger.DimensionsBackedByAnswers(cwd, "s1")}),
				"shapeGate": interview.EvaluateInterviewGate(tracker, nil)}
			actual, _ := json.Marshal(got)
			var have, want any
			if err := json.Unmarshal(actual, &have); err != nil {
				t.Fatal(err)
			}
			expected := strings.NewReplacer("cxc scan", "crw pabcd scan", ".codexclaw", ".crw").Replace(string(c.Expect))
			if err := json.Unmarshal([]byte(expected), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Fatalf("oracle mismatch\ngot: %s\nwant: %s", actual, expected)
			}
		})
	}
}

func TestScanRecordWriteFailures(t *testing.T) {
	for _, which := range []string{"append", "state"} {
		t.Run(which, func(t *testing.T) {
			cwd := scanRecordWorkspace(t)
			if r := scanRecordInvoke(t, cwd, "--known", "goal=prior"); r.Code != 0 {
				t.Fatal(r)
			}
			p := state.StatePath(cwd, "s1")
			lp := filepath.Join(cwd, ".crw", "interviews", "s1.jsonl")
			oldState, oldLedger := scanRecordRead(t, p), scanRecordRead(t, lp)
			a := *ParseScanCliArgs([]string{"record", "--session", "s1"}, cwd).Args
			appendEvent := state.AppendInterviewEvent
			if which == "append" {
				if err := os.Chmod(lp, 0o400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(lp, 0o600) })
			} else {
				dir := filepath.Dir(p)
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				appendEvent = func(cwd string, e state.InterviewEvent) error {
					if _, err := os.Stat(p + ".lock"); err != nil {
						return fmt.Errorf("writer is outside session lock: %w", err)
					}
					if err := state.AppendInterviewEvent(cwd, e); err != nil {
						return err
					}
					return os.Chmod(dir, 0o500)
				}
			}
			r := scanRecordRun(a, appendEvent)
			if r.Code != 1 || !strings.HasPrefix(r.Output, "scan record failed:") {
				t.Fatalf("failure not surfaced: %+v", r)
			}
			if which == "state" {
				if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(scanRecordRead(t, p), oldState) {
				t.Fatal("failed write replaced old state")
			}
			newLedger := scanRecordRead(t, lp)
			if !bytes.HasPrefix(newLedger, oldLedger) {
				t.Fatal("failed write changed earlier ledger rows")
			}
			if which == "append" && !bytes.Equal(newLedger, oldLedger) {
				t.Fatal("failed append wrote a row")
			}
			if which == "state" && len(state.ReadInterviewEvents(cwd, "s1")) != 2 {
				t.Fatal("second-write failure must retain appended row")
			}
		})
	}
}

func TestScanRecordLockAndConcurrentUpdates(t *testing.T) {
	var oracle struct {
		Classification, Reason string
		Oracle                 struct {
			ScanRounds, KnownCount, Rows int
			RoundIDs                     []int
		}
	}
	if err := json.Unmarshal(scanRecordRead(t, "testdata/scan_record/unlocked.json"), &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.Classification != "intentionally-changed" || oracle.Reason == "" || oracle.Oracle.ScanRounds != 1 || oracle.Oracle.KnownCount != 1 || oracle.Oracle.Rows != 2 || !reflect.DeepEqual(oracle.Oracle.RoundIDs, []int{1, 1}) {
		t.Fatal("missing recorded oracle data-loss case")
	}
	cwd := scanRecordWorkspace(t)
	if r := scanRecordInvoke(t, cwd); r.Code != 0 {
		t.Fatal(r)
	}
	p := state.StatePath(cwd, "s1")
	old := scanRecordRead(t, p)
	scanRecordPut(t, p+".lock", []byte("held"))
	if r := scanRecordInvoke(t, cwd); r.Code != 1 || !strings.Contains(r.Output, "EEXIST") {
		t.Fatal(r)
	}
	if !bytes.Equal(scanRecordRead(t, p), old) || len(state.ReadInterviewEvents(cwd, "s1")) != 1 {
		t.Fatal("held lock changed records")
	}
	if err := os.Remove(p + ".lock"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan CliResult, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := ScanCliArgs{Action: ScanActionRecord, SessionID: "s1", Cwd: cwd, Known: []ScanDimensionText{{Dimension: interview.DimensionGoal, Text: fmt.Sprintf("fact-%d", i)}}}
			results <- RunScanCli(a)
		}(i)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.Code != 0 {
			t.Fatal(r)
		}
	}
	s := state.ReadState(cwd, "s1")
	if s.Interview.ScanRounds != 5 || s.Interview.LastScanRoundID != 5 || len(s.Interview.Dimensions.Goal.Known) != 4 {
		t.Fatal("concurrent update lost")
	}
	for i, e := range state.ReadInterviewEvents(cwd, "s1") {
		if e.RoundID != float64(i+1) {
			t.Fatal("nonmonotonic scan ledger")
		}
	}
}

func scanRecordWorkspace(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		p := filepath.Join(cwd, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, p)
	}
	return cwd
}
func scanRecordInvoke(t *testing.T, cwd string, argv ...string) CliResult {
	t.Helper()
	p := ParseScanCliArgs(append([]string{"record", "--session", "s1"}, argv...), cwd)
	if p.Error != "" {
		t.Fatal(p.Error)
	}
	return RunScanCli(*p.Args)
}
func scanRecordTestAny(values []string) []any {
	out := []any{}
	for _, s := range values {
		out = append(out, s)
	}
	return out
}
func scanRecordPut(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
func scanRecordRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
