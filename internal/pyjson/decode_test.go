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
