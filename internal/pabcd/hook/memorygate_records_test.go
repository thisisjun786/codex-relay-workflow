package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// memoryRecordsSeed writes a session file that holds a spendable authorization (the CLI grant, or the marker for turn t1)
// and the given unverifiedSubagents list as the raw text of the file, and returns the file's bytes.
func memoryRecordsSeed(t *testing.T, cwd, kind string, list any) []byte {
	t.Helper()
	gateSeed(t, cwd, func(s *state.State) {
		s.MemoryWriteGrant = kind == "grant"
		s.MemoryWriteRequested, s.MemoryWriteTurn = kind == "marker", gateTurn("t1")
	})
	path := state.StatePath(cwd, gateSession)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["unverifiedSubagents"] = list
	stored, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, stored, 0o644); err != nil {
		t.Fatal(err)
	}
	return stored
}

func memoryRecordsOf(n int, edit func(i int, m map[string]any)) []any {
	out := make([]any, n)
	for i := range out {
		m := map[string]any{"agentId": fmt.Sprintf("a%d", i), "turnId": "t1", "agentType": "worker", "attempts": 3, "receiptClaimed": "none", "recordedAt": "recorded", "resolvable": true}
		if edit != nil {
			edit(i, m)
		}
		out[i] = m
	}
	return out
}

// A stored record the state reader would change (cut, retyped or dropped) must not be written back by the spend: the call is
// denied on the cannot-rewrite path, the authorization stays and the file is byte for byte as it was.
func TestMemoryGateRefusesToRewriteARecordTheReaderChanged(t *testing.T) {
	long := func(i int, m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1) }
	for name, list := range map[string]any{
		"a receipt of 257 characters":           memoryRecordsOf(1, long),
		"a receipt cut inside an astral char":   memoryRecordsOf(1, func(i int, m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", 255) + "\U0001F600b" }),
		"a long receipt after a short one":      append(memoryRecordsOf(1, nil), memoryRecordsOf(1, long)...),
		"attempts as text":                      memoryRecordsOf(1, func(i int, m map[string]any) { m["attempts"] = "3" }),
		"attempts a float64 cannot hold":        memoryRecordsOf(1, func(i int, m map[string]any) { m["attempts"] = json.Number("9007199254740993") }),
		"one record more than the reader keeps": memoryRecordsOf(state.MaxUnverifiedSubagents+1, nil),
		"an entry the reader drops":             append(memoryRecordsOf(1, nil), "not a record"),
		"a record without its recorded time":    []any{map[string]any{"agentId": "a"}},
	} {
		for _, kind := range []string{"grant", "marker"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				cwd, _, env := gateScene(t)
				before := memoryRecordsSeed(t, cwd, kind, list)
				if reason := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, nil), env)); !strings.Contains(reason, "cannot rewrite") {
					t.Errorf("reason: %s", reason)
				}
				if after, _ := os.ReadFile(state.StatePath(cwd, gateSession)); string(after) != string(before) {
					t.Errorf("the state was rewritten: %s", after)
				}
				if s := state.ReadState(cwd, gateSession); !s.MemoryWriteGrant && kind == "grant" || !s.MemoryWriteRequested && kind == "marker" {
					t.Errorf("a refused call spent the authorization: %+v", s)
				}
			})
		}
	}
}

// Records within what the reader keeps are written back whole, and the call spends its authorization as before.
func TestMemoryGateSpendsOverRecordsTheReaderKeepsWhole(t *testing.T) {
	edge := func(i int, m map[string]any) {
		m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen)
		m["attempts"] = json.Number("3.0")
	}
	for name, list := range map[string]any{
		"records with every field":    memoryRecordsOf(2, nil),
		"a receipt of 256 characters": memoryRecordsOf(1, edge),
		"the most records kept":       memoryRecordsOf(state.MaxUnverifiedSubagents, nil),
		"records with a field absent": []any{map[string]any{"agentId": "a", "recordedAt": "t"}},
		"no records":                  []any{},
	} {
		for _, kind := range []string{"grant", "marker"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				cwd, _, env := gateScene(t)
				memoryRecordsSeed(t, cwd, kind, list)
				kept := state.ReadState(cwd, gateSession).UnverifiedSubagents
				if got := HandleMemoryWriteGate(gatePayload(t, cwd, nil), env); got != "" {
					t.Fatalf("the call was not allowed: %q", got)
				}
				s := state.ReadState(cwd, gateSession)
				if s.MemoryWriteGrant || s.MemoryWriteRequested {
					t.Errorf("the authorization was not spent: %+v", s)
				}
				if len(s.UnverifiedSubagents) != len(kept) || fmt.Sprint(s.UnverifiedSubagents) != fmt.Sprint(kept) {
					t.Errorf("the records changed: %+v", s.UnverifiedSubagents)
				}
			})
		}
	}
}
