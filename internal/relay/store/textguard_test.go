package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// EncodeUTF8 is str.encode("utf-8") of the str Python holds: an argv byte that is not UTF-8 is its
// surrogate escape and a WTF-8 surrogate (a "\udXXX" JSON escape) is its code point, and the
// refusal names the first run of surrogates by code point, one character alone or a run by its
// first and last position, in CPython's words.
func TestEncodeUTF8IsStrEncode(t *testing.T) {
	t.Parallel()
	cases := []struct{ goText, python string }{
		{"x\xffy", `os.fsdecode(b"x\xffy")`},
		{"\xfe\xffz", `os.fsdecode(b"\xfe\xffz")`},
		{"é\xff", `os.fsdecode(b"\xc3\xa9\xff")`},
		{"a\xed\xb3\xbfb", `"a\udcffb"`},
		{"ab\xed\xb3\xbf\xff\xed\xa0\x80c", `"ab\udcff" + os.fsdecode(b"\xff") + "\ud800c"`},
		{"a\xed\xa0\x80x\xed\xb3\xbf\xffb\xfe", `"a\ud800x\udcff" + os.fsdecode(b"\xffb\xfe")`},
		{"\U0001f600\xff", `"\U0001f600" + os.fsdecode(b"\xff")`},
		{"plain", `"plain"`},
	}
	var refusals [][2]any
	for _, c := range cases {
		var refusal any
		if err := EncodeUTF8(c.goText); err != nil {
			refusal = err.Error()
		}
		refusals = append(refusals, [2]any{c.python, refusal})
	}
	// str.encode("utf-8") of each text, as CPython answered it (the golden).
	checkJSON(t, "str, UnicodeEncodeError", refusals)
}

// A store connection binds a string argument as Python's sqlite3 binds a str: one it cannot encode
// as UTF-8 is refused with that UnicodeEncodeError before SQLite sees it, whether it is written or
// looked up and whether it arrives bare or through a driver.Valuer, so no TEXT Python's sqlite3
// cannot decode is ever written; every other string, and bytes, bind as they always did.
func TestAStoreRefusesToBindAStrPythonCannotEncode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, arg := range []any{"T\xff", sql.NullString{String: "T\xed\xb3\xbf", Valid: true}} {
		_, err := s.DB.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('probe', ?)", arg)
		if refused := EncodeError(err); refused == nil || refused.HostDetail() != "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 1: surrogates not allowed" {
			t.Errorf("binding %#v: %v", arg, err)
		}
		var found int
		if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM schema_meta WHERE value = ?", arg).Scan(&found); EncodeError(err) == nil {
			t.Errorf("looking %#v up: %v", arg, err)
		}
	}
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('probe', ?)", "Tÿ"); err != nil {
		t.Fatalf("a UTF-8 string: %v", err)
	}
	if _, err = s.DB.ExecContext(ctx, "INSERT INTO schema_meta VALUES ('blob', ?)", []byte("T\xff")); err != nil {
		t.Fatalf("bytes: %v", err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM schema_meta WHERE key IN ('probe', 'blob')").Scan(&count); err != nil || count != 2 {
		t.Fatalf("rows written: %d, %v", count, err)
	}
}
