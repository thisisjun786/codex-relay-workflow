package recall

import (
	"path/filepath"
	"testing"
)

// CRW-1123 :392 -- SQLite percent-decodes the key and the value of a URI parameter, so an encoded mode is a mode.
func TestReadOnlyURIRefusesEncodedAndRepeatedModes(t *testing.T) {
	for _, uri := range []string{
		"file:verifier?%6Dode=memory",
		"file:verifier?mode=%6Demory",
		"file:verifier?%6D%6F%64%65=%72%77%63",
		"file:verifier?mode=ro&mode=memory",
		"file:verifier?mode=ro&%6Dode=rwc",
		"file:verifier?mode=%72o&mode=rw",
		"file:verifier?cache=shared&mode=ro%00&mode=memory",
	} {
		db, err := openDbReadOnly(uri)
		if err == nil {
			_ = db.Close()
			t.Errorf("%s: the read-only API opened it", uri)
		}
	}
	path := filepath.Join(t.TempDir(), "ro.db")
	rw, err := openDbReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"file:" + path + "?mode=ro", "file:" + path + "?%6Dode=%72o", "file:" + path + "?mode=ro&mode=%72o&cache=private"} {
		db, err := openDbReadOnly(uri)
		if err != nil {
			t.Errorf("%s: %v", uri, err)
			continue
		}
		_ = db.Close()
	}
}

// CRW-1123 :449 -- a literal percent and a preserved escape are different spellings of different paths.
func TestRepoKeyLiteralPercentIsNotAnEncodedSeparator(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://example.test/a%2Fb", "https://example.test/a%252Fb"},
		{"https://example.test/a%5Cb", "https://example.test/a%255Cb"},
		{"https://example.test/a%2Fb", "example.test:a%2Fb"}, // scp has no decoding: its %2F is a literal one.
		{"https://example.test/a%2Fb", "git@example.test:a%2Fb"},
		{"https://example.test/a/b", "https://example.test/a%252Fb"},
	} {
		a, b := normalizeRepoKey(pair[0]), normalizeRepoKey(pair[1])
		if a == "" || b == "" || a == b {
			t.Errorf("%s -> %q and %s -> %q must be two keys", pair[0], a, pair[1], b)
		}
	}
	for _, pair := range [][2]string{
		{"https://example.test/a%252Fb", "git@example.test:a%2Fb"},
		{"https://example.test/a%25b.git", "example.test:a%b"},
		{"https://example.test/a%2Fb", "https://example.test/a%2fb"},
		{"https://example.test/caf%C3%A9", "https://example.test/café"},
	} {
		if a, b := normalizeRepoKey(pair[0]), normalizeRepoKey(pair[1]); a == "" || a != b {
			t.Errorf("%s -> %q and %s -> %q must be one key", pair[0], a, pair[1], b)
		}
	}
	if got := normalizeRepoKey("https://example.test/a%252Fb"); got != "example.test/a%252Fb" {
		t.Error(got)
	}
}
