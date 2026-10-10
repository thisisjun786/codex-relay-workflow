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
	return readTranscriptTailWith(path, maxBytes, nil)
}

// transcriptTailSeams let a test act between the stat and the read (an append, a truncation, a rename) and
// see what the read touches. Production passes nil.
type transcriptTailSeams struct {
	afterStat func()
	reader    func(io.ReaderAt) io.ReaderAt
}

// readTranscriptTailWith is ReadTranscriptTail with the test seams: the last min(size, maxBytes) bytes, decoded.
func readTranscriptTailWith(path string, maxBytes int, seams *transcriptTailSeams) string {
	data, _, _ := readTranscriptRange(path, maxBytes, seams, func(size int64) int64 { return max(0, size-int64(maxBytes)) })
	return decodeUTF8(data)
}

// transcriptWindow is what readTranscriptBytes read for a reader of whole records: the bytes of the window and whether
// the first line of them is a whole record.
type transcriptWindow struct {
	data  []byte
	whole bool
}

// readTranscriptBytes reads the window of whole records at the end of the transcript, touching at most maxBytes bytes
// with one ReadAt (CRW-1160 end condition 1: <= 65,536 for TailBytes, verification round 3). A file of at most maxBytes is
// read whole. A longer one is read as the maxBytes bytes that end one byte before its end: the first of them says
// whether the window after it starts a record (a newline does; CRW-1160 evaluation d1: whether the window starts at byte
// zero does not say whether its first record is cut), and the file's last byte is left unread. Every landed record ends
// in a newline, so the record that ends there is whole without it, and the records this keeps are the whole records that
// lie in the last maxBytes bytes of the file. A record whose newline has not landed is still being written: cut by its
// last byte it is no JSON object, so it is not counted until it lands.
func readTranscriptBytes(path string, maxBytes int, seams *transcriptTailSeams) transcriptWindow {
	data, size, ok := readTranscriptRange(path, maxBytes, seams, func(size int64) int64 { return max(0, size-int64(maxBytes)-1) })
	if !ok {
		return transcriptWindow{}
	}
	if size <= int64(maxBytes) {
		return transcriptWindow{data: data, whole: true}
	}
	if len(data) == 0 {
		return transcriptWindow{}
	}
	return transcriptWindow{data: data[1:], whole: data[0] == '\n'}
}

// readTranscriptRange opens the file once, without waiting (O_NONBLOCK: a FIFO with no writer would hold the open),
// refuses anything but a regular file by fstat of that descriptor, and reads min(size, maxBytes) bytes of the size it
// saw from from(size) with one ReadAt. A file that changes after the stat is read as it then is, best effort: an append
// past the stat is not seen, a truncation leaves a short read, a rename keeps the open file; the read never widens. Any
// error is no read (fail open). size is the size the stat saw.
func readTranscriptRange(path string, maxBytes int, seams *transcriptTailSeams, from func(size int64) int64) ([]byte, int64, bool) {
	if path == "" || maxBytes <= 0 {
		return nil, 0, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, false
	}
	if seams != nil && seams.afterStat != nil {
		seams.afterStat()
	}
	var r io.ReaderAt = f
	if seams != nil && seams.reader != nil {
		r = seams.reader(f)
	}
	size := info.Size()
	start := from(size)
	buf := make([]byte, min(size, int64(maxBytes)))
	n, err := r.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, false
	}
	return buf[:n], size, true
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
