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

// TranscriptMark is a position in a transcript that a hook took before it read the state it decides from (CRW-1159, fix
// round 1). It stands for the context generation: the injection cursor in the session state is not enough, because PostCompact
// leaves a cursor that is already reset as it is, so a compaction that lands after the decision cannot be told from the
// cursor. The transcript records every compaction (a `compacted` record, the ContextCompaction item, the context_compacted
// event), and CompactedSince reads only what was appended after the mark.
//
// The zero value, and the mark of an unreadable or non-regular transcript, is no mark: nothing is known, so nothing changed
// (fail open, as ReadTranscriptGeneration).
type TranscriptMark struct {
	path string
	size int64
	ok   bool
}

// MarkTranscript takes the mark: the path and the size of the regular file now. It stats without opening, so a FIFO with no
// writer cannot hold the hook.
func MarkTranscript(path string) TranscriptMark {
	if path == "" {
		return TranscriptMark{}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return TranscriptMark{}
	}
	return TranscriptMark{path: path, size: info.Size(), ok: true}
}

// CompactedSince is whether the transcript records a compaction after the mark. A transcript that shrank under the mark was
// replaced, so its generation is unknown and it reads as changed; one that is unreadable now reads as unchanged (fail open).
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
	case size-m.size > transcriptMarkScanLimit:
		return true
	default:
		return holdsCompaction(io.NewSectionReader(f, m.size, size-m.size))
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
