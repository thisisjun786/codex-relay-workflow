package host

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unicode/utf8"
)

// TailBytes is how much of a transcript's end is searched (TRANSCRIPT_SEARCH_BYTES).
const TailBytes = 65_536

// ReadTranscriptTail is the last maxBytes of the transcript at path, decoded as Node's
// Buffer.toString("utf8") does, or "" on any error: an unreadable transcript must not block Codex
// (fail open). Only the tail is read (CRW-1160): the oracle read the whole file to cut its last
// 64 KiB, so a long session paid the whole transcript in I/O and memory on every prompt and Stop.
func ReadTranscriptTail(path string, maxBytes int) string {
	tail, _ := readTranscriptWindow(path, maxBytes)
	return tail
}

// readTranscriptWindow is ReadTranscriptTail and whether the window's first line is a whole record: the window starts
// at the file's first byte, or at the first byte after a newline. A reader of whole records drops the first line of
// a window for which this is false (it is the cut end of a longer record) and keeps it otherwise.
func readTranscriptWindow(path string, maxBytes int) (string, bool) {
	return readTranscriptWindowWith(path, maxBytes, nil)
}

// transcriptTailSeams let a test act between the stat and the read (an append, a truncation, a rename) and
// see what the read touches. Production passes nil.
type transcriptTailSeams struct {
	afterStat func()
	reader    func(io.ReaderAt) io.ReaderAt
}

func readTranscriptWindowWith(path string, maxBytes int, seams *transcriptTailSeams) (string, bool) {
	w := readTranscriptBytes(path, maxBytes, seams)
	return decodeUTF8(w.data), w.whole
}

// transcriptWindow is the bytes readTranscriptBytes read: the last bytes of the file, whether the first line of them is a
// whole record, and whether they start the file (nothing precedes them).
type transcriptWindow struct {
	data    []byte
	whole   bool
	covered bool
}

// readTranscriptBytes opens the file once, without waiting (O_NONBLOCK: a FIFO with no writer would
// hold the open), refuses anything but a regular file by fstat of that descriptor, and reads the last
// min(size, maxBytes) bytes of the size it saw with one ReadAt, preceded by the one byte before them when
// there is one: a window is cut mid-record unless that byte is a newline, which this reads without widening
// the window (CRW-1160 evaluation d1: whether the window starts at byte zero does not say whether its first record
// is cut). A file that changes after the stat is read as it then is, best effort: an append past the stat is not
// seen, a truncation leaves a short read, a rename keeps the open file; the read never widens to the whole file.
// Any error is an empty window (fail open).
func readTranscriptBytes(path string, maxBytes int, seams *transcriptTailSeams) transcriptWindow {
	if path == "" || maxBytes <= 0 {
		return transcriptWindow{}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return transcriptWindow{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return transcriptWindow{}
	}
	if seams != nil && seams.afterStat != nil {
		seams.afterStat()
	}
	var r io.ReaderAt = f
	if seams != nil && seams.reader != nil {
		r = seams.reader(f)
	}
	size := info.Size()
	start := max(0, size-int64(maxBytes))
	from := max(0, start-1) // the byte before the window says whether it starts a record
	buf := make([]byte, size-from)
	n, err := r.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return transcriptWindow{}
	}
	buf = buf[:n]
	if start == 0 {
		return transcriptWindow{data: buf, whole: true, covered: true}
	}
	if len(buf) == 0 {
		return transcriptWindow{}
	}
	return transcriptWindow{data: buf[1:], whole: buf[0] == '\n'}
}

// decodeUTF8 replaces each maximal invalid subpart with one U+FFFD (the WHATWG decoder Node uses);
// utf8.DecodeRune alone would replace every byte.
func decodeUTF8(b []byte) string {
	runes := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			n = invalidSubpart(b)
		}
		runes, b = append(runes, r), b[n:]
	}
	return string(runes)
}

// invalidSubpart is the length of the lead byte of b and the continuation bytes that could still
// have completed it (Unicode table 3-7).
func invalidSubpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		lo, need = 0xA0, 2
	case lead == 0xED:
		hi, need = 0x9F, 2
	case lead >= 0xE1 && lead <= 0xEF:
		need = 2
	case lead == 0xF0:
		lo, need = 0x90, 3
	case lead == 0xF4:
		hi, need = 0x8F, 3
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(b) && b[n] >= lo && b[n] <= hi; n++ {
		lo, hi = 0x80, 0xBF
	}
	return n
}
