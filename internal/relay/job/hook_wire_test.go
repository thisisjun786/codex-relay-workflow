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
// so the budget of the text a drain hands over is the size it has in that spelling. A drain that cannot describe every due job inside
// it describes the first ones that fit, each with its id and its result (and its get when the line leaves anything out), stamps only
// those, and the next drain hands over the others.
func TestDrainBudgetIsMeasuredInTheSpellingTheRelayWrites(t *testing.T) {
	const limit = 4096
	emoji := strings.Repeat("\U0001F600", 38)
	note := strings.Repeat("\U0001F600", 30)
	for _, asJSON := range []bool{false, true} {
		ws := workspace(t)
		var all []string
		for i := range 5 {
			// 201 bytes, so the temporary name of a write still fits; 600 bytes and more as the relay writes it, twice in a line.
			id := strings.Repeat("\U0001F600", 50) + strconv.Itoa(i)
			all = append(all, id)
			r := hookDone(t, ws, id, "S1")
			r.Command, r.Note = []string{"echo", emoji}, sp(note)
			r.EndedAt = sp("2026-09-09T00:0" + strconv.Itoa(i) + ":00.000Z")
			save(t, ws, r)
		}
		drain := func() string {
			t.Helper()
			got, err := RunParsedCLI(CLIOptions{Verb: "drain", Session: sp("S1"), JSON: asJSON}, ws, func(string) (string, bool) { return "", false }, noon)
			if err != nil {
				t.Fatal(err)
			}
			text := got.Out
			if o, ok := got.Out.(pyjson.Object); ok {
				text = o.Get("text")
			}
			s, _ := text.(string)
			wire, err := pyjson.Encode(text, pyjson.Options{})
			if err != nil || !utf8.ValidString(s) || len(wire) > limit {
				t.Fatalf("json=%v: the drain text is %d bytes as the relay writes it (%v)", asJSON, len(wire), err)
			}
			return s
		}
		seen := map[string]bool{}
		for round := 0; len(seen) < len(all); round++ {
			if round == len(all) {
				t.Fatalf("json=%v: five drains handed over only %d jobs", asJSON, len(seen))
			}
			text := drain()
			shown := 0
			for _, id := range all {
				line, in := "", false
				for _, l := range strings.Split(text, "\n") {
					if strings.HasPrefix(l, "- "+id+" ("+string(StatusComplete)+", exit 0, ") {
						line, in = l, true
					}
				}
				// A line that leaves the command or the note out points at the job's get.
				if in && !strings.HasSuffix(line, " — echo "+emoji+" ["+note+"]") && !strings.HasSuffix(line, "(전체: crw relay job get "+id+")") {
					t.Errorf("json=%v: job %s leaves something out and has no get pointer: %q", asJSON, id[len(id)-1:], line)
				}
				if in && seen[id] {
					t.Errorf("json=%v: job %s was handed over twice", asJSON, id[len(id)-1:])
				}
				if !seen[id] && in != delivered(t, ws, id) {
					t.Errorf("json=%v: job %s shown %v, delivered %v", asJSON, id[len(id)-1:], in, delivered(t, ws, id))
				}
				if in {
					seen[id], shown = true, shown+1
				}
			}
			if shown == 0 || !strings.Contains(text, strconv.Itoa(shown)+"건이 끝났습니다") {
				t.Fatalf("json=%v: a drain described %d jobs:\n%q", asJSON, shown, text)
			}
			if round == 0 && shown == len(all) {
				t.Errorf("json=%v: the first drain described all five jobs; the budget was not reached", asJSON)
			}
		}
		if text := drain(); asJSON && text != "" || !asJSON && text != "미전달 완료 없음" {
			t.Errorf("json=%v: a drain after every job was handed over: %q", asJSON, text)
		}
	}
}
