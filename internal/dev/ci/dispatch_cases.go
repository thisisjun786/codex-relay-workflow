//go:build dev

package ci

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The case sections of the dispatch-verification reference are generated from one file per entry
// under docs/crw-run/dispatch-cases. The document is part of the plugin payload, so it is committed
// and regenerated; the data files are not in the payload. Each section's kind is fixed in the table
// below and is never guessed from the section's content.
const (
	dispatchCasesDoc = "plugins/crw/skills/crw-run/references/dispatch-verification.md"
	dispatchCasesDir = "docs/crw-run/dispatch-cases"
	// dispatchCasesDriftMessage is what --check prints when the committed document and the data
	// files disagree. crw-dev ci validate runs the same check, so the drift fails CI.
	dispatchCasesDriftMessage = "edit the rows under docs/crw-run/dispatch-cases and run crw-dev ci dispatch-cases --write"
	// dispatchCasesLead opens a case block's first line: two asterisks, the id, a space, an em dash
	// (U+2014) and a space.
	dispatchCasesLead = "**"
	dispatchCasesDash = " — "
)

// dispatchCasesKind is how a section's generated region is read from and written to the document.
type dispatchCasesKind int

const (
	dispatchCasesTable  dispatchCasesKind = iota // one table row per file
	dispatchCasesBlocks                          // one case block per file
)

// dispatchCasesSection is one section of the document the generator owns.
type dispatchCasesSection struct {
	heading string
	dir     string
	kind    dispatchCasesKind
}

// dispatchCasesSections is the fixed section table. The heading is matched literally; a section the
// document does not hold is a refusal, not a guess.
var dispatchCasesSections = []dispatchCasesSection{
	{"## Recorded cases", "recorded", dispatchCasesTable},
	{"## Negative cases", "negative", dispatchCasesBlocks},
	{"## Contrast cases", "contrast", dispatchCasesBlocks},
}

var dispatchCasesName = regexp.MustCompile(`^([0-9]{4})-(.+)\.md$`)

// dispatchCasesCaseID is the id line's bold lead names, and whether line is a case block's first
// line at all: two asterisks, the id, a space, an em dash (U+2014) and a space.
func dispatchCasesCaseID(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, dispatchCasesLead)
	if !ok {
		return "", false
	}
	at := strings.Index(rest, dispatchCasesDash)
	if at <= 0 {
		return "", false
	}
	id := rest[:at]
	if strings.ContainsAny(id, " \t*") {
		return "", false
	}
	return id, true
}

// dispatchCasesSeparator reports whether line is a table's separator row.
func dispatchCasesSeparator(line string) bool {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") || !strings.Contains(line, "-") {
		return false
	}
	for _, r := range line {
		if r != '|' && r != '-' && r != ':' && r != ' ' {
			return false
		}
	}
	return true
}

// dispatchCasesDataFile reports whether name, a repository-relative path, is one of the data files of
// a section. They are generator inputs rather than documents: their relative links are written for
// the generated document's directory, and the generated document is itself link-checked, so a broken
// link in a row still fails validate through it. Anything else below dispatchCasesDir, a README for
// example, is an ordinary document and is link-checked.
func dispatchCasesDataFile(name string) bool {
	for _, section := range dispatchCasesSections {
		rest, ok := strings.CutPrefix(name, dispatchCasesDir+"/"+section.dir+"/")
		if ok && rest != "" && !strings.Contains(rest, "/") {
			return true
		}
	}
	return false
}

// dispatchCasesRegion is the half-open line range a section's generated region occupies in doc.
func dispatchCasesRegion(doc []string, section dispatchCasesSection) (int, int, error) {
	heading := -1
	for i, line := range doc {
		if line == section.heading {
			heading = i
			break
		}
	}
	if heading < 0 {
		return 0, 0, fmt.Errorf("%s: no %q section heading", dispatchCasesDoc, section.heading)
	}
	// A section ends at the next "## " heading, so neither kind can read into the next one.
	next := len(doc)
	for i := heading + 1; i < len(doc); i++ {
		if strings.HasPrefix(doc[i], "## ") {
			next = i
			break
		}
	}
	if section.kind == dispatchCasesTable {
		header := -1
		for i := heading + 1; i < next; i++ {
			if strings.HasPrefix(doc[i], "|") {
				header = i
				break
			}
		}
		if header < 0 || header+1 >= next || !dispatchCasesSeparator(doc[header+1]) {
			return 0, 0, fmt.Errorf("%s: %q has no table header", dispatchCasesDoc, section.heading)
		}
		end := next
		for i := header + 2; i < next; i++ {
			if doc[i] == "" {
				end = i
				break
			}
		}
		return header + 2, end, nil
	}
	first := -1
	for i := heading + 1; i < next; i++ {
		if _, ok := dispatchCasesCaseID(doc[i]); ok {
			first = i
			break
		}
	}
	if first < 0 {
		return 0, 0, fmt.Errorf("%s: %q has no case block", dispatchCasesDoc, section.heading)
	}
	end := next
	for end > first && doc[end-1] == "" {
		end--
	}
	return first, end, nil
}

// dispatchCasesRowLine is a recorded file's single table line, whose first cell names the file's ID.
func dispatchCasesRowLine(where, id, text string) (string, error) {
	body, ok := strings.CutSuffix(text, "\n")
	if !ok || strings.Contains(body, "\n") || !strings.HasPrefix(body, "|") {
		return "", fmt.Errorf("%s: not exactly one table line", where)
	}
	cell, _, found := strings.Cut(strings.TrimPrefix(body, "|"), "|")
	if !found || strings.TrimSpace(cell) != id {
		return "", fmt.Errorf("%s: the row's first cell names the file name's ID %q", where, id)
	}
	return body, nil
}

// dispatchCasesBlockLines is a case-block file's lines, which hold one case and nothing else.
func dispatchCasesBlockLines(where, id, text string) ([]string, error) {
	body, ok := strings.CutSuffix(text, "\n")
	if !ok {
		return nil, fmt.Errorf("%s: a block file ends with one newline", where)
	}
	block := lines(body)
	if block[0] == "" || block[len(block)-1] == "" {
		return nil, fmt.Errorf("%s: a block file starts and ends with a case line, not a blank one", where)
	}
	lead, ok := dispatchCasesCaseID(block[0])
	if !ok || lead != id {
		return nil, fmt.Errorf("%s: a block file's first line is the bold lead %q", where, dispatchCasesLead+id+dispatchCasesDash)
	}
	for _, line := range block[1:] {
		if _, ok := dispatchCasesCaseID(line); ok {
			return nil, fmt.Errorf("%s: a block file holds one case", where)
		}
	}
	return block, nil
}

// dispatchCasesRegionText is the lines the section's data files generate, in file-name order.
func dispatchCasesRegionText(root string, section dispatchCasesSection) ([]string, error) {
	dir := filepath.Join(root, dispatchCasesDir, section.dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("no data directory %s/%s", dispatchCasesDir, section.dir)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("%s/%s: a data directory holds entry files, not directories", dispatchCasesDir, section.dir)
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if section.kind == dispatchCasesBlocks && len(names) == 0 {
		// A case-block section is located by its first bold-lead case, so an empty one could not be
		// regenerated after --write removed every case from the document.
		return nil, fmt.Errorf("%s/%s: a case-block section holds at least one case file", dispatchCasesDir, section.dir)
	}
	var region []string
	for _, name := range names {
		where := filepath.Join(dispatchCasesDir, section.dir, name)
		match := dispatchCasesName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%s: a data file's name is <order>-<ID>.md", where)
		}
		text, err := readText(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if section.kind == dispatchCasesTable {
			line, err := dispatchCasesRowLine(where, match[2], text)
			if err != nil {
				return nil, err
			}
			region = append(region, line)
			continue
		}
		block, err := dispatchCasesBlockLines(where, match[2], text)
		if err != nil {
			return nil, err
		}
		if len(region) > 0 {
			region = append(region, "")
		}
		region = append(region, block...)
	}
	return region, nil
}

// dispatchCasesGenerate is the document the data files under root produce.
func dispatchCasesGenerate(root string) (string, error) {
	text, err := readText(filepath.Join(root, dispatchCasesDoc))
	if err != nil {
		return "", err
	}
	if strings.Contains(text, "\r") {
		// The generator splices lines and rejoins them with LF, so a CRLF reference would come back
		// with its prose rewritten. Refuse it instead of silently changing bytes outside the regions.
		return "", fmt.Errorf("%s: the reference must use LF line endings", dispatchCasesDoc)
	}
	trailing := strings.HasSuffix(text, "\n")
	doc := lines(strings.TrimSuffix(text, "\n"))
	type edit struct {
		start, end int
		region     []string
	}
	edits := make([]edit, 0, len(dispatchCasesSections))
	for _, section := range dispatchCasesSections {
		start, end, err := dispatchCasesRegion(doc, section)
		if err != nil {
			return "", err
		}
		region, err := dispatchCasesRegionText(root, section)
		if err != nil {
			return "", err
		}
		edits = append(edits, edit{start, end, region})
	}
	// The regions were found in the document as committed. Splice from the last one backwards so the
	// earlier offsets stay valid whatever order the headings sit in.
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]string(nil), doc...)
	for _, e := range edits {
		next := make([]string, 0, len(out)-(e.end-e.start)+len(e.region))
		next = append(next, out[:e.start]...)
		next = append(next, e.region...)
		next = append(next, out[e.end:]...)
		out = next
	}
	generated := strings.Join(out, "\n")
	if trailing {
		generated += "\n"
	}
	return generated, nil
}

// dispatchCasesVerify is the drift check crw-dev ci validate runs.
func dispatchCasesVerify(root string) error {
	path := filepath.Join(root, dispatchCasesDoc)
	if _, err := os.Stat(path); err != nil {
		// The component is absent, so validate claims no coverage for it, as the contract checks do.
		return nil
	}
	generated, err := dispatchCasesGenerate(root)
	if err != nil {
		return err
	}
	current, err := readText(path)
	if err != nil {
		return err
	}
	if current != generated {
		return errors.New(dispatchCasesDoc + ": " + dispatchCasesDriftMessage)
	}
	return nil
}

// DispatchCases is the "crw-dev ci dispatch-cases" check: rewrite the generated case sections of
// the dispatch-verification reference from the data files under docs/crw-run/dispatch-cases, or
// refuse when the committed document and those files disagree (--check, the default).
func DispatchCases(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("dispatch-cases")
	write := flags.Bool("write", false, "rewrite the document from the data files")
	check := flags.Bool("check", false, "refuse when the document and the data files disagree (the default)")
	if code := parseFlags(flags, "Generate the dispatch-verification case sections from the data files under "+dispatchCasesDir+", or check that the committed document matches them.", args, stdout, stderr); code >= 0 {
		return code
	}
	if *write && *check {
		return failf(stderr, "dispatch-cases: --write and --check are exclusive")
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "dispatch-cases: %s", err)
	}
	generated, err := dispatchCasesGenerate(root)
	if err != nil {
		return failf(stderr, "dispatch-cases: %s", err)
	}
	path := filepath.Join(root, dispatchCasesDoc)
	current, err := readText(path)
	if err != nil {
		return failf(stderr, "dispatch-cases: %s", err)
	}
	if !*write {
		if current != generated {
			return failf(stderr, "dispatch-cases: %s", dispatchCasesDriftMessage)
		}
		fmt.Fprintf(stdout, "%s matches %s"+"\n", dispatchCasesDoc, dispatchCasesDir)
		return 0
	}
	if current == generated {
		fmt.Fprintf(stdout, "%s is already generated from %s"+"\n", dispatchCasesDoc, dispatchCasesDir)
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return failf(stderr, "dispatch-cases: %s", err)
	}
	if err := os.WriteFile(path, []byte(generated), info.Mode().Perm()); err != nil {
		return failf(stderr, "dispatch-cases: %s", err)
	}
	fmt.Fprintf(stdout, "%s regenerated from %s"+"\n", dispatchCasesDoc, dispatchCasesDir)
	return 0
}
