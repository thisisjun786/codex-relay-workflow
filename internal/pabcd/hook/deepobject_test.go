package hook

import (
	"reflect"
	"testing"
)

// deepObject replaces editObject for the goal and automation gates (CRW-1075); below encoding/json's
// depth limit the two read every payload alike.
func TestDeepObjectReadsLikeEditObjectBelowTheDepthLimit(t *testing.T) {
	for _, doc := range []string{
		`{}`, ` {"a":1} `, `{"a":1,"a":2}`, `{"n":1e400,"m":-0,"k":12345678901234567890123}`,
		`{"s":"\ud800","t":"é\n","u":"a😀"}`, `{"x":[1,[2,{"y":null}],true,false]}`,
		`[]`, `null`, `"s"`, `12`, ``, `{`, `{"a":}`, `{"a":1}x`, `{"a":NaN}`, `{"a":01}`, "{\"a\":\"\t\"}", `{"a":1,}`,
		"\xef\xbb\xbf{}", `{"a":[]}`,
	} {
		want, got := editObject(doc), deepObject(doc)
		if !reflect.DeepEqual(want, got) || (want == nil) != (got == nil) {
			t.Errorf("%q: editObject %#v, deepObject %#v", doc, want, got)
		}
	}
}
