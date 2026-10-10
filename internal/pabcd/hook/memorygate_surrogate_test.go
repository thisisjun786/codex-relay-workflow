package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// rewriteLosslessSeed writes, as raw text, a phase B session file that holds a spendable authorization (the CLI grant, or the
// marker for turn t1) and one unverified record whose receiptClaimed and attempts are the given JSON texts, and returns the bytes.
// The text is written as given because a Go string cannot hold a lone surrogate escape or be marshalled to one.
func rewriteLosslessSeed(t *testing.T, cwd, kind, receipt, attempts string) []byte {
	t.Helper()
	auth := `"memoryWriteGrant":true`
	if kind == "marker" {
		auth = `"memoryWriteRequested":true,"memoryWriteTurn":"t1"`
	}
	raw := []byte(fmt.Sprintf(`{"phase":"B","sessionId":"s1",%s,"unverifiedSubagents":[{"agentId":"a","recordedAt":"t","receiptClaimed":%s,"attempts":%s}]}`, auth, receipt, attempts))
	file := state.StatePath(cwd, gateSession)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

// A stored receipt the decoder would alter (an unpaired surrogate escape, invalid UTF-8) reads as U+FFFD on both sides of the
// guard, so the spend would write U+FFFD over it. The call is denied on the cannot-rewrite path, the file keeps its bytes and the
// authorization stays.
func TestMemoryGateRefusesToRewriteTextTheDecoderWouldAlter(t *testing.T) {
	for name, receipt := range map[string]string{
		"a lone surrogate escape":                   `"\ud800"`,
		"a receipt the 256-unit cut left on a high": "\"" + strings.Repeat("r", 255) + `\ud83d"`,
		"the bytes of a surrogate as UTF-8":         "\"\xed\xa0\x80\"",
		"an invalid byte":                           "\"\xff\"",
	} {
		for _, kind := range []string{"grant", "marker"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				cwd, _, env := gateScene(t)
				before := rewriteLosslessSeed(t, cwd, kind, receipt, "3")
				payload := gatePayload(t, cwd, map[string]any{"tool_name": "memories.add_ad_hoc_note"})
				if reason := gateDeny(t, HandleMemoryWriteGate(payload, env)); !strings.Contains(reason, "authorization-state") {
					t.Errorf("reason: %s", reason)
				}
				if after, _ := os.ReadFile(state.StatePath(cwd, gateSession)); string(after) != string(before) {
					t.Errorf("the state was rewritten: %q", after)
				}
				if s := state.ReadState(cwd, gateSession); !s.MemoryWriteGrant && kind == "grant" || !s.MemoryWriteRequested && kind == "marker" {
					t.Errorf("a refused call spent the authorization: %+v", s)
				}
			})
		}
	}
}

// Text the decoder keeps is written back whole and the call spends its authorization: a valid pair, text that only looks like an
// escape, and an exponent form of the number the reader keeps.
func TestMemoryGateSpendsOverTextTheDecoderKeeps(t *testing.T) {
	for name, c := range map[string]struct{ receipt, attempts, want string }{
		"a valid pair and attempts 3e0000":     {`"\ud83d\ude00"`, "3e0000", "\U0001F600"},
		"an escaped backslash before the text": {`"\\ud800"`, "3", `\ud800`},
	} {
		for _, kind := range []string{"grant", "marker"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				cwd, _, env := gateScene(t)
				rewriteLosslessSeed(t, cwd, kind, c.receipt, c.attempts)
				payload := gatePayload(t, cwd, map[string]any{"tool_name": "memories.add_ad_hoc_note"})
				if got := HandleMemoryWriteGate(payload, env); got != "" {
					t.Fatalf("the call was not allowed: %q", got)
				}
				s := state.ReadState(cwd, gateSession)
				if s.MemoryWriteGrant || s.MemoryWriteRequested {
					t.Errorf("the authorization was not spent: %+v", s)
				}
				if len(s.UnverifiedSubagents) != 1 || s.UnverifiedSubagents[0].ReceiptClaimed != c.want || s.UnverifiedSubagents[0].Attempts != 3 {
					t.Errorf("the record changed: %+v", s.UnverifiedSubagents)
				}
			})
		}
	}
}
