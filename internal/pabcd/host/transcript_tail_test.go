package host

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bigTranscript is a 64 MiB transcript (sparse, so the test writes only its tail) that ends with tail.
func bigTranscript(t *testing.T, tail string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 64 << 20
	if err := f.Truncate(size - int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(tail), size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadTranscriptTailDoesNotLoadTheWholeFile is CRW-1160 end condition 1 seen from the heap: the oracle (and the port
// before CRW-1160) read the whole transcript to keep its last 64 KiB, so a 64 MiB session allocated 64 MiB per prompt.
func TestReadTranscriptTailDoesNotLoadTheWholeFile(t *testing.T) {
	path := bigTranscript(t, "\n{\"tail\":true}\n")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := ReadTranscriptTail(path, TailBytes)
	runtime.ReadMemStats(&after)
	if len(got) == 0 || got[len(got)-len("{\"tail\":true}\n"):] != "{\"tail\":true}\n" {
		t.Fatalf("the tail was not read: %d bytes", len(got))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Errorf("reading a 64 KiB tail allocated %d bytes", allocated)
	}
}

// TestReadTranscriptTailRefusesAFIFOAtOnce is CRW-1160 end condition 3: a transcript path that names a FIFO with no
// writer must not hold the hook (an open without O_NONBLOCK waits for a writer for ever).
func TestReadTranscriptTailRefusesAFIFOAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan string, 1)
	go func() { done <- ReadTranscriptTail(path, TailBytes) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("a FIFO answered %q", got)
		}
	case <-time.After(3 * time.Second):
		// Unblock the stuck open so the goroutine ends, then fail.
		if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("reading a FIFO transcript blocked")
	}
}

// countingReader counts the bytes each ReadAt hands back.
type countingReader struct {
	r    io.ReaderAt
	read *int64
}

func (c countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	*c.read += int64(n)
	return n, err
}

// TestReadTranscriptTailReadsAtMostTheWindow is CRW-1160 end condition 1: on a 64 MiB transcript the reader touches
// at most TailBytes and the one byte before them, which says whether the window starts at a record.
func TestReadTranscriptTailReadsAtMostTheWindow(t *testing.T) {
	path := bigTranscript(t, "\n{\"tail\":true}\n")
	var read int64
	got, _ := readTranscriptWindowWith(path, TailBytes, &transcriptTailSeams{reader: func(r io.ReaderAt) io.ReaderAt { return countingReader{r, &read} }})
	if read > TailBytes+1 || read == 0 || len(got) == 0 {
		t.Errorf("read %d bytes (%d decoded), want at most %d", read, len(got), TailBytes+1)
	}
}

// oldReadTranscriptTail is the reader before CRW-1160: the whole file, then its last maxBytes, decoded.
func oldReadTranscriptTail(path string, maxBytes int) string {
	if path == "" || maxBytes <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return decodeUTF8(data[max(0, len(data)-maxBytes):])
}

// TestReadTranscriptTailDecodesAsBefore is CRW-1160 end condition 2: short, long, invalid UTF-8 and rune-split
// windows decode to the same text as the whole-file reader did.
func TestReadTranscriptTailDecodesAsBefore(t *testing.T) {
	long := strings.Repeat("é—z\xa9\xf0\x9f\x98\x80", 20_000)
	for name, content := range map[string]string{
		"empty": "", "short": "AAAA\nBBBB\ntail-here", "invalid": "\xa9z\xed\xa0\x80\xf4\x90\x80\x80\xc0\x80",
		"long": long, "long, invalid at the cut": strings.Repeat("x", 100) + "\xe2\x80" + strings.Repeat("y", TailBytes-1),
	} {
		path := writeTail(t, content)
		for _, maxBytes := range []int{TailBytes, 1, 2, 3, 4, 5, 7, 1000, len(content), len(content) + 1, 0, -1} {
			if got, want := ReadTranscriptTail(path, maxBytes), oldReadTranscriptTail(path, maxBytes); got != want {
				t.Errorf("%s, max %d: %q, want %q", name, maxBytes, got, want)
			}
		}
	}
}

// openDescriptors is the number of this process's open descriptors, or -1 where /proc/self/fd is missing.
func openDescriptors() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// TestReadTranscriptTailUnderAChangingFile is CRW-1160 end condition 3: an append, a truncation or a rename between
// the stat and the read is read best effort from the descriptor already open, never widened to the whole file; an
// unreadable path, a directory and a FIFO answer "" and leave no descriptor open.
func TestReadTranscriptTailUnderAChangingFile(t *testing.T) {
	const content = "first\nsecond\nthird\n"
	t.Run("append", func(t *testing.T) {
		path := writeTail(t, content)
		got, _ := readTranscriptWindowWith(path, 8, &transcriptTailSeams{afterStat: func() {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("fourth\n"); err != nil {
				t.Fatal(err)
			}
		}})
		if got != content[len(content)-8:] {
			t.Errorf("an append after the stat: %q", got)
		}
	})
	t.Run("truncate", func(t *testing.T) {
		path := writeTail(t, content)
		got, _ := readTranscriptWindowWith(path, 8, &transcriptTailSeams{afterStat: func() {
			if err := os.Truncate(path, int64(len(content)-4)); err != nil {
				t.Fatal(err)
			}
		}})
		if got != content[len(content)-8:len(content)-4] {
			t.Errorf("a truncation after the stat: %q", got)
		}
		got, _ = readTranscriptWindowWith(path, 8, &transcriptTailSeams{afterStat: func() {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
		}})
		if got != "" {
			t.Errorf("a file truncated to nothing: %q", got)
		}
	})
	t.Run("rename", func(t *testing.T) {
		path := writeTail(t, content)
		got, _ := readTranscriptWindowWith(path, 6, &transcriptTailSeams{afterStat: func() {
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}})
		if got != "third\n" {
			t.Errorf("a rename after the stat: %q", got)
		}
	})
	t.Run("released", func(t *testing.T) {
		dir := t.TempDir()
		unreadable := filepath.Join(dir, "locked.jsonl")
		if err := os.WriteFile(unreadable, []byte(content), 0o000); err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		before := openDescriptors()
		for _, path := range []string{unreadable, dir, fifo, filepath.Join(dir, "missing")} {
			if got := ReadTranscriptTail(path, TailBytes); got != "" && !(path == unreadable && os.Geteuid() == 0) {
				t.Errorf("%s answered %q", path, got)
			}
		}
		if after := openDescriptors(); after != before {
			t.Errorf("open descriptors %d before, %d after", before, after)
		}
	})
}

// TestReadTranscriptGenerationKeepsTheFirstRecordOfAnAlignedWindow is CRW-1160 evaluation d1: a window that starts at a
// record boundary (the byte before it is the newline that ended the previous record) holds a whole first record, so the
// marker in it counts; a window that starts inside a record still drops that record's cut end.
func TestReadTranscriptGenerationKeepsTheFirstRecordOfAnAlignedWindow(t *testing.T) {
	record := devRecord("[crw: PLAN]\nWrite a diff-level plan")
	file := writeTail(t, strings.Repeat("a", 100)+"\n"+record)
	if !ReadTranscriptGeneration(file, len(record)).HasStageMarkerForPhase("P") {
		t.Error("a window that starts at a record boundary dropped its first, whole record")
	}
	if ReadTranscriptGeneration(file, len(record)-1).HasStageMarkerForPhase("P") {
		t.Error("a window that starts inside the record read it")
	}
	if !ReadTranscriptGeneration(file, len(record)+1).HasStageMarkerForPhase("P") {
		t.Error("a window that starts at the newline before the record dropped it")
	}
	if tail := ReadTranscriptTail(file, len(record)); tail != record {
		t.Errorf("the tail is not the last %d bytes: %q", len(record), tail)
	}
}

// TestContextPressureOutlivesTheTailWindow is CRW-1090 evaluation d2: a compaction no user prompt has followed is pressure
// however much is appended after it, until a prompt is recorded; the scan widens past the 64 KiB tail to find the
// boundary and stops at ContextPressureScanBytes (a transcript with neither in that reach reads as no pressure).
func TestContextPressureOutlivesTheTailWindow(t *testing.T) {
	compaction := `{"type":"compacted","payload":{"message":"","replacement_history":[]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"ContextCompaction"}}}` + "\n"
	user := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}` + "\n"
	filler := func(n int) string {
		return strings.Repeat(`{"type":"response_item","payload":{"type":"function_call_output","output":"`+strings.Repeat("w", 900)+`"}}`+"\n", n)
	}
	for _, c := range []struct {
		name, content string
		want          bool
	}{
		{"compaction at the end", compaction, true},
		{"compaction, then 200 KiB of tool output", compaction + filler(220), true},
		{"compaction, 3 MiB of tool output", compaction + filler(3300), true},
		{"compaction, a prompt, then 200 KiB", compaction + user + filler(220), false},
		{"a prompt, then 200 KiB, no compaction", user + filler(220), false},
		{"no compaction at all", filler(220), false},
		{"a prompt, a compaction, then 200 KiB", user + compaction + filler(220), true},
	} {
		if got := TranscriptContextPressure(writeTail(t, c.content)); got != c.want {
			t.Errorf("%s: pressure %v, want %v", c.name, got, c.want)
		}
	}
	// Beyond the scan reach the boundary is not looked for.
	if TranscriptContextPressure(bigTranscript(t, "\n")) {
		t.Error("a 64 MiB transcript with no boundary read as pressure")
	}
	if !TranscriptContextPressure(bigTranscript(t, "\n"+compaction)) {
		t.Error("a compaction at the end of a 64 MiB transcript did not read as pressure")
	}
	path := filepath.Join(t.TempDir(), "far.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(compaction); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(ContextPressureScanBytes + 4<<20); err != nil {
		t.Fatal(err)
	}
	if TranscriptContextPressure(path) {
		t.Error("a compaction beyond the scan reach read as pressure")
	}
}
