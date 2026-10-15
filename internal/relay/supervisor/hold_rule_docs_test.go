package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHoldRuleDocsTableMatchesDecision (CRW-1500 U9): the hold rule table of
// docs/relay/supervisor-channel.md ("The hold rule") and holdDecision state one table. Each body row
// of the documented table names an entry and the change it is asked after; each cell starts with the
// verdict the documentation gives (set, keep, release, refuse, if owed, or "same statement: keep,
// else if owed"), and anything after the verdict is explanation. Every cell is compared with what
// holdDecision answers for that entry, that hold kind and every change the row covers, with owed
// answering true and false. This is a structure check: the docs and the code say the same thing.
// What the paths do with the verdict is proved by TestSupervisorHoldRuleTable's store-backed rows.
func TestHoldRuleDocsTableMatchesDecision(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "relay", "supervisor-channel.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := holdRuleDocsTable(string(data))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		label := row[0]
		changes, ok := holdRuleDocsRows[label]
		if !ok {
			t.Errorf("the documented hold table has a row %q the rule test does not know", label)
			continue
		}
		if seen[label] {
			t.Errorf("the documented hold table has the row %q twice", label)
		}
		seen[label] = true
		for i, kind := range holdRuleDocsKinds {
			cell := row[i+1]
			for _, change := range changes {
				if problem := holdRuleDocsCellMatches(cell, kind, change); problem != "" {
					t.Errorf("row %q, kind %s (%+v): the docs say %q, %s", label, kind, change, cell, problem)
				}
			}
		}
	}
	for label := range holdRuleDocsRows {
		if !seen[label] {
			t.Errorf("the documented hold table has no row %q", label)
		}
	}
}

// holdRuleDocsKinds are the hold kind columns of the documented table, in order.
var holdRuleDocsKinds = []holdKind{holdNone, holdOther, holdDerived, holdAcknowledged, holdNotice}

// holdRuleDocsRows are the documented row labels and every change each row covers. A row holds for
// every change listed, so a column that does not matter to it is listed with both values.
var holdRuleDocsRows = func() map[string][]holdChange {
	each := func(entries []holdEntry, words []string, stmt, rcpt, sndr []bool) []holdChange {
		var out []holdChange
		for _, e := range entries {
			for _, w := range words {
				for _, st := range stmt {
					for _, r := range rcpt {
						for _, s := range sndr {
							out = append(out, holdChange{Entry: e, SetWord: w, SameStatement: st, SameRecipient: r, SameSender: s})
						}
					}
				}
			}
		}
		return out
	}
	both, yes, no := []bool{false, true}, []bool{true}, []bool{false}
	anyWords := []string{"", "busy_cap"}
	return map[string][]holdChange{
		"acknowledge, notice_park, derive":      each([]holdEntry{entryAcknowledge, entryNoticePark, entryDerive}, anyWords, both, both, both),
		"defer":                                 each([]holdEntry{entryDefer}, anyWords, both, both, both),
		"claim":                                 each([]holdEntry{entryClaim}, anyWords, both, both, both),
		"settle, a hold word asked":             each([]holdEntry{entrySettle}, []string{"busy_cap"}, both, both, both),
		"settle, no hold word asked":            each([]holdEntry{entrySettle}, []string{""}, both, both, both),
		"readdress, recipient moved":            each([]holdEntry{entryReaddress}, anyWords, both, no, both),
		"readdress, sender moved only":          each([]holdEntry{entryReaddress}, anyWords, both, yes, no),
		"restage, recipient moved":              each([]holdEntry{entryRestage}, anyWords, both, no, both),
		"restage, same recipient":               each([]holdEntry{entryRestage}, anyWords, both, yes, both),
		"send_by_id, recipient or sender moved": append(each([]holdEntry{entrySendByID}, anyWords, both, no, both), each([]holdEntry{entrySendByID}, anyWords, both, yes, no)...),
		"send_by_id, same endpoints":            each([]holdEntry{entrySendByID}, anyWords, both, yes, yes),
	}
}()

// holdRuleDocsCellMatches is "" when holdDecision answers what the documented cell says for kind
// after change, else what differs.
func holdRuleDocsCellMatches(cell string, kind holdKind, change holdChange) string {
	token, err := holdRuleDocsToken(cell)
	if err != nil {
		return err.Error()
	}
	ask := func(owed bool) (holdVerdict, error) {
		v, _, err := holdDecision(kind, change, func() (bool, error) { return owed, nil })
		return v, err
	}
	expect := func(want holdVerdict) string {
		for _, owed := range []bool{true, false} {
			if got, err := ask(owed); err != nil || got != want {
				return "holdDecision answers " + string(got) + " with owed " + map[bool]string{true: "true", false: "false"}[owed]
			}
		}
		return ""
	}
	ifOwed := func() string {
		if got, err := ask(true); err != nil || got != holdRelease {
			return "holdDecision answers " + string(got) + " when owed, not release"
		}
		if got, err := ask(false); err != nil || got != holdKeep {
			return "holdDecision answers " + string(got) + " when not owed, not keep"
		}
		return ""
	}
	switch token {
	case "same statement: keep, else if owed":
		if change.SameStatement {
			return expect(holdKeep)
		}
		return ifOwed()
	case "if owed":
		return ifOwed()
	}
	return expect(holdVerdict(token))
}

// holdRuleDocsVerdicts are the verdicts a documented cell may start with, longest first.
var holdRuleDocsVerdicts = []string{"same statement: keep, else if owed", "if owed", "release", "refuse", "keep", "set"}

// holdRuleDocsToken is the verdict a documented cell starts with. Backticks and emphasis are
// dropped first; what follows the verdict must start with a space, a comma or a parenthesis.
func holdRuleDocsToken(cell string) (string, error) {
	text := strings.TrimSpace(strings.NewReplacer("`", "", "**", "", "*", "").Replace(cell))
	for _, v := range holdRuleDocsVerdicts {
		if rest, ok := strings.CutPrefix(text, v); ok && (rest == "" || strings.ContainsRune(" ,(;", rune(rest[0]))) {
			return v, nil
		}
	}
	return "", errors.New("the cell does not start with a verdict")
}

// holdRuleDocsTable is the body rows of the first table under the heading "### The hold rule", each
// as its cells. The header must name the entry column and the five kinds in order.
func holdRuleDocsTable(doc string) ([][]string, error) {
	_, after, ok := strings.Cut(doc, "\n### The hold rule\n")
	if !ok {
		return nil, errors.New(`supervisor-channel.md has no "### The hold rule" section`)
	}
	if next := strings.Index(after, "\n#"); next >= 0 {
		after = after[:next]
	}
	var rows [][]string
	inTable := false
	for _, line := range strings.Split(after, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(strings.NewReplacer("`", "").Replace(cells[i]))
		}
		if !inTable {
			want := []string{"entry", "none", "other", "derived", "acknowledged", "notice"}
			if strings.Join(cells, "|") != strings.Join(want, "|") {
				return nil, errors.New("the hold table's header is " + line + ", want | entry | none | other | derived | acknowledged | notice |")
			}
			inTable = true
			continue
		}
		if strings.Trim(strings.Join(cells, ""), "-: ") == "" {
			continue
		}
		if len(cells) != 6 {
			return nil, errors.New("the hold table row " + line + " does not have six cells")
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return nil, errors.New("the hold rule section has no table")
	}
	return rows, nil
}
