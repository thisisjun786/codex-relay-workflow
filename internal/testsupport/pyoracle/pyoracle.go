// Package pyoracle replays the answers the Python reference implementation gave to the Go port's
// parity tests.
//
// The port was proven by running the Python implementation live beside the Go one. That
// implementation leaves the repository (todo 44), so a test reads the answer Python gave from a
// recording instead of asking Python again. A recording is one file per test under the calling
// package's testdata/python-oracle directory, holding each answer the test asked for under a key
// the test names.
//
// CRW_PYTHON_ORACLE selects the mode:
//
//   - unset: replay. capture never runs, and a missing recording fails the test.
//   - "record": run capture (the live Python) and write what it answered.
//   - "check": run capture and fail when it answers other than the recording.
//
// Only a checkout that still carries the Python implementation can record or check.
package pyoracle

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Mode is how an answer is obtained.
type Mode string

const (
	Replay Mode = ""
	Record Mode = "record"
	Check  Mode = "check"
)

// ModeEnv names the variable that selects the mode.
const ModeEnv = "CRW_PYTHON_ORACLE"

// Directory is where a package keeps its recordings, relative to the package directory.
const Directory = "testdata/python-oracle"

// compressAbove is the size from which a recording is written gzip-compressed.
const compressAbove = 256 << 10

// CurrentMode reads ModeEnv.
func CurrentMode() Mode {
	switch mode := Mode(os.Getenv(ModeEnv)); mode {
	case Replay, Record, Check:
		return mode
	default:
		panic(fmt.Sprintf("pyoracle: %s=%q is not one of record, check or unset", ModeEnv, string(mode)))
	}
}

// Live reports whether answers come from the live Python in this run.
func Live() bool { return CurrentMode() != Replay }

// Option adjusts one Answer.
type Option func(*options)

type options struct {
	substitutions [][2]string
	same          func(recorded, live []byte) bool
}

// Substitute stores actual as placeholder in the recording and puts actual back on replay. Use it
// for every run-specific string an answer carries, such as a temporary directory. Substitutions
// apply in the order given, so name a longer path before a prefix of it.
func Substitute(actual, placeholder string) Option {
	return func(o *options) {
		if actual != "" {
			o.substitutions = append(o.substitutions, [2]string{actual, placeholder})
		}
	}
}

// SameWhen replaces byte equality in check mode, for answers that carry values a rerun changes.
func SameWhen(same func(recorded, live []byte) bool) Option {
	return func(o *options) { o.same = same }
}

// Answer returns what Python answered under key for the calling test. capture runs the live
// Python and is called only in record and check mode. The key must be unique within the test and
// the same on every run; a test that asks in a loop names each question.
func Answer(t testing.TB, key string, capture func() ([]byte, error), opts ...Option) []byte {
	t.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	rec := recordingFor(t)
	switch CurrentMode() {
	case Record:
		live := run(t, key, capture)
		rec.put(key, apply(live, o.substitutions, false))
		return live
	case Check:
		live := run(t, key, capture)
		recorded, ok := rec.get(key)
		if !ok {
			t.Fatalf("pyoracle: %s has no recording for %q; record it with %s=record", rec.path, key, ModeEnv)
		}
		recorded = apply(recorded, o.substitutions, true)
		same := o.same
		if same == nil {
			same = bytes.Equal
		}
		if !same(recorded, live) {
			t.Fatalf("pyoracle: live Python answers %q differently from %s\nrecorded: %s\nlive:     %s", key, rec.path, clip(recorded), clip(live))
		}
		return live
	default:
		recorded, ok := rec.get(key)
		if !ok {
			t.Fatalf("pyoracle: %s has no recording for %q; record it with %s=record on a checkout that has the Python implementation", rec.path, key, ModeEnv)
		}
		return apply(recorded, o.substitutions, true)
	}
}

// JSON is Answer for a value: capture's result is recorded as JSON and decoded into out.
func JSON(t testing.TB, key string, out any, capture func() (any, error), opts ...Option) {
	t.Helper()
	raw := Answer(t, key, func() ([]byte, error) {
		value, err := capture()
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}, opts...)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("pyoracle: decode %q: %v", key, err)
	}
}

func run(t testing.TB, key string, capture func() ([]byte, error)) []byte {
	t.Helper()
	live, err := capture()
	if err != nil {
		t.Fatalf("pyoracle: capture %q: %v", key, err)
	}
	return live
}

func apply(data []byte, substitutions [][2]string, back bool) []byte {
	if back {
		for i := len(substitutions) - 1; i >= 0; i-- {
			data = bytes.ReplaceAll(data, []byte(substitutions[i][1]), []byte(substitutions[i][0]))
		}
		return data
	}
	for _, s := range substitutions {
		data = bytes.ReplaceAll(data, []byte(s[0]), []byte(s[1]))
	}
	return data
}

func clip(data []byte) string {
	if len(data) > 2000 {
		return string(data[:2000]) + "…"
	}
	return string(data)
}

// recording is one test's file. Answers are held as they are stored: UTF-8 text as a string,
// anything else as base64 under a "b64:" prefix.
type recording struct {
	mu      sync.Mutex
	path    string
	answers map[string]string
	dirty   bool
}

var (
	recordingsMu sync.Mutex
	recordings   = map[string]*recording{}
)

func recordingFor(t testing.TB) *recording {
	t.Helper()
	path := filepath.Join(Directory, fileName(t.Name()))
	recordingsMu.Lock()
	defer recordingsMu.Unlock()
	if rec, ok := recordings[path]; ok {
		return rec
	}
	rec := &recording{path: path, answers: map[string]string{}}
	if CurrentMode() != Record {
		if err := rec.load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("pyoracle: %v", err)
		}
	}
	recordings[path] = rec
	if CurrentMode() == Record {
		t.Cleanup(func() {
			if err := rec.save(); err != nil {
				t.Errorf("pyoracle: %v", err)
			}
		})
	}
	return rec
}

// fileName turns a test name into a file name. A name made only of letters, digits, '-', '_' and
// '.' is used as it is; any other name, a subtest's included, keeps a readable prefix (subtest
// separators as "__", other characters as "_") and gains a digest of the whole name, so two names
// never share a file.
func fileName(name string) string {
	var b strings.Builder
	plain := len(name) <= 120 && !strings.Contains(name, "__")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteString("__")
			plain = false
		default:
			b.WriteByte('_')
			plain = false
		}
	}
	base := b.String()
	if !plain {
		sum := sha256.Sum256([]byte(name))
		if len(base) > 100 {
			base = base[:100]
		}
		base += "-" + hex.EncodeToString(sum[:])[:12]
	}
	return base + ".json"
}

func (r *recording) get(key string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.answers[key]
	if !ok {
		return nil, false
	}
	if rest, found := strings.CutPrefix(stored, "b64:"); found {
		data, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	if rest, found := strings.CutPrefix(stored, "txt:"); found {
		return []byte(rest), true
	}
	return nil, false
}

func (r *recording) put(key string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if utf8.Valid(data) {
		r.answers[key] = "txt:" + string(data)
	} else {
		r.answers[key] = "b64:" + base64.StdEncoding.EncodeToString(data)
	}
	r.dirty = true
}

type file struct {
	Note    string            `json:"note"`
	Answers map[string]string `json:"answers"`
}

const note = "Answers the Python reference implementation gave this test, recorded by internal/testsupport/pyoracle."

func (r *recording) load() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		compressed, gzErr := os.ReadFile(r.path + ".gz")
		if gzErr != nil {
			return err
		}
		reader, gzErr := gzip.NewReader(bytes.NewReader(compressed))
		if gzErr != nil {
			return fmt.Errorf("%s.gz: %w", r.path, gzErr)
		}
		if data, err = io.ReadAll(reader); err != nil {
			return fmt.Errorf("%s.gz: %w", r.path, err)
		}
	} else if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("%s: %w", r.path, err)
	}
	r.answers = f.Answers
	if r.answers == nil {
		r.answers = map[string]string{}
	}
	return nil
}

func (r *recording) save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(file{Note: note, Answers: r.answers}); err != nil {
		return err
	}
	data := encoded.Bytes()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(r.path)
	_ = os.Remove(r.path + ".gz")
	if len(data) <= compressAbove {
		return os.WriteFile(r.path, data, 0o644)
	}
	var buf bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return os.WriteFile(r.path+".gz", buf.Bytes(), 0o644)
}
