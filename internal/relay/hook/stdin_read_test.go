package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-504. A stdin_unreadable row names the cause of the failed read under stdinRead: the
// cause, the Go error text, the bytes the completed reads reported and the wait window as
// offsets from process entry (the origin of elapsedMs). These tests read the written row as
// JSON, so they say what a reader of the journal sees.

// stdinReadKeys are the five keys of the row's stdinRead object, and nothing else.
var stdinReadKeys = []string{"bytesRead", "cause", "error", "waitEndedMs", "waitStartedMs"}

// stdinRun drives the adapter over input under checkout settings with the given budget (seconds)
// and returns the one journal row it left.
func stdinRun(t *testing.T, budget float64, input io.Reader) (map[string]any, string) {
	t.Helper()
	home := hookHome(t, budget)
	t.Setenv("CODEX_HOME", home)
	var out bytes.Buffer
	if code := runAdapter(context.Background(), nil, input, &out, time.Now(), nil); code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 {
		t.Fatalf("want one journal row, got %v", rows)
	}
	var journal strings.Builder
	paths, _ := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		journal.Write(raw)
	}
	return rows[0], journal.String()
}

// stdinReadOf is the row's stdinRead object, which must be there with exactly its five keys.
func stdinReadOf(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	if row["adapterOutcome"] != "stdin_unreadable" || row["sessionId"] != nil || row["turnId"] != nil {
		t.Fatalf("row = %v", row)
	}
	reading, ok := row["stdinRead"].(map[string]any)
	if !ok {
		t.Fatalf("a stdin_unreadable row must say why it could not read: %v", row)
	}
	if len(reading) != len(stdinReadKeys) {
		t.Fatalf("stdinRead = %v", reading)
	}
	for _, key := range stdinReadKeys {
		if _, present := reading[key]; !present {
			t.Fatalf("stdinRead lacks %s: %v", key, reading)
		}
	}
	started, ended := reading["waitStartedMs"].(float64), reading["waitEndedMs"].(float64)
	if started < 0 || ended < started || ended > row["elapsedMs"].(float64) {
		t.Fatalf("the wait window must lie inside the invocation: %v elapsedMs=%v", reading, row["elapsedMs"])
	}
	return reading
}

func expectCause(t *testing.T, reading map[string]any, cause, text string, bytes float64) {
	t.Helper()
	if reading["cause"] != cause || reading["bytesRead"] != bytes || !strings.Contains(reading["error"].(string), text) {
		t.Fatalf("want cause=%s error~%q bytesRead=%v; got %v", cause, text, bytes, reading)
	}
}

// holdingReader is a reader that is not a descriptor and has nothing to say until released.
type holdingReader struct{ release chan struct{} }

func (r holdingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func holding(t *testing.T) holdingReader {
	r := holdingReader{release: make(chan struct{})}
	t.Cleanup(func() { close(r.release) })
	return r
}

// scriptedReader answers its steps in order, then reports end of input.
type scriptedReader struct {
	steps []func() (int, []byte, error)
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	n, data, err := step()
	copy(p, data)
	return n, err
}

func chunk(s string) func() (int, []byte, error) {
	return func() (int, []byte, error) { return len(s), []byte(s), nil }
}

// The input allocation ran out on a descriptor the host never wrote to.
func TestStdinReadInputLateOnADescriptorWithNothingWritten(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	row, _ := stdinRun(t, 5, reader)
	reading := stdinReadOf(t, row)
	expectCause(t, reading, "input_late", "did not arrive within the input allocation", 0)
	if reading["waitEndedMs"].(float64) < 100 {
		t.Fatalf("the wait ended before the 100 ms allocation: %v", reading)
	}
}

// A partial payload that stalls is counted but never recorded: payload bytes may hold secrets.
func TestStdinReadInputLateWithAPartialPayloadCountsBytesNotContent(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	partial := `{"secret":"hunter2-`
	if _, err := writer.WriteString(partial); err != nil {
		t.Fatal(err)
	}
	row, journal := stdinRun(t, 5, reader)
	expectCause(t, stdinReadOf(t, row), "input_late", "input allocation", float64(len(partial)))
	if strings.Contains(journal, "hunter2") {
		t.Fatalf("the journal holds payload bytes: %s", journal)
	}
}

// A reader that is not a descriptor is waited for under the plain deadline.
func TestStdinReadInputLateOnAReaderThatIsNotADescriptor(t *testing.T) {
	row, _ := stdinRun(t, 5, holding(t))
	expectCause(t, stdinReadOf(t, row), "input_late", "input allocation", 0)
}

// With a budget under the allocation the work context ends first. The reader is held so both
// candidate contexts end unrecognised and only their origin decides.
func TestStdinReadWorkEndedBeforeTheInputAllocation(t *testing.T) {
	row, _ := stdinRun(t, 0.1, holding(t))
	expectCause(t, stdinReadOf(t, row), "work_ended", "context deadline exceeded", 0)
}

// A descriptor whose read fails: poll reports it ready and read answers EISDIR.
func TestStdinReadErrorFromTheDescriptor(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	row, _ := stdinRun(t, 5, dir)
	expectCause(t, stdinReadOf(t, row), "read_error", "is a directory", 0)
}

// A closed descriptor cannot be polled; the plain read fails on it.
func TestStdinReadErrorFromAClosedFile(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	row, _ := stdinRun(t, 5, reader)
	expectCause(t, stdinReadOf(t, row), "read_error", "file already closed", 0)
}

// Bytes that were reported before the failure are counted whichever way the failure came.
func TestStdinReadErrorKeepsTheBytesReportedBeforeIt(t *testing.T) {
	boom := errors.New("boom")
	for name, c := range map[string]struct {
		steps []func() (int, []byte, error)
		want  float64
	}{
		"after a chunk":  {[]func() (int, []byte, error){chunk("abc"), func() (int, []byte, error) { return 0, nil, boom }}, 3},
		"with the error": {[]func() (int, []byte, error){chunk("ab"), func() (int, []byte, error) { return 2, []byte("cd"), boom }}, 4},
	} {
		t.Run(name, func(t *testing.T) {
			row, _ := stdinRun(t, 5, &scriptedReader{steps: c.steps})
			expectCause(t, stdinReadOf(t, row), "read_error", "boom", c.want)
		})
	}
}

// A reader that itself returns a deadline error did not run out of allocation.
func TestStdinReadErrorThatLooksLikeADeadlineIsStillAReadError(t *testing.T) {
	reader := &scriptedReader{steps: []func() (int, []byte, error){func() (int, []byte, error) { return 0, nil, context.DeadlineExceeded }}}
	row, _ := stdinRun(t, 5, reader)
	expectCause(t, stdinReadOf(t, row), "read_error", "context deadline exceeded", 0)
}

// A panic is recorded by its Go type; its value could be anything, payload included.
func TestStdinReadPanicIsRecordedByTypeNotValue(t *testing.T) {
	reader := &scriptedReader{steps: []func() (int, []byte, error){chunk("abc"), func() (int, []byte, error) { panic("hunter2 is the payload") }}}
	row, journal := stdinRun(t, 5, reader)
	expectCause(t, stdinReadOf(t, row), "read_error", "panic in the stdin reader: string", 3)
	if strings.Contains(journal, "hunter2") {
		t.Fatalf("the journal holds a panic value: %s", journal)
	}
}

// Every byte arrived but they are not UTF-8: the bytes are counted, the secret before them is not recorded.
func TestStdinReadInvalidUTF8(t *testing.T) {
	payload := []byte(`{"secret":"hunter2-`)
	payload = append(payload, 0xff)
	row, journal := stdinRun(t, 5, pipeInput(t, payload))
	reading := stdinReadOf(t, row)
	expectCause(t, reading, "invalid_utf8", "", float64(len(payload)))
	detail := row["detail"].(string)
	if !strings.HasPrefix(detail, "the Stop payload is not UTF-8: ") || strings.TrimPrefix(detail, "the Stop payload is not UTF-8: ") != reading["error"] {
		t.Fatalf("detail=%q stdinRead=%v", detail, reading)
	}
	if strings.Contains(journal, "hunter2") {
		t.Fatalf("the journal holds payload bytes: %s", journal)
	}
}

// Not read failures, and so no stdinRead: a clean end of input (also before any byte) reaches
// JSON parsing, text that is not JSON is stdin_not_json, and there is no byte cap.
func TestStdinReadIsAbsentWhereReadingDidNotFail(t *testing.T) {
	big := []byte(strings.TrimSuffix(`{"session_id":"s","turn_id":"t"}`, "}") + `,"padding":"` + strings.Repeat("x", 2<<20) + `"}`)
	for name, c := range map[string]struct {
		input   func(*testing.T) io.Reader
		outcome string
	}{
		"end of input before a payload": {func(t *testing.T) io.Reader { return pipeInput(t, nil) }, "stdin_not_json"},
		"not json":                      {func(t *testing.T) io.Reader { return pipeInput(t, []byte("not json")) }, "stdin_not_json"},
		"over 1 MiB, no byte cap":       {func(t *testing.T) io.Reader { return fileInput(t, big) }, "guard_unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			row, _ := stdinRun(t, 5, c.input(t))
			if _, present := row["stdinRead"]; present || row["adapterOutcome"] != c.outcome {
				t.Fatalf("row = %v", row)
			}
		})
	}
}
