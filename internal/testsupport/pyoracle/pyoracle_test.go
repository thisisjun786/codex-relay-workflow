package pyoracle

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inFreshDirectory runs a case from its own directory, so recordings land in a scratch tree.
func inFreshDirectory(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	forget()
}

// forget writes every recording made so far and drops them, as a new test process would.
// readRecording reads a recording file as written, decompressed.
func readRecording(path string) ([]byte, error) {
	compressed, err := os.ReadFile(path + ".gz")
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

func forget() {
	recordingsMu.Lock()
	defer recordingsMu.Unlock()
	for _, rec := range recordings {
		if err := rec.save(); err != nil {
			panic(err)
		}
		rec.dirty = false
	}
	recordings = map[string]*recording{}
}

// fatalRecorder turns Fatalf into a recorded message and a panic, so a test can observe it.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	panic(f)
}

func (f *fatalRecorder) Helper() {}

func fatalOf(t testing.TB, ask func(testing.TB)) string {
	f := &fatalRecorder{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil && r != f {
				panic(r)
			}
		}()
		ask(f)
	}()
	return f.message
}

func TestRecord_then_replay_returns_the_answer_with_run_paths_put_back(t *testing.T) {
	inFreshDirectory(t)
	var path string
	{
		t.Setenv(ModeEnv, "record")
		path = filepath.Join(Directory, fileName(t.Name()))
		got := Answer(t, "first", func() ([]byte, error) { return []byte("state at /tmp/run-1/db"), nil },
			Substitute("/tmp/run-1", "<TMP>"))
		if string(got) != "state at /tmp/run-1/db" {
			t.Fatalf("record returned %q", got)
		}
		Answer(t, "binary", func() ([]byte, error) { return []byte{0xff, 0x00}, nil })
	}
	forget()
	data, err := readRecording(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/tmp/run-1") || !strings.Contains(string(data), "<TMP>") {
		t.Fatalf("the recording keeps the run path: %s", data)
	}
	{
		t.Setenv(ModeEnv, "")
		got := Answer(t, "first", func() ([]byte, error) { return nil, errors.New("replay must not capture") },
			Substitute("/tmp/run-2", "<TMP>"))
		if string(got) != "state at /tmp/run-2/db" {
			t.Fatalf("replay returned %q", got)
		}
		if got := Answer(t, "binary", nil); string(got) != "\xff\x00" {
			t.Fatalf("replay returned %q", got)
		}
	}
}

func TestCheck_compares_live_Python_with_the_recording(t *testing.T) {
	inFreshDirectory(t)
	{
		t.Setenv(ModeEnv, "record")
		Answer(t, "k", func() ([]byte, error) { return []byte("one"), nil })
	}
	forget()
	{
		t.Setenv(ModeEnv, "check")
		if got := Answer(t, "k", func() ([]byte, error) { return []byte("one"), nil }); string(got) != "one" {
			t.Fatalf("check returned %q", got)
		}
		message := fatalOf(t, func(tb testing.TB) {
			Answer(tb, "k", func() ([]byte, error) { return []byte("two"), nil })
		})
		if !strings.Contains(message, "differently") {
			t.Fatalf("a changed answer passed check: %q", message)
		}
		lenient := SameWhen(func(recorded, live []byte) bool { return len(recorded) == len(live) })
		if message := fatalOf(t, func(tb testing.TB) {
			Answer(tb, "k", func() ([]byte, error) { return []byte("two"), nil }, lenient)
		}); message != "" {
			t.Fatalf("SameWhen was not used: %q", message)
		}
	}
}

func TestReplay_without_a_recording_names_the_file_and_the_key(t *testing.T) {
	inFreshDirectory(t)
	t.Setenv(ModeEnv, "")
	message := fatalOf(t, func(tb testing.TB) {
		Answer(tb, "missing", func() ([]byte, error) { panic("replay must not capture") })
	})
	if !strings.Contains(message, `"missing"`) || !strings.Contains(message, Directory) {
		t.Fatalf("message %q", message)
	}
}

func TestLarge_recordings_are_compressed_and_read_back(t *testing.T) {
	inFreshDirectory(t)
	big := strings.Repeat("row\n", 256<<10)
	var path string
	{
		t.Setenv(ModeEnv, "record")
		path = filepath.Join(Directory, fileName(t.Name()))
		Answer(t, "k", func() ([]byte, error) { return []byte(big), nil })
	}
	forget()
	if _, err := os.Stat(path + ".gz"); err != nil {
		t.Fatal(err)
	}
	{
		t.Setenv(ModeEnv, "")
		if got := Answer(t, "k", nil); string(got) != big {
			t.Fatalf("read back %d bytes", len(got))
		}
	}
}

func TestJSON_round_trips_a_value(t *testing.T) {
	inFreshDirectory(t)
	{
		t.Setenv(ModeEnv, "record")
		var out map[string]any
		JSON(t, "v", &out, func() (any, error) { return map[string]any{"n": 1, "s": "x"}, nil })
	}
	forget()
	{
		t.Setenv(ModeEnv, "")
		var out map[string]any
		JSON(t, "v", &out, nil)
		if fmt.Sprint(out["n"]) != "1" || out["s"] != "x" {
			t.Fatalf("read back %v", out)
		}
	}
}

func TestFile_names_keep_distinct_tests_apart(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"TestA", "TestA/b c", "TestA/b_c", "TestA/b/c", "TestA__b__c", "TestA_b_c",
		strings.Repeat("TestLong", 30), strings.Repeat("TestLong", 30) + "x"} {
		file := fileName(name)
		if other, ok := seen[file]; ok {
			t.Fatalf("%q and %q share %s", name, other, file)
		}
		seen[file] = name
		if len(file) > 140 {
			t.Fatalf("%s is too long", file)
		}
	}
	if fileName("TestPlain_name") != "TestPlain_name.json" {
		t.Fatalf("a plain name changed: %s", fileName("TestPlain_name"))
	}
}

func TestAn_unknown_mode_is_refused(t *testing.T) {
	t.Setenv(ModeEnv, "replay-please")
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown mode was accepted")
		}
	}()
	CurrentMode()
}

func TestA_plain_json_recording_is_still_read(t *testing.T) {
	inFreshDirectory(t)
	t.Setenv(ModeEnv, "")
	path := filepath.Join(Directory, fileName(t.Name()))
	if err := os.MkdirAll(Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"note": "", "answers": {"k": "txt:plain"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Answer(t, "k", nil); string(got) != "plain" {
		t.Fatalf("read %q", got)
	}
}
