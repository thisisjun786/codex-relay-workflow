package host

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestTranscriptMarkSeesACompactionAppendedAfterIt is CRW-1159 fix round 1: the mark answers whether the transcript
// recorded a compaction after it, by the record kinds Codex writes, however large the compacted record is.
func TestTranscriptMarkSeesACompactionAppendedAfterIt(t *testing.T) {
	const compacted = `{"timestamp":"2026-10-09T20:49:26.924Z","type":"compacted","payload":{"message":"","replacement_history":[]}}` + "\n"
	const item = `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"ContextCompaction"}}}` + "\n"
	const legacyEvent = `{"type":"event_msg","payload":{"type":"context_compacted"}}` + "\n"
	huge := `{"timestamp":"2026-10-09T20:49:26.924Z","type":"compacted","payload":{"message":"` + strings.Repeat("x", 300_000) + `"}}` + "\n"
	hugeOther := `{"timestamp":"2026-10-09T20:49:26.924Z","type":"response_item","payload":{"output":"` + strings.Repeat("x", 300_000) + `"}}` + "\n"
	for _, c := range []struct {
		name, appended string
		want           bool
	}{
		{"nothing appended", "", false},
		{"an ordinary record", devRecord("[crw: PLAN]"), false},
		{"a quote of the type", `{"type":"event_msg","payload":{"type":"agent_message","message":"\"type\":\"compacted\""}}` + "\n", false},
		{"a compacted record", compacted, true},
		{"a compaction item", item, true},
		{"a context_compacted event", legacyEvent, true},
		{"a compacted record after an ordinary one", devRecord("x") + compacted, true},
		{"a compacted record larger than the line buffer", huge, true},
		{"a large record that is no compaction, then an ordinary one", hugeOther + devRecord("x"), false},
		{"a large record that is no compaction, then a compaction", hugeOther + item, true},
		{"a compacted record cut short", strings.TrimSuffix(compacted, "\n")[:60], true},
	} {
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(devRecord("before")+compacted), 0o644); err != nil {
			t.Fatal(err)
		}
		mark := MarkTranscript(path)
		if mark.CompactedSince() {
			t.Fatalf("%s: a compaction before the mark counted", c.name)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(c.appended)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := mark.CompactedSince(); got != c.want {
			t.Errorf("%s: CompactedSince %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTranscriptMarkFailsOpen: no path, a missing file, a directory and a FIFO give no mark (nothing known, nothing
// changed); a transcript that shrank under the mark was replaced and reads as changed.
func TestTranscriptMarkFailsOpen(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	for _, path := range []string{"", filepath.Join(dir, "missing.jsonl"), dir, fifo} {
		if (MarkTranscript(path) != TranscriptMark{}) || MarkTranscript(path).CompactedSince() {
			t.Errorf("%q gave a mark or a change", path)
		}
	}
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(devRecord("a")+devRecord("b")), 0o644); err != nil {
		t.Fatal(err)
	}
	mark := MarkTranscript(path)
	if err := os.WriteFile(path, []byte(devRecord("a")), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mark.CompactedSince() {
		t.Error("a transcript replaced by a shorter one read as unchanged")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if mark.CompactedSince() {
		t.Error("a transcript that vanished read as changed")
	}
}
