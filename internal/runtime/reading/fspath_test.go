package reading

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// store.FSDecode spells each byte that is not part of UTF-8 as its surrogate escape (os.fsdecode with
// surrogateescape, as WTF-8), leaves UTF-8 alone, and FSEncode gives the bytes back; a
// surrogate outside U+DC80..U+DCFF names no file.
func TestFSDecodeAndEncodeAreOSFsdecodeAndFsencode(t *testing.T) {
	for raw, want := range map[string]string{
		"/p/pol\x80icy.json": "/p/pol\xed\xb2\x80icy.json",
		"/p/\xe2\x82A":       "/p/\xed\xb3\xa2\xed\xb2\x82A",
		"/p/\xed\xa0\x80":    "/p/\xed\xb3\xad\xed\xb2\xa0\xed\xb2\x80",
		"/p/caf\xc3\xa9":     "/p/caf\xc3\xa9",
		"/p/\xff":            "/p/\xed\xb3\xbf",
	} {
		if got := store.FSDecode(raw); got != want {
			t.Errorf("FSDecode(%q) = %q, want %q", raw, got, want)
		}
		if back, ok := FSEncode(store.FSDecode(raw)); !ok || back != raw {
			t.Errorf("FSEncode(FSDecode(%q)) = %q %v", raw, back, ok)
		}
	}
	if _, ok := FSEncode("/p/\xed\xa0\x80"); ok {
		t.Error("a high surrogate was encoded")
	}
	if _, ok := FSEncode("/p/\xed\xb0\x80"); ok {
		t.Error("U+DC00, which escapes no byte, was encoded")
	}
}
