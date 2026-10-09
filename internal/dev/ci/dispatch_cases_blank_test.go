//go:build dev

package ci

import (
	"strings"
	"testing"
)

// CRW-969 (merged into CRW-968): a line of only spaces is a blank line wherever the generator judges one, and a CR in a data file is refused where it is read, so --write never writes what the next
// run refuses.

// A whitespace-only line after the table ends the generated region as a blank one does: the prose after it survives a rewrite.
func TestDispatchCasesTableEndsAtAWhitespaceOnlyLine(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc, "| R2 | second row |\n\nTrailing prose", "| R2 | second row |\n  \t\nTrailing prose after the blank.\n\nOld paragraph that must stay.\n\nTrailing prose", 1)
	data := dispatchCasesFixtureData()
	data["recorded/0030-R3.md"] = "| R3 | third row |\n"
	root := writeDispatchCasesTree(t, doc, data)
	want := strings.Replace(doc, "| R2 | second row |\n", "| R2 | second row |\n| R3 | third row |\n", 1)
	expectEqual(t, "generated document", generateDispatchCases(t, root), want)
}

// A whitespace-only line at the end of a case-block section is trailing blank space, not a part of the last case.
func TestDispatchCasesBlockSectionEndIgnoresWhitespaceOnlyLines(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc, "**C1 — only contrast.** Body of C1.\n\n## Limits", "**C1 — only contrast.** Body of C1.\n   \n\n## Limits", 1)
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	expectEqual(t, "generated document", generateDispatchCases(t, root), doc)
}

// A block file that starts or ends with a whitespace-only line is refused like one that starts or ends with an empty line.
func TestDispatchCasesRefusesABlockFileWithAWhitespaceOnlyEdgeLine(t *testing.T) {
	for name, text := range map[string]string{
		"end":   "**N1 — first negative.** First line\ncontinues here.\n  \n",
		"start": "  \n**N1 — first negative.** First line\ncontinues here.\n",
	} {
		data := dispatchCasesFixtureData()
		data["negative/0010-N1.md"] = text
		refuseDispatchCases(t, writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data), "a block file starts and ends with a case line")
		_ = name
	}
}

// CRLF data is refused when it is read, with the same rule the document has; --write never gets to write a CR.
func TestDispatchCasesRefusesCRInDataFiles(t *testing.T) {
	for file, text := range map[string]string{
		"recorded/0010-R1.md": "| R1 | first row |\r\n",
		"negative/0010-N1.md": "**N1 — first negative.** First line\r\ncontinues here.\r\n",
		"contrast/0010-C1.md": "**C1 — only contrast.** Body of C1.\r\n",
	} {
		data := dispatchCasesFixtureData()
		data[file] = text
		refuseDispatchCases(t, writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data), "must use LF line endings")
	}
}

// The end rule of a case-block section (CRW-800): the last case runs to the section's last non-blank line, so a paragraph typed after it is part of that case. --check calls it drift and --write
// replaces it with the case's data file; prose belongs in a data file's case or before the first case.
func TestDispatchCasesLastCaseRunsToTheSectionEnd(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc, "**C1 — only contrast.** Body of C1.\n", "**C1 — only contrast.** Body of C1.\n\nA paragraph typed after the last case.\n", 1)
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	expectEqual(t, "generated document", generateDispatchCases(t, root), dispatchCasesFixtureDoc)
	if err := dispatchCasesVerify(root); err == nil || !strings.Contains(err.Error(), dispatchCasesDriftMessage) {
		t.Errorf("dispatchCasesVerify: %v, want the drift message", err)
	}
}
