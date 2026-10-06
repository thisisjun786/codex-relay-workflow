//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture covers both section kinds: a table section with an intro paragraph before its header,
// and two case-block sections, one of which has a case whose block holds an inner blank line. The
// prose around each generated region is the part a rewrite must leave alone.
const dispatchCasesFixtureDoc = `# Fixture reference

Opening prose that must not change.

## Recorded cases

Recorded intro prose before the table.

| Case | What it decides |
|---|---|
| R1 | first row |
| R2 | second row |

Trailing prose after the table.

## Negative cases

Negative intro prose before the first case.

**N1 — first negative.** First line
continues here.

**N2 — second negative.** A second
paragraph follows.

still N2's text.

## Contrast cases

**C1 — only contrast.** Body of C1.

## Limits

Tail prose.
`

// dispatchCasesFixtureData is the data tree that generates dispatchCasesFixtureDoc.
func dispatchCasesFixtureData() map[string]string {
	return map[string]string{
		"recorded/0010-R1.md": "| R1 | first row |\n",
		"recorded/0020-R2.md": "| R2 | second row |\n",
		"negative/0010-N1.md": "**N1 — first negative.** First line\ncontinues here.\n",
		"negative/0020-N2.md": "**N2 — second negative.** A second\nparagraph follows.\n\nstill N2's text.\n",
		"contrast/0010-C1.md": "**C1 — only contrast.** Body of C1.\n",
	}
}

// writeDispatchCasesTree lays out a fixture checkout under a fresh directory.
func writeDispatchCasesTree(t *testing.T, doc string, data map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, text string) {
		t.Helper()
		target := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dispatchCasesDoc, doc)
	for rel, text := range data {
		write(filepath.Join(dispatchCasesDir, rel), text)
	}
	return root
}

func dispatchCasesFixture(t *testing.T) string {
	t.Helper()
	return writeDispatchCasesTree(t, dispatchCasesFixtureDoc, dispatchCasesFixtureData())
}

// generateDispatchCases is dispatchCasesGenerate with the error handled, for the tests that expect success.
func generateDispatchCases(t *testing.T, root string) string {
	t.Helper()
	got, err := dispatchCasesGenerate(root)
	if err != nil {
		t.Fatalf("dispatchCasesGenerate: %v", err)
	}
	return got
}

// refuseDispatchCases requires dispatchCasesGenerate to fail with an error naming want.
func refuseDispatchCases(t *testing.T, root, want string) {
	t.Helper()
	_, err := dispatchCasesGenerate(root)
	if err == nil {
		t.Fatalf("dispatchCasesGenerate: no error, want one naming %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("dispatchCasesGenerate: %v, want one naming %q", err, want)
	}
}

// The migration is byte-identical: the data tree reproduces the document it was taken from.
func TestDispatchCasesGenerateReproducesTheDocument(t *testing.T) {
	expectEqual(t, "generated document", generateDispatchCases(t, dispatchCasesFixture(t)), dispatchCasesFixtureDoc)
}

// A rewrite touches the generated regions and nothing else: the heading, the two intro paragraphs,
// the prose between the sections and the tail all come back unchanged.
func TestDispatchCasesGenerateChangesOnlyTheGeneratedRegions(t *testing.T) {
	data := dispatchCasesFixtureData()
	data["recorded/0015-R15.md"] = "| R15 | inserted |\n"
	data["negative/0015-N15.md"] = "**N15 — inserted.** Inserted body.\n"
	root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data)
	want := strings.Replace(dispatchCasesFixtureDoc,
		"| R1 | first row |\n| R2 | second row |\n",
		"| R1 | first row |\n| R15 | inserted |\n| R2 | second row |\n", 1)
	want = strings.Replace(want,
		"**N1 — first negative.** First line\ncontinues here.\n\n**N2 — second negative.**",
		"**N1 — first negative.** First line\ncontinues here.\n\n**N15 — inserted.** Inserted body.\n\n**N2 — second negative.**", 1)
	expectEqual(t, "generated document", generateDispatchCases(t, root), want)
}

// The order is the file name's, not the creation order: 0015 lands between 0010 and 0020.
func TestDispatchCasesGenerateOrdersByFileName(t *testing.T) {
	data := dispatchCasesFixtureData()
	delete(data, "recorded/0010-R1.md")
	delete(data, "recorded/0020-R2.md")
	data["recorded/0030-R3.md"] = "| R3 | third |\n"
	data["recorded/0010-R1.md"] = "| R1 | first |\n"
	data["recorded/0020-R2.md"] = "| R2 | second |\n"
	doc := strings.Replace(dispatchCasesFixtureDoc,
		"| R1 | first row |\n| R2 | second row |\n",
		"| R1 | first |\n| R2 | second |\n| R3 | third |\n", 1)
	root := writeDispatchCasesTree(t, doc, data)
	expectEqual(t, "generated document", generateDispatchCases(t, root), doc)
}

// A block with two paragraphs keeps its inner blank line when it is written back.
func TestDispatchCasesGenerateKeepsABlocksInnerBlankLine(t *testing.T) {
	generated := generateDispatchCases(t, dispatchCasesFixture(t))
	if !strings.Contains(generated, "paragraph follows.\n\nstill N2's text.\n") {
		t.Errorf("the two-paragraph block lost its inner blank line:\n%s", generated)
	}
}

// A section the document does not hold is refused rather than guessed at.
func TestDispatchCasesRefusesAMissingSectionHeading(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc, "## Negative cases", "## Negatives", 1)
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	refuseDispatchCases(t, root, "## Negative cases")
}

// A table section without its header and separator lines is refused.
func TestDispatchCasesRefusesAMissingTableHeader(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc, "| Case | What it decides |\n|---|---|\n", "", 1)
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	refuseDispatchCases(t, root, "## Recorded cases")
}

// A case-block section with no bold-lead case is refused.
func TestDispatchCasesRefusesACaseBlockSectionWithNoCase(t *testing.T) {
	doc := strings.Replace(dispatchCasesFixtureDoc,
		"**N1 — first negative.** First line\ncontinues here.\n\n**N2 — second negative.** A second\nparagraph follows.\n\nstill N2's text.\n",
		"Only prose, no case.\n", 1)
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	refuseDispatchCases(t, root, "## Negative cases")
}

// A recorded file that is not exactly one table line is refused.
func TestDispatchCasesRefusesAMalformedRecordedFile(t *testing.T) {
	cases := map[string]string{
		"two lines":   "| R1 | first row |\n| R2 | second row |\n",
		"not a table": "R1 is not a table line\n",
		"no newline":  "| R1 | first row |",
		"empty":       "",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			data := dispatchCasesFixtureData()
			data["recorded/0010-R1.md"] = text
			root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data)
			refuseDispatchCases(t, root, "0010-R1.md")
		})
	}
}

// A block file whose first line is not the bold lead naming its own id, that holds a second
// bold-lead case, or that starts or ends with a blank line is refused.
func TestDispatchCasesRefusesAMalformedBlockFile(t *testing.T) {
	cases := map[string]string{
		"no lead":        "N1 is not a lead line\n",
		"wrong id":       "**N9 — another case.** Body.\n",
		"second lead":    "**N1 — first negative.** Body.\n\n**N2 — second negative.** More.\n",
		"leading blank":  "\n**N1 — first negative.** Body.\n",
		"trailing blank": "**N1 — first negative.** Body.\n\n",
		"no newline":     "**N1 — first negative.** Body.",
		"empty":          "",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			data := dispatchCasesFixtureData()
			data["negative/0010-N1.md"] = text
			root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data)
			refuseDispatchCases(t, root, "0010-N1.md")
		})
	}
}

// A data directory that is missing is refused rather than treated as an empty section.
func TestDispatchCasesRefusesAMissingDataDirectory(t *testing.T) {
	root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, map[string]string{
		"recorded/0010-R1.md": "| R1 | first row |\n",
	})
	refuseDispatchCases(t, root, "negative")
}

// The command writes the document from the data tree and refuses a document that drifted from it.
func TestDispatchCasesCommandWriteAndCheck(t *testing.T) {
	repo := newRepo(t)
	repo.write(dispatchCasesDoc, dispatchCasesFixtureDoc)
	for rel, text := range dispatchCasesFixtureData() {
		repo.write(filepath.Join(dispatchCasesDir, rel), text)
	}
	repo.commit()

	if r := goCheck(t, repo.root, nil, "dispatch-cases", "--check"); r.code != 0 {
		t.Fatalf("--check on an equal document: code %d, stderr %q", r.code, r.stderr)
	}
	repo.write(filepath.Join(dispatchCasesDir, "recorded", "0010-R1.md"), "| R1 | changed |\n")
	if r := goCheck(t, repo.root, nil, "dispatch-cases", "--check"); r.code != 1 || !strings.Contains(r.stderr, dispatchCasesDriftMessage) {
		t.Fatalf("--check on drift: code %d, stderr %q, want the drift message", r.code, r.stderr)
	}
	if r := goCheck(t, repo.root, nil, "dispatch-cases", "--write"); r.code != 0 {
		t.Fatalf("--write: code %d, stderr %q", r.code, r.stderr)
	}
	if r := goCheck(t, repo.root, nil, "dispatch-cases", "--check"); r.code != 0 {
		t.Fatalf("--check after --write: code %d, stderr %q", r.code, r.stderr)
	}
	want := strings.Replace(dispatchCasesFixtureDoc, "| R1 | first row |", "| R1 | changed |", 1)
	got, err := readText(filepath.Join(repo.root, dispatchCasesDoc))
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "document after --write", got, want)
}

// The repository's own rows and blocks regenerate its committed document byte for byte.
func TestDispatchCasesRepositoryDocumentIsGenerated(t *testing.T) {
	root := repoRoot()
	want, err := readText(filepath.Join(root, dispatchCasesDoc))
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, dispatchCasesDoc, generateDispatchCases(t, root), want)
}

// A recorded file whose first cell names another case is refused: the file name and the row must agree.
func TestDispatchCasesRefusesARecordedFileWhoseIDDiffers(t *testing.T) {
	data := dispatchCasesFixtureData()
	data["recorded/0010-R1.md"] = "| R2 | another row |\n"
	root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data)
	refuseDispatchCases(t, root, "0010-R1.md")
}

// A directory inside a data directory is refused rather than silently dropped.
func TestDispatchCasesRefusesADirectoryInADataDirectory(t *testing.T) {
	root := dispatchCasesFixture(t)
	if err := os.MkdirAll(filepath.Join(root, dispatchCasesDir, "recorded", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	refuseDispatchCases(t, root, "recorded")
}

// An empty case-block directory is refused: after --write removed every case, the section could not be
// located again, so the next check could never pass.
func TestDispatchCasesRefusesAnEmptyCaseBlockDirectory(t *testing.T) {
	data := dispatchCasesFixtureData()
	delete(data, "negative/0010-N1.md")
	delete(data, "negative/0020-N2.md")
	root := writeDispatchCasesTree(t, dispatchCasesFixtureDoc, data)
	refuseDispatchCases(t, root, "negative")
}

// A CRLF reference is refused instead of having its prose rewritten to LF.
func TestDispatchCasesRefusesACRLFDocument(t *testing.T) {
	doc := strings.ReplaceAll(dispatchCasesFixtureDoc, "\n", "\r\n")
	root := writeDispatchCasesTree(t, doc, dispatchCasesFixtureData())
	refuseDispatchCases(t, root, "LF line endings")
}

// The sections may sit in any order: each region is spliced by its own position, not by the order the
// section table happens to name.
func TestDispatchCasesGenerateHandlesReorderedSections(t *testing.T) {
	doc := `# Fixture reference

## Contrast cases

**C1 — only contrast.** Body of C1.

## Recorded cases

| Case | What it decides |
|---|---|
| R1 | first row |

## Negative cases

Negative intro prose before the first case.

**N1 — first negative.** First line
continues here.

**N2 — second negative.** A second
paragraph follows.

still N2's text.

## Limits

Tail prose.
`
	data := map[string]string{
		"contrast/0010-C1.md":  "**C1 — only contrast.** Body of C1.\n",
		"recorded/0010-R1.md":  "| R1 | first row |\n",
		"negative/0010-N1.md":  "**N1 — first negative.** First line\ncontinues here.\n",
		"negative/0015-N15.md": "**N15 — inserted.** Inserted body.\n",
		"negative/0020-N2.md":  "**N2 — second negative.** A second\nparagraph follows.\n\nstill N2's text.\n",
	}
	want := strings.Replace(doc,
		"continues here.\n\n**N2 — second negative.**",
		"continues here.\n\n**N15 — inserted.** Inserted body.\n\n**N2 — second negative.**", 1)
	root := writeDispatchCasesTree(t, doc, data)
	expectEqual(t, "generated document", generateDispatchCases(t, root), want)
}
