package job

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// CRW-1095, post-evaluation round (f24d11cd): the budget of a drain's text is measured as the relay writes it.

// The relay writes the drain's text with Python's ensure_ascii, where a character past U+007E is \uXXXX (two escapes beyond the BMP),
// so the budget of the text a drain hands over is the size it has in that spelling.
func TestDrainBudgetIsMeasuredInTheSpellingTheRelayWrites(t *testing.T) {
	const limit = 4096
	ws := workspace(t)
	emoji := strings.Repeat("\U0001F600", 38)
	note := strings.Repeat("\U0001F600", 30)
	for i := range 5 {
		r := hookDone(t, ws, "job"+strconv.Itoa(i), "S1")
		r.Command, r.Note = []string{"echo", emoji}, sp(note)
		r.EndedAt = sp("2026-09-09T00:0" + strconv.Itoa(i) + ":00.000Z")
		save(t, ws, r)
	}
	for _, asJSON := range []bool{false, true} {
		got, err := RunParsedCLI(CLIOptions{Verb: "drain", Session: sp("S1"), JSON: asJSON}, ws, func(string) (string, bool) { return "", false }, noon)
		if err != nil {
			t.Fatal(err)
		}
		text := got.Out
		if o, ok := got.Out.(pyjson.Object); ok {
			text = o[1].Value
		}
		wire, err := pyjson.Encode(text, pyjson.Options{})
		if s, _ := text.(string); err != nil || !utf8.ValidString(s) || len(wire) > limit {
			t.Errorf("json=%v: the drain text is %d bytes as the relay writes it (%v)", asJSON, len(wire), err)
		}
		for i := range 5 { // the call stamped what it described; put them back for the other form
			r, _ := ReadRecord(ws, "job"+strconv.Itoa(i))
			r.DeliveredAt = nil
			save(t, ws, r)
		}
	}
}
