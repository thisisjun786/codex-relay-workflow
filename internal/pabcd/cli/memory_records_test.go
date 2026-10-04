package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// cliRecordsStored is the session file text holding the resolvable record a1/t1 and the extra record edit leaves behind.
func cliRecordsStored(t *testing.T, edit func(m map[string]any)) []byte {
	t.Helper()
	var extra map[string]any
	b, err := json.Marshal(cliVerdict("other", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &extra); err != nil {
		t.Fatal(err)
	}
	edit(extra)
	stored, err := json.Marshal(map[string]any{"phase": "B", "unverifiedSubagents": []any{cliVerdict("a1", "t1"), extra}})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

var cliRecordsTheReaderChanges = map[string]func(m map[string]any){
	"a receipt of 257 characters":         func(m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen+1) },
	"a receipt cut inside an astral char": func(m map[string]any) { m["receiptClaimed"] = strings.Repeat("r", 255) + "\U0001F600b" },
	"attempts as text":                    func(m map[string]any) { m["attempts"] = "3" },
	"attempts a float64 cannot hold":      func(m map[string]any) { m["attempts"] = json.Number("9007199254740993") },
	"attempts printed as another number":  func(m map[string]any) { m["attempts"] = json.Number("1000000000000000128") },
}

// The three commands that write a rebuilt state back share cliVerdictsIntact, so a stored record the reader would cut or
// retype is refused by each of them: exit 1, the file byte for byte as it was, and nothing else written.
func TestCLIRefusesToRewriteARecordTheReaderChanged(t *testing.T) {
	for name, edit := range cliRecordsTheReaderChanges {
		t.Run("memory allow-write/"+name, func(t *testing.T) {
			cwd, _, _ := cliSeed(t)
			before := cliRecordsStored(t, edit)
			cliPut(t, state.StatePath(cwd, "rec-s1"), string(before))
			out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
			if code != 1 || !strings.Contains(out, "refusing to rewrite") {
				t.Fatalf("got %d %s", code, out)
			}
			cliUnchanged(t, cwd, before)
		})
		t.Run("evidence resolve/"+name, func(t *testing.T) {
			cwd, a, _ := cliSeed(t)
			before := cliRecordsStored(t, edit)
			cliPut(t, state.StatePath(cwd, "rec-s1"), string(before))
			out, code := RunEvidenceCLI(a)
			if code != 1 || !strings.Contains(out, "refusing to rewrite") {
				t.Fatalf("got %d %s", code, out)
			}
			cliUnchanged(t, cwd, before)
			if evidence.ReadAttempts(cwd, "rec-s1", "a1", "t1") != 3 {
				t.Error("the counter was cleared")
			}
			if _, err := os.Stat(filepath.Join(cwd, ".crw/ledger.jsonl")); !os.IsNotExist(err) {
				t.Error("the refusal wrote a ledger row")
			}
		})
		t.Run("scan record/"+name, func(t *testing.T) {
			cwd := scanRecordWorkspace(t)
			before := cliRecordsStored(t, edit)
			scanRecordPut(t, state.StatePath(cwd, "s1"), before)
			res := scanRecordInvoke(t, cwd)
			if res.Code != 1 || !strings.Contains(res.Output, "refusing to rewrite") {
				t.Fatalf("got %+v", res)
			}
			if string(scanRecordRead(t, state.StatePath(cwd, "s1"))) != string(before) {
				t.Error("the state was rewritten")
			}
			if _, err := os.Stat(filepath.Join(cwd, ".crw", "interviews", "s1.jsonl")); !os.IsNotExist(err) {
				t.Error("the refusal appended a scan row")
			}
		})
	}
}

// Records the reader keeps whole do not stop the grant, and the grant leaves them as they were.
func TestMemoryGrantKeepsRecordsWithinTheReadersLimits(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	stored := cliRecordsStored(t, func(m map[string]any) {
		m["receiptClaimed"] = strings.Repeat("r", state.MaxReceiptClaimLen)
		m["attempts"] = json.Number("3.0")
	})
	cliPut(t, state.StatePath(cwd, "rec-s1"), string(stored))
	want := state.ReadState(cwd, "rec-s1").UnverifiedSubagents
	if out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}); code != 0 {
		t.Fatalf("got %d %s", code, out)
	}
	s := state.ReadState(cwd, "rec-s1")
	if !s.MemoryWriteGrant || len(s.UnverifiedSubagents) != 2 || s.UnverifiedSubagents[1] != want[1] || len(s.UnverifiedSubagents[1].ReceiptClaimed) != state.MaxReceiptClaimLen {
		t.Fatalf("grant or records wrong: %+v", s)
	}
}
