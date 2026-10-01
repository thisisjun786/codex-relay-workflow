package pyjson

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type capturedDecode struct {
	Hex         string  `json:"hex"`
	Text        *string `json:"text"`
	DecodeError string  `json:"decodeError"`
	HookedError *string `json:"hookedError"`
}

// Every byte string testdata/decode_capture.py gave json.loads: DecodeBytes decodes it to
// Python's text, or refuses it with str(UnicodeDecodeError), and HookedError refuses the text
// where json.loads(raw, object_pairs_hook=<refuse a repeated key>) does, first failure first.
func TestDecodeBytes_and_HookedError_are_json_loads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "decode.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]capturedDecode{}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		input, err := hex.DecodeString(c.Hex)
		if err != nil {
			t.Fatal(err)
		}
		text, err := DecodeBytes(input)
		if c.Text == nil {
			if err == nil || err.Error() != c.DecodeError {
				t.Errorf("%s: decode error %v, python %q", name, err, c.DecodeError)
			}
			continue
		}
		if err != nil || text != *c.Text {
			t.Errorf("%s: decoded %q (%v), python %q", name, text, err, *c.Text)
			continue
		}
		message, duplicate, _ := HookedError(text, 0)
		want := ""
		if c.HookedError != nil {
			want = *c.HookedError
		}
		if got := map[bool]string{true: "duplicate", false: message}[duplicate]; got != want {
			t.Errorf("%s: hooked parse %q, python %q", name, got, want)
		}
	}
}

// json.loads checks for a byte order mark only on a str argument. Bytes are decoded first
// (utf-8-sig drops one mark), so a mark left in the decoded text is an ordinary character the
// scanner refuses where a value was expected: json.loads(b"\xef\xbb\xbf\xef\xbb\xbf{}") raises
// "Expecting value", json.loads("\ufeff{}") the mark's own refusal.
func TestDecodedError_scans_a_mark_left_by_the_codec_as_a_character(t *testing.T) {
	text, err := DecodeBytes([]byte("\xef\xbb\xbf\xef\xbb\xbf{}"))
	if err != nil || text != "\ufeff{}" {
		t.Fatalf("decoded %q, %v", text, err)
	}
	if got := DecodedError(text); got != "Expecting value: line 1 column 1 (char 0)" {
		t.Errorf("bytes: %q", got)
	}
	if got := Error(text); got != "Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)" {
		t.Errorf("str: %q", got)
	}
	if got := DecodedError("{}"); got != "" {
		t.Errorf("a valid document: %q", got)
	}
}
