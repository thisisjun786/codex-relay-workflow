package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"syscall"
)

// transcriptMarkScanLimit bounds how much appended transcript CompactedSince reads. A hook decides and records within
// milliseconds, so more than this appended since the mark is not an ordinary gap: it reads as a compaction (the decision is
// dropped, never answered stale).
const transcriptMarkScanLimit = 256 << 20

// transcriptMarkLineScan bounds how far back MarkTranscript looks for the start of a record still being written at the
// mark (fix round 2): every record ends in a newline, so a transcript that does not end in one holds a record still
// landing, and Codex writes its type near its start. A record whose start lies further back is of an unknown kind.
const transcriptMarkLineScan = 1 << 20

// TranscriptMark is a position in a transcript that a hook took before it read the state it decides from (CRW-1159, fix
// round 1). It stands for the context generation: the injection cursor in the session state is not enough, because PostCompact
// leaves a cursor that is already reset as it is, so a compaction that lands after the decision cannot be told from the
// cursor. The transcript records every compaction (a `compacted` record, the ContextCompaction item, the context_compacted
// event), and CompactedSince reads only what was appended after the mark, from the start of the record the mark fell in
// (fix round 2): a compacted record whose type was on disk at the mark and whose remainder lands after it still counts.
//
// The zero value, and the mark of an unreadable or non-regular transcript, is no mark: nothing is known, so nothing changed
// (fail open, as ReadTranscriptGeneration).
type TranscriptMark struct {
	path    string
	size    int64 // the file's size at the mark
	start   int64 // where the record the mark fell in starts: size when the file ended in a newline
	unknown bool  // the record the mark fell in starts further back than transcriptMarkLineScan
	ok      bool
}

// MarkTranscript takes the mark: the path, the size of the regular file now and the start of the record still being
// written at its end, if any. It stats before it opens, and opens without blocking, so a FIFO with no writer cannot hold
// the hook; a file it cannot read back is marked at its end.
func MarkTranscript(path string) TranscriptMark {
	if path == "" {
		return TranscriptMark{}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return TranscriptMark{}
	}
	m := TranscriptMark{path: path, size: info.Size(), start: info.Size(), ok: true}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return m
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return m
	}
	m.start, m.unknown = recordStart(f, m.size)
	return m
}

// recordStart is where the record that ends at size starts: size itself when the byte before it is a newline (or the file
// is empty), the byte after the last newline otherwise, 0 when there is none, and unknown when transcriptMarkLineScan bytes
// back hold no newline. A read that fails marks the end.
func recordStart(f *os.File, size int64) (int64, bool) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	floor := max(0, size-transcriptMarkLineScan)
	for end := size; end > floor; {
		begin := max(floor, end-chunk)
		n, _ := f.ReadAt(buf[:end-begin], begin)
		if int64(n) != end-begin {
			return size, false
		}
		if end == size && buf[n-1] == '\n' {
			return size, false
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return begin + int64(i) + 1, false
		}
		end = begin
	}
	if floor == 0 {
		return 0, false
	}
	return size, true
}

// CompactedSince is whether the transcript records a compaction after the mark. A transcript that shrank under the mark was
// replaced, so its generation is unknown and it reads as changed, as does one that grew when the record the mark fell in
// starts beyond the look-back; one that is unreadable now reads as unchanged (fail open).
func (m TranscriptMark) CompactedSince() bool {
	if !m.ok {
		return false
	}
	f, err := os.OpenFile(m.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	switch size := info.Size(); {
	case size < m.size:
		return true
	case size == m.size:
		return false
	case m.unknown || size-m.start > transcriptMarkScanLimit:
		return true
	default:
		return holdsCompaction(io.NewSectionReader(f, m.start, size-m.start))
	}
}

// holdsCompaction reads r as JSONL records and says whether one is a compaction. A line longer than the buffer (a compacted
// record carries the whole history it replaced) or one that is not a whole record is judged by its first bytes, where Codex
// writes the record type, and the rest of a long line is skipped without being held.
func holdsCompaction(r io.Reader) bool {
	const head = 1024
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, long, err := br.ReadLine()
		if len(line) > 0 {
			var record transcriptRecord
			whole := !long && json.Unmarshal(line, &record) == nil
			// A record that is too long to read whole, or not whole yet (the write is still landing), is judged by its head.
			if whole && record.isCompaction() || !whole && bytes.Contains(line[:min(len(line), head)], []byte(`"type":"compacted"`)) {
				return true
			}
			for long && err == nil {
				_, long, err = br.ReadLine()
			}
		}
		if err != nil {
			return false
		}
	}
}
