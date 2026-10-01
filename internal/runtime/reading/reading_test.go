package reading_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// The ordered partition: established absence, something whose shape cannot be read, a
// question that could not be asked, and a record that was read, each its own answer.
func TestReadJSONKeepsFourAnswers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.json", `{"a": 1}`)
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "loop"), filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, state, exception, detail string
	}{
		{good, reading.Present, "", ""},
		{filepath.Join(dir, "missing.json"), reading.Absent, "", "nothing exists at " + filepath.Join(dir, "missing.json")},
		{dir, reading.Unreadable, "", "this path is a directory, not a regular file"},
		{filepath.Join(dir, "fifo"), reading.Unreadable, "", "this path is a named pipe, not a regular file"},
		{filepath.Join(dir, "dangling"), reading.Unreadable, "FileNotFoundError", "a symbolic link whose target does not exist"},
		{filepath.Join(dir, "loop"), reading.Unreadable, "OSError", "a symbolic link that loops"},
		{write("broken.json", "{ not json"), reading.Unreadable, "JSONDecodeError", "could not read the thing (invalid character 'o' in literal null (expecting 'u'))"},
		{write("latin.json", "\xff"), reading.Unreadable, "UnicodeDecodeError", ""},
		{filepath.Join(good, "child"), reading.AccessError, "NotADirectoryError", ""},
		{"nul\x00path", reading.Unreadable, "ValueError", "this path cannot name a file (embedded null byte)"},
	} {
		got := reading.ReadJSON(c.path, "the thing", nil, nil)
		if got.State != c.state || got.Exception != c.exception || (c.detail != "" && got.Detail != c.detail) {
			t.Errorf("%s: %s %s %q", c.path, got.State, got.Exception, got.Detail)
		}
	}
	shaped := reading.ReadJSON(good, "the thing", nil, func(any) error { return reading.Fail("TypeError", "no") })
	if shaped.State != reading.Unreadable || shaped.Exception != "TypeError" || shaped.Detail != "could not read the thing (no)" || shaped.Identity == nil {
		t.Fatalf("a shape refusal: %+v", shaped)
	}
	refusal := shaped.Refusal()
	if len(refusal) != 6 || refusal[0].Key != "state" || refusal[3].Key != "raisedAt" || refusal[3].Value != nil {
		t.Fatalf("refusal shape %v", refusal)
	}
	if absent := reading.ReadJSON(filepath.Join(dir, "missing.json"), "x", func() any { return "empty" }, nil); absent.Value != "empty" {
		t.Fatal("established absence carries the caller's empty value")
	}
}

// Universal newlines apply before the JSON is read, as Python's text mode does.
func TestDecodeReadsUniversalNewlines(t *testing.T) {
	if _, err := reading.Decode([]byte("{\r\n\"a\": 1\r}")); err != nil {
		t.Fatal(err)
	}
	v, err := reading.Decode([]byte(`{"a": NaN, "a": 2}`))
	if o, ok := v.(contract.OrderedObject); err != nil || !ok || len(o) != 1 || o[0].Value != int64(2) {
		t.Fatalf("a repeated key keeps its last value: %v %v", v, err)
	}
	if _, err := reading.Decode([]byte("\ufeff{}")); err == nil {
		t.Fatal("a UTF-8 BOM is refused, as json.loads refuses it")
	}
}

func TestSameDirectoryAsksTheFilesystem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if !reading.SameDirectory(filepath.Join(root, "alias"), filepath.Join(root, "real")) {
		t.Fatal("an alias is the directory it names")
	}
	if reading.SameDirectory(root+"/missing/..", root) {
		t.Fatal("a path the kernel answers ENOENT for is not the same directory")
	}
}
