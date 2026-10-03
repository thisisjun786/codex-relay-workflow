package recall

import (
	"reflect"
	"strings"
	"testing"
)

// CXC recall/test/crlf-recall.test.ts:37-54, with fixed expected entries as well as LF/CRLF equality.
func TestRolloutCRLF(t *testing.T) {
	lf := "{\"type\":\"response_item\",\"timestamp\":\"2026-08-21T00:00:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"first\"}]}}\n" +
		"{\"type\":\"response_item\",\"timestamp\":\"2026-08-21T00:00:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"second\"}]}}\n"
	want := []ChatEntry{
		{TS: "2026-08-21T00:00:00Z", Role: "user", Text: "first", MatchField: "content"},
		{TS: "2026-08-21T00:00:00Z", Role: "user", Text: "second", MatchField: "content"},
	}
	for _, doc := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		got, err := ParseRollout(doc, true)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("rollout entries = %#v, %v; want %#v", got, err, want)
		}
	}
}
