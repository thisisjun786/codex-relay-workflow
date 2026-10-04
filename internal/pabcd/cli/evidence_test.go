package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These answers were recorded twice from CXC v0.2.40, not derived from the Go code.
func TestCLIOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/oracle-cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Kind string
		Argv     []string
		Expect   json.RawMessage
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			cwd := t.TempDir()
			expected := cliNames(string(c.Expect))
			argv := make([]string, len(c.Argv))
			for i, s := range c.Argv {
				argv[i] = cliNames(s)
			}
			var got any
			switch c.Kind {
			case "evidence":
				a, err := ParseEvidenceCLIArgs(argv, "<WS>")
				if err != nil {
					got = map[string]any{"error": err.Error()}
				} else {
					m := map[string]any{"verb": a.Verb, "sessionId": a.SessionID, "agentId": a.AgentID, "receipt": a.Receipt, "cwd": a.Cwd}
					if a.TurnID != nil {
						m["turnId"] = *a.TurnID
					}
					got = m
				}
			case "memory":
				p := ParseMemoryCLIArgs(argv, "<WS>")
				if p.Error != "" {
					got = map[string]any{"error": p.Error}
				} else if p.Help {
					got = map[string]any{"help": true}
				} else {
					got = map[string]any{"verb": p.Args.Verb, "sessionId": p.Args.SessionID, "cwd": p.Args.Cwd}
				}
			case "usage":
				got = MemoryUsage
			case "run":
				got = runOracleCase(t, cwd, c.ID)
			default:
				t.Fatalf("unhandled oracle case %q", c.Kind)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			actual := strings.ReplaceAll(string(b), cwd, "<WS>")
			actual = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z`).ReplaceAllString(actual, "<TIME>")
			var want, have any
			if err := json.Unmarshal([]byte(expected), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(actual), &have); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Fatalf("oracle mismatch\ngot  %s\nwant %s", actual, expected)
			}
		})
	}
}

func cliNames(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, ".codexclaw", ".crw"), "cxc memory allow-write", "crw pabcd memory allow-write")
}

func runOracleCase(t *testing.T, cwd, id string) any {
	t.Helper()
	if id != "memory-new" {
		s := state.DefaultState("rec-s1", "keep")
		s.Phase, s.MemoryWriteRequested, s.StopBlockTotal = state.PhaseB, true, 7
		for _, turn := range []string{"t1", "t2", ""} {
			s.UnverifiedSubagents = append(s.UnverifiedSubagents, cliVerdict("a1", turn))
		}
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
	}
	receipt := ".crw/evidence/check.md"
	cliPut(t, filepath.Join(cwd, receipt), "verified")
	for _, turn := range []string{"t1", "t2", ""} {
		if !evidence.WriteAttempts(cwd, "rec-s1", "a1", 3, turn) {
			t.Fatal("could not seed attempts")
		}
	}
	a := EvidenceResolveArgs{Verb: "resolve", SessionID: "rec-s1", AgentID: "a1", Receipt: receipt, Cwd: cwd}
	if id == "evidence-no-match" {
		a.AgentID = "other"
	}
	if id == "evidence-resolve" {
		turn := "t1"
		a.TurnID = &turn
	}
	if id == "evidence-empty-turn" {
		turn := ""
		a.TurnID = &turn
	}
	var output string
	var code int
	if strings.HasPrefix(id, "memory") {
		output, code = RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
	} else {
		output, code = RunEvidenceCLI(a)
	}
	ledger := []any{}
	if b, err := os.ReadFile(filepath.Join(cwd, ".crw", "ledger.jsonl")); err == nil {
		for _, row := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var v any
			if err := json.Unmarshal([]byte(row), &v); err != nil {
				t.Fatal(err)
			}
			ledger = append(ledger, v)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return map[string]any{"result": map[string]any{"output": output, "code": code}, "state": state.ReadState(cwd, "rec-s1"),
		"counters": []int{evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1"), evidence.ReadAttempts(cwd, "rec-s1", "a1", "t2"), evidence.ReadAttempts(cwd, "rec-s1", "a1", "")}, "ledger": ledger}
}

func cliPut(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliVerdict(agent, turn string) state.UnverifiedSubagent {
	return state.UnverifiedSubagent{AgentID: agent, TurnID: turn, AgentType: "worker", Attempts: 3, ReceiptClaimed: "none", RecordedAt: "recorded", Resolvable: true}
}

func cliSeed(t *testing.T) (string, EvidenceResolveArgs, []byte) {
	t.Helper()
	cwd := t.TempDir()
	s := state.DefaultState("rec-s1", "keep")
	s.Phase = state.PhaseB
	s.UnverifiedSubagents = []state.UnverifiedSubagent{cliVerdict("a1", "t1")}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	cliPut(t, filepath.Join(cwd, ".crw/evidence/check.md"), "verified")
	if !evidence.WriteAttempts(cwd, "rec-s1", "a1", 3, "t1") {
		t.Fatal("seed counter failed")
	}
	b, err := os.ReadFile(state.StatePath(cwd, "rec-s1"))
	if err != nil {
		t.Fatal(err)
	}
	return cwd, EvidenceResolveArgs{Verb: "resolve", SessionID: "rec-s1", AgentID: "a1", Receipt: ".crw/evidence/check.md", Cwd: cwd}, b
}

func cliUnchanged(t *testing.T, cwd string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(state.StatePath(cwd, "rec-s1"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("state changed: err=%v, bytes=%s", err, after)
	}
}

func TestEvidenceReceiptGuard(t *testing.T) {
	for _, kind := range []string{"outside", "missing", "empty", "link", "directory", "absent"} {
		t.Run(kind, func(t *testing.T) {
			cwd, a, before := cliSeed(t)
			switch kind {
			case "outside":
				cliPut(t, filepath.Join(cwd, "outside.md"), "verified")
				a.Receipt = "outside.md"
			case "missing":
				a.Receipt = ".crw/evidence/missing.md"
			case "empty":
				cliPut(t, filepath.Join(cwd, a.Receipt), "")
			case "link":
				a.Receipt = ".crw/evidence/link.md"
				if err := os.Symlink("check.md", filepath.Join(cwd, a.Receipt)); err != nil {
					t.Fatal(err)
				}
			case "directory":
				a.Receipt = ".crw/evidence"
			case "absent":
				a.Receipt = ""
			}
			out, code := RunEvidenceCLI(a)
			if code != 1 || !strings.Contains(out, "evidence-root guard") {
				t.Fatalf("got %d %s", code, out)
			}
			cliUnchanged(t, cwd, before)
			if evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1") != 3 {
				t.Fatal("counter was cleared")
			}
			if _, err := os.Stat(filepath.Join(cwd, ".crw/ledger.jsonl")); !os.IsNotExist(err) {
				t.Fatal("guard wrote ledger")
			}
		})
	}
}

func TestEvidenceAuditFailureAndUnresolvable(t *testing.T) {
	for _, kind := range []string{"ledger-failure", "nonresolvable", "other-agent", "other-turn", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			cwd, a, before := cliSeed(t)
			switch kind {
			case "ledger-failure":
				if err := os.Mkdir(filepath.Join(cwd, ".crw/ledger.jsonl"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "nonresolvable":
				s := state.ReadState(cwd, "rec-s1")
				s.UnverifiedSubagents[0].Resolvable = false
				if err := state.WriteState(cwd, s); err != nil {
					t.Fatal(err)
				}
				before, _ = os.ReadFile(state.StatePath(cwd, "rec-s1"))
			case "other-agent":
				a.AgentID = "another"
			case "other-turn":
				turn := "different"
				a.TurnID = &turn
			case "unreadable":
				before = []byte("broken JSON")
				cliPut(t, state.StatePath(cwd, "rec-s1"), string(before))
			}
			out, code := RunEvidenceCLI(a)
			if code != 1 {
				t.Fatalf("unexpected success: %s", out)
			}
			cliUnchanged(t, cwd, before)
			if evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1") != 3 {
				t.Fatal("counter cleared on failure")
			}
		})
	}
}

// intentionally-changed: both oracle writes discard malformed/overflow verdict records.
func TestCLIDataLossPreservesVerdictBytes(t *testing.T) {
	for _, command := range []string{"evidence", "memory"} {
		for _, kind := range []string{"overflow", "malformed", "non-array"} {
			t.Run(command+"/"+kind, func(t *testing.T) {
				cwd, a, _ := cliSeed(t)
				var list any = []any{cliVerdict("a1", "t1"), map[string]any{"lost": "raw record"}}
				if kind == "overflow" {
					items := []state.UnverifiedSubagent{cliVerdict("a1", "t1")}
					for i := 1; i <= state.MaxUnverifiedSubagents; i++ {
						items = append(items, cliVerdict(fmt.Sprintf("other-%d", i), "t1"))
					}
					list = items
				}
				if kind == "non-array" {
					list = map[string]any{"lost": "record"}
				}
				before, err := json.Marshal(map[string]any{"phase": "B", "unverifiedSubagents": list})
				if err != nil {
					t.Fatal(err)
				}
				cliPut(t, state.StatePath(cwd, "rec-s1"), string(before))
				var out string
				var code int
				if command == "evidence" {
					out, code = RunEvidenceCLI(a)
				} else {
					out, code = RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
				}
				if code != 1 {
					t.Fatalf("data-loss refusal missing: %s", out)
				}
				cliUnchanged(t, cwd, before)
				if evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1") != 3 {
					t.Fatal("counter erased")
				}
				if _, err := os.Stat(filepath.Join(cwd, ".crw/ledger.jsonl")); !os.IsNotExist(err) {
					t.Fatal("loss guard wrote ledger")
				}
			})
		}
	}
}

func TestCLIHeldLock(t *testing.T) {
	for _, command := range []string{"evidence", "memory"} {
		t.Run(command, func(t *testing.T) {
			cwd, a, before := cliSeed(t)
			lock := state.StatePath(cwd, "rec-s1") + ".lock"
			cliPut(t, lock, "held")
			var out string
			var code int
			if command == "evidence" {
				out, code = RunEvidenceCLI(a)
			} else {
				out, code = RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
			}
			if code != 1 || !strings.Contains(out, "EEXIST: file already exists, open '"+lock+"'") {
				t.Fatalf("lock diagnostic: %d %s", code, out)
			}
			cliUnchanged(t, cwd, before)
			if b, err := os.ReadFile(lock); err != nil || string(b) != "held" {
				t.Fatal("foreign lock was altered")
			}
		})
	}
}
