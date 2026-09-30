package pyjson

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
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

// DecodeBytesWTF8 decodes what DecodeBytes decodes and fails where it fails, keeping each lone
// surrogate as its three WTF-8 bytes where DecodeBytes has U+FFFD: from raw UTF-8 bytes passed by
// surrogatepass, from a UTF-16 unit left unpaired and from a UTF-32 surrogate value, while a
// UTF-16 pair is the one character it encodes.
func TestDecodeBytesWTF8_keeps_each_lone_surrogate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []byte
		want  string
	}{
		{"utf-8", []byte("\"\xed\xa0\x80x\xed\xb3\xbf\""), "\"\xed\xa0\x80x\xed\xb3\xbf\""},
		{"utf-8-sig", []byte("\xef\xbb\xbf\"\xed\xa0\x80\""), "\"\xed\xa0\x80\""},
		{"utf-16-le", []byte{'"', 0, 0x00, 0xd8, 'x', 0, 0x00, 0xdc, '"', 0}, "\"\xed\xa0\x80x\xed\xb0\x80\""},
		{"utf-16-be pair", []byte{0, '"', 0xd8, 0x3d, 0xde, 0x00, 0, '"'}, "\"\U0001f600\""},
		{"utf-16 bom", []byte{0xfe, 0xff, 0, '"', 0xdf, 0xff, 0, '"'}, "\"\xed\xbf\xbf\""},
		{"utf-32-be", []byte{0, 0, 0, '"', 0, 0, 0xd8, 0x00, 0, 0, 0, '"'}, "\"\xed\xa0\x80\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeBytesWTF8(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("%q (%v), want %q", got, err, tc.want)
			}
			if text, err := DecodeBytes(tc.input); err != nil || surrogatesReplaced(got) != text {
				t.Fatalf("DecodeBytes %q (%v)", text, err)
			}
		})
	}
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
		text, textErr := DecodeBytes(input)
		wtf8, err := DecodeBytesWTF8(input)
		if (err == nil) != (textErr == nil) || err != nil && err.Error() != textErr.Error() {
			t.Errorf("%s: %v where DecodeBytes gave %v", name, err, textErr)
			continue
		}
		if replaced := surrogatesReplaced(wtf8); err == nil && replaced != text {
			t.Errorf("%s: %q is not %q with its surrogates kept", name, wtf8, text)
		}
	}
}

// surrogatesReplaced is s with each WTF-8 surrogate written as U+FFFD.
func surrogatesReplaced(s string) string {
	var b []rune
	for i := 0; i < len(s); {
		if i+3 <= len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf {
			b = append(b, 0xfffd)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b = append(b, r)
		i += size
	}
	return string(b)
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
