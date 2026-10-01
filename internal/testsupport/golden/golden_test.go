package golden

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fresh runs a case from its own directory, so goldens and fixtures land in a scratch tree.
func fresh(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	forget()
}

// forget writes every golden held so far and drops them, as a new test process would.
func forget() {
	filesMu.Lock()
	defer filesMu.Unlock()
	for _, f := range files {
		if err := f.save(); err != nil {
			panic(err)
		}
		f.dirty = false
	}
	files = map[string]*file{}
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

func fatalOf(t testing.TB, check func(testing.TB)) string {
	f := &fatalRecorder{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil && r != f {
				panic(r)
			}
		}()
		check(f)
	}()
	return f.message
}

func TestUpdate_then_check_passes_and_a_change_fails_with_the_first_difference(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "update")
	Check(t, "out", []byte("one\ntwo\nthree\n"))
	forget()
	t.Setenv(ModeEnv, "")
	Check(t, "out", []byte("one\ntwo\nthree\n"))
	message := fatalOf(t, func(tb testing.TB) { Check(tb, "out", []byte("one\nTWO\nthree\n")) })
	if !strings.Contains(message, "line 2 want: two") || !strings.Contains(message, "line 2 got:  TWO") {
		t.Fatalf("difference not shown: %q", message)
	}
}

func TestA_missing_value_names_the_file_the_key_and_the_update_mode(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "")
	message := fatalOf(t, func(tb testing.TB) { Check(tb, "absent", []byte("x")) })
	if !strings.Contains(message, `"absent"`) || !strings.Contains(message, Directory) || !strings.Contains(message, ModeEnv+"=update") {
		t.Fatalf("message %q", message)
	}
}

func TestSubstitutions_keep_run_paths_out_of_the_golden(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "update")
	path := filepath.Join(Directory, fileName(t.Name())+".json.gz")
	Check(t, "state", []byte("db at /tmp/run-1/relay.sqlite3"), Substitute("/tmp/run-1", "<TMP>"))
	forget()
	stored, err := readGzip(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "/tmp/run-1") || !strings.Contains(string(stored), "<TMP>") {
		t.Fatalf("the golden keeps the run path: %s", stored)
	}
	t.Setenv(ModeEnv, "")
	Check(t, "state", []byte("db at /tmp/run-2/relay.sqlite3"), Substitute("/tmp/run-2", "<TMP>"))
}

func TestCompare_replaces_byte_equality(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "update")
	Check(t, "k", []byte("12:00"))
	forget()
	t.Setenv(ModeEnv, "")
	sameLength := Compare(func(want, got []byte) error {
		if len(want) != len(got) {
			return fmt.Errorf("length %d, want %d", len(got), len(want))
		}
		return nil
	})
	Check(t, "k", []byte("13:01"), sameLength)
	if message := fatalOf(t, func(tb testing.TB) { Check(tb, "k", []byte("1:01"), sameLength) }); !strings.Contains(message, "length 4") {
		t.Fatalf("message %q", message)
	}
}

func TestCheckJSON_sorts_keys_and_keeps_numbers(t *testing.T) {
	encoded, err := Encode(map[string]any{"b": 1, "a": "x<y"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{\n  \"a\": \"x<y\",\n  \"b\": 1\n}\n" {
		t.Fatalf("encoded %q", encoded)
	}
	fresh(t)
	t.Setenv(ModeEnv, "update")
	CheckJSON(t, "v", map[string]any{"b": 1, "a": 2})
	forget()
	t.Setenv(ModeEnv, "")
	CheckJSON(t, "v", map[string]any{"a": 2, "b": 1})
}

func TestWant_returns_the_golden_and_calls_produce_only_when_updating(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "update")
	if got := Want(t, "k", func() []byte { return []byte("value") }); string(got) != "value" {
		t.Fatalf("update returned %q", got)
	}
	forget()
	t.Setenv(ModeEnv, "")
	if got := Want(t, "k", func() []byte { panic("produce called outside update") }); string(got) != "value" {
		t.Fatalf("read back %q", got)
	}
}

func TestBinary_values_round_trip(t *testing.T) {
	fresh(t)
	t.Setenv(ModeEnv, "update")
	Check(t, "b", []byte{0xff, 0x00, 0xfe})
	forget()
	t.Setenv(ModeEnv, "")
	Check(t, "b", []byte{0xff, 0x00, 0xfe})
}

func TestFixture_reads_a_plain_or_gzip_file(t *testing.T) {
	fresh(t)
	if err := os.MkdirAll(FixtureDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(FixtureDirectory, "plain.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(FixtureDirectory, "packed.sqlite3.gz"))
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(out)
	if _, err := writer.Write([]byte("packed")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if got := Fixture(t, "plain.txt"); string(got) != "plain" {
		t.Fatalf("plain read %q", got)
	}
	if got := Fixture(t, "packed.sqlite3"); string(got) != "packed" {
		t.Fatalf("gzip read %q", got)
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
	}
}

func TestAn_unknown_mode_is_refused(t *testing.T) {
	t.Setenv(ModeEnv, "record")
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown mode was accepted")
		}
	}()
	Updating()
}
