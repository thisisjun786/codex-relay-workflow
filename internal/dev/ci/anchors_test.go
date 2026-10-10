//go:build dev

package ci

import (
	"strings"
	"testing"
)

func TestAnchorSlug(t *testing.T) {
	for _, row := range []struct{ heading, slug string }{
		{"Plain Heading", "plain-heading"},
		{"Punctuation, (and) more: `code`!", "punctuation-and-more-code"},
		{"What's new in 1.2.3?", "whats-new-in-123"},
		{"A / B & C", "a--b--c"},
		{"[Link text](other.md) here", "link-text-here"},
		{"**Bold** and _emphasis_ and snake_case", "bold-and-emphasis-and-snake_case"},
		{"Subagent Skill Attachment (SEARCH-ATTACH-01)", "subagent-skill-attachment-search-attach-01"},
		{"한글 제목 (설명)", "한글-제목-설명"},
		{"<b>Tag</b> text", "tag-text"},
		{"under_score_ end", "under_score-end"},
	} {
		expectEqual(t, row.heading, anchorSlug(row.heading), row.slug)
	}
	// A repeated heading gets -1, -2 in document order; a heading that is already named like a
	// numbered slug is skipped over; explicit HTML anchors count; fenced code and frontmatter do not.
	text := "---\ntitle: Front\n---\n# Same\n\n## Same\n\n### Same\n\n## Same-1\n\n" +
		"<a id=\"explicit\"></a>\n<a name='old-style'></a>\n```md\n# Fenced\n```\nSetext\n======\n\n> ## Quoted\n"
	anchors := markdownAnchors(text)
	for _, want := range []string{"same", "same-1", "same-2", "same-1-1", "explicit", "old-style", "setext", "quoted"} {
		if !anchors[want] {
			t.Errorf("anchor %q missing from %v", want, anchors)
		}
	}
	for _, unwanted := range []string{"fenced", "front", "same-3", "title-front"} {
		if anchors[unwanted] {
			t.Errorf("anchor %q must not exist: %v", unwanted, anchors)
		}
	}
}

// anchorFixture is a repository with the documents in files; the first of a failing pair is the
// document, the rest the fixture's other files.
func anchorFixture(t *testing.T, files map[string]string) []string {
	t.Helper()
	r := newRepo(t)
	for name, text := range files {
		r.write(name, text)
	}
	names, err := repositoryFiles(r.root)
	if err != nil {
		t.Fatal(err)
	}
	errs, err := anchorErrors(r.root, names)
	if err != nil {
		t.Fatal(err)
	}
	return errs
}

func TestSkillAnchorsResolve(t *testing.T) {
	t.Run("fixture: a same-file anchor that names no heading fails", func(t *testing.T) {
		errs := anchorFixture(t, map[string]string{
			"plugins/crw/skills/a/SKILL.md": "# Title\n\n## Real heading\n\n[ok](#real-heading) [bad](#missing-heading)\n",
		})
		expectEqual(t, "errors", errs, []string{"plugins/crw/skills/a/SKILL.md:5: invalid anchor #missing-heading"})
	})
	t.Run("fixture: an anchor that the other file lacks fails", func(t *testing.T) {
		errs := anchorFixture(t, map[string]string{
			"docs/a.md": "[ok](b.md#target) [bad](b.md#nowhere) [file only](b.md) [code line](c.go#L3)\n",
			"docs/b.md": "# Target\n",
			"docs/c.go": "package c\n",
		})
		expectEqual(t, "errors", errs, []string{"docs/a.md:1: invalid anchor b.md#nowhere"})
	})
	t.Run("fixture: a repeated heading is numbered from -1", func(t *testing.T) {
		errs := anchorFixture(t, map[string]string{
			"docs/dup.md": "# Same\n\n## Same\n\n[first](#same) [second](#same-1) [third](#same-2)\n\n# Alone\n\n[none](#alone-1)\n",
		})
		expectEqual(t, "errors", errs, []string{"docs/dup.md:5: invalid anchor #same-2", "docs/dup.md:9: invalid anchor #alone-1"})
	})
	t.Run("fixture: outside the scope, in fences and in generator inputs nothing is read", func(t *testing.T) {
		errs := anchorFixture(t, map[string]string{
			"README.md":                         "[bad](#nothing)\n",
			"docs/fenced.md":                    "```md\n[bad](#nothing)\n```\n",
			"docs/port/refactor-backlog.d/x.md": "[bad](#nothing)\n",
			"docs/web.md":                       "[remote](https://example.invalid/a.md#nothing) [mail](mailto:a@b.c#x)\n",
		})
		expectEqual(t, "errors", errs, []string(nil))
	})
	t.Run("the tree has no broken anchor", func(t *testing.T) {
		root := repoRoot()
		names, err := repositoryFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		errs, err := anchorErrors(root, names)
		if err != nil {
			t.Fatal(err)
		}
		if len(errs) > 0 {
			t.Errorf("%d broken anchors:\n%s", len(errs), strings.Join(errs, "\n"))
		}
	})
}

func TestAnchorsCheckedInGeneratedDispatchDoc(t *testing.T) {
	// A data file's links are written for the generated document's directory: at its own path
	// they would not resolve, so they are not read there; the generated document is.
	dataFile := dispatchCasesDir + "/recorded/0001-case.md"
	if !dispatchCasesDataFile(dataFile) || anchorChecked(dataFile) {
		t.Fatalf("%s must be a generator input that the anchor check skips", dataFile)
	}
	if !anchorScope(dispatchCasesDoc) || !anchorChecked(dispatchCasesDoc) {
		t.Fatalf("%s must be anchor-checked", dispatchCasesDoc)
	}
	errs := anchorFixture(t, map[string]string{
		dataFile:         "[row](other.md#there) [bad relative to the data file](../../../../README.md#nowhere)\n",
		dispatchCasesDoc: "# Doc\n\n[ok](other.md#there) [bad](other.md#gone) [self](#doc)\n",
		"plugins/crw/skills/crw-run/references/other.md": "## There\n",
	})
	expectEqual(t, "errors", errs, []string{dispatchCasesDoc + ":3: invalid anchor other.md#gone"})
	// The checked-in document: every anchor in it resolves, and the data files exist as inputs.
	root := repoRoot()
	names, err := repositoryFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	data, doc := 0, false
	for _, name := range names {
		doc = doc || name == dispatchCasesDoc
		if dispatchCasesDataFile(name) {
			data++
		}
	}
	if !doc || data == 0 {
		t.Fatalf("the generated document %v and %d data files are expected in the tree", doc, data)
	}
	found, err := linkErrors(root, dispatchCasesDoc, map[string]map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "checked-in document", found, []string(nil))
}

// crw-dev ci validate runs the anchor check on the documents in scope, and leaves the others alone.
func TestValidateRefusesABrokenAnchor(t *testing.T) {
	r := validateRepo(t)
	r.write("docs/target.md", "# Target\n\n## Same\n\n## Same\n")
	r.write("docs/source.md", "[a](target.md#target) [b](target.md#same-1) [c](#own)\n\n## Own\n")
	r.write("README.md", "[outside the scope](docs/target.md#nowhere)\n")
	if got := validate(t, r); got.code != 0 || !strings.HasPrefix(got.stdout, validated) || got.stderr != "" {
		t.Fatalf("valid anchors: %+v", got)
	}
	r.write("docs/source.md", "[a](target.md#target) [b](target.md#same-2) [c](#own)\n\n## Own\n")
	r.write("plugins/crw/skills/example/references/x.md", "[d](../../../../../docs/target.md#gone)\n")
	got := validate(t, r)
	want := "docs/source.md:1: invalid anchor target.md#same-2\n" +
		"plugins/crw/skills/example/references/x.md:1: invalid anchor ../../../../../docs/target.md#gone\n"
	expectEqual(t, "broken anchors", got, result{1, "", want})
}
