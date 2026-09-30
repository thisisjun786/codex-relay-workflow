package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PyRepr is repr() of a str as CPython 3.14.4 prints it: its quote choice, its escapes of every
// character str.isprintable() refuses, and a lone surrogate (an argv byte that is not UTF-8, or
// the WTF-8 form a JSON decoder keeps) as \udXXX. Each expectation is that interpreter's answer.
func TestPyReprIsPythonsReprOfAStr(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"plain", `'plain'`},
		{"x\u00a0y", `'x\xa0y'`},
		{"x\u2028y", `'x\u2028y'`},
		{"x\u200by", `'x\u200by'`},
		{"x\xffy", `'x\udcffy'`},
		{"x\xed\xb3\xbfy", `'x\udcffy'`},
		{"it's", `"it's"`},
		{"a\"b'c", `'a"b\'c'`},
		{"x\u0c5cy", `'x\u0c5cy'`}, // assigned in Unicode 17, unassigned in CPython 3.14's 16.0.0
		{"caf\u00e9 \u3042", "'caf\u00e9 \u3042'"},
		{"t\tn\nr\r\\\x01\x7f", `'t\tn\nr\r\\\x01\x7f'`},
		{"\U0001f600\U000e0001", "'\U0001f600\\U000e0001'"},
	} {
		if got := PyRepr(c.text); got != c.want {
			t.Errorf("PyRepr(%q) = %s, want %s", c.text, got, c.want)
		}
	}
}

// The hold's refusal names the store path as intent.registration_hold does, "the relay store path
// " + repr(db_path) + " could not be read as a path": Python's quote choice and its escapes, not a
// hand-made quoting. The path is under the live state directory, which Go refuses to hold under
// test isolation.
func TestTheHoldNamesAPathItCannotReadAsPythonReprsIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
	for _, name := range []string{"it's\u00a0x", `back\slash`, "l\u2028s", "b\xffyte"} {
		directory := filepath.Join(root, "xdg", "codex-session-relay", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "relay.sqlite3")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		want := "the relay store path " + pythonStoreValue(t, "import sys; print(repr(sys.argv[1]))", path) + " could not be read as a path"
		hold, unavailable := HoldForWrite(t.Context(), path, time.Second)
		if hold != nil {
			_ = hold.Release()
			t.Fatalf("held a store under the live state directory: %s", path)
		}
		if unavailable != want {
			t.Errorf("%q:\n go     %s\n python %s", name, unavailable, want)
		}
	}
}
