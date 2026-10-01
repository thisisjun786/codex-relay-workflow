//go:build dev

package pyload

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// A record reads as an earlier writer could have written it: the constants json.dumps writes,
// a lone surrogate escape, an integer past int64, and a repeated key's last value at its first
// place.
func TestLoadsKeepsWhatAnEarlierWriterWrote(t *testing.T) {
	v, err := Loads([]byte(`[NaN, Infinity, -Infinity, 9223372036854775807, 9223372036854775808, 0.5]`))
	if err != nil {
		t.Fatal(err)
	}
	items := v.([]any)
	if f, ok := items[0].(float64); !ok || !math.IsNaN(f) {
		t.Errorf("NaN: %#v", items[0])
	}
	if items[1] != math.Inf(1) || items[2] != math.Inf(-1) {
		t.Errorf("infinities: %#v %#v", items[1], items[2])
	}
	if items[3] != int64(math.MaxInt64) || items[4] != json.Number("9223372036854775808") || items[5] != 0.5 {
		t.Errorf("numbers: %#v", items[3:])
	}
	v, err = Loads([]byte(`{"a": 1, "b": "\ud800", "a": 2}`))
	if err != nil {
		t.Fatal(err)
	}
	object := v.(contract.OrderedObject)
	if len(object) != 2 || object[0].Key != "a" || object[0].Value != int64(2) || object[1].Value != "\xed\xa0\x80" {
		t.Errorf("object: %#v", object)
	}
}

func nested(open, fill, close string, depth int) string {
	return strings.Repeat(open, depth) + fill + strings.Repeat(close, depth)
}

// A record reads as deep as a Python writer could have written it, past encoding/json's 10000,
// and one container deeper is refused, however deep the document goes; a bracket inside a string
// is no container.
func TestLoadsReadsAsDeepAsAWriterWrote(t *testing.T) {
	for _, doc := range []string{
		nested("[", "", "]", MaxNesting),
		nested(`{"a": `, "1", "}", MaxNesting),
		`["` + strings.Repeat("[", MaxNesting+1) + `\"["]`,
	} {
		if _, err := Loads([]byte(doc)); err != nil {
			t.Errorf("%.20q (%d bytes): %v", doc, len(doc), err)
		}
	}
	for _, doc := range []string{
		nested("[", "", "]", MaxNesting+1),
		nested(`{"a": `, "1", "}", MaxNesting+1),
		nested("[", "", "]", 2_000_000),
	} {
		if _, err := Loads([]byte(doc)); err == nil || err.Error() != "the record nests deeper than 57900 containers" {
			t.Errorf("%.20q (%d bytes): %v", doc, len(doc), err)
		}
	}
}

// A record that is not UTF-8 or not JSON is refused in encoding/json's words.
func TestLoadsRefuses(t *testing.T) {
	for _, c := range []struct{ doc, message string }{
		{"\xff", "the record is not UTF-8 text"},
		{"[\"\xed\xa0\x80\"]", "the record is not UTF-8 text"},
		{`{"a" 1}`, "invalid character '1' after object key"},
		{`[1, 2`, "unexpected end of JSON input"},
		{`{"a": "b"} x`, "trailing data after the JSON value"},
	} {
		if _, err := Loads([]byte(c.doc)); err == nil || err.Error() != c.message {
			t.Errorf("%.40q: %v, want %q", c.doc, err, c.message)
		}
	}
}
