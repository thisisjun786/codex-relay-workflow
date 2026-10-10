package host

import (
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/jsnode"
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
	return jsnode.DecodeUTF8(data)
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
