//go:build dev

package ci

import (
	"os"
	"path/filepath"
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
		{"under_score_ end", "under_score_-end"},
		{"__init__ method", "init-method"},
		{"Fish &amp; Chips", "fish--chips"},
		{"Fish &#38; Chips &copy; &unknown; x", "fish--chips--unknown-x"},
		{"*a **b** c* and ***d***", "a-b-c-and-d"},
		{"a * b * c and 2*3*4", "a--b--c-and-234"},
		{"snake_case_name and _x_", "snake_case_name-and-x"},
		{"\\*not emphasis\\* here", "not-emphasis-here"},
		{"`code with *star*` and **bold `code`**", "code-with-star-and-bold-code"},
		{"![alt *text*](img.png) after", "alt-text-after"},
		{"[**linked** text](a_b.md#c) and <http://x.example/a_b> end", "linked-text-and-httpxexamplea_b-end"},
		{"Ünïcode Straße", "ünïcode-straße"},
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

// A setext heading is the whole paragraph above its underline; HTML that is not an element with an
// id or name attribute, or that sits in code, names no anchor.
func TestMarkdownAnchorsBlocks(t *testing.T) {
	has := func(text string, want []string, unwanted []string) {
		t.Helper()
		anchors := markdownAnchors(text)
		for _, name := range want {
			if !anchors[name] {
				t.Errorf("%q: anchor %q missing from %v", text, name, anchors)
			}
		}
		for _, name := range unwanted {
			if anchors[name] {
				t.Errorf("%q: anchor %q must not exist: %v", text, name, anchors)
			}
		}
	}
	has("Foo\nBar\n---\n", []string{"foo-bar"}, []string{"bar", "foo"})
	has("  Foo *bar\n  baz*\n===\n", []string{"foo-bar-baz"}, []string{"baz"})
	has("Before\n\nFoo\nBar\n===\n", []string{"foo-bar"}, []string{"bar", "before"})
	has("Text\n\n* * *\n\nAfter\n---\n", []string{"after"}, []string{"text", "text-after"})
	has("- item\n---\n", nil, []string{"item"})
	has("- item\nlazy\n---\n", nil, []string{"lazy", "item-lazy"})
	has("| a |\n---\n", nil, []string{"a", "-a"})
	has("<div data-id=\"ghost\"></div>\n", nil, []string{"ghost"})
	has("<div id=\"real\" data-id=\"ghost\"></div> <a name='old'></a> <span\tID = plain>\n", []string{"real", "old", "plain"}, []string{"ghost"})
	has("The text `<a id=\"ghost\"></a>` and ``<a id=\"spans`\"></a>``, but <a id=\"live\"></a>\n", []string{"live"}, []string{"ghost", "spans"})
	has("Paragraph\n\n    <a id=\"ghost\"></a>\n    # Not a heading\n", nil, []string{"ghost", "not-a-heading"})
	has("Paragraph\n    <a id=\"continued\"></a>\n", []string{"continued"}, nil)
	has("\t<a id=\"ghost\"></a>\n", nil, []string{"ghost"})
	has("- item\n\n      <a id=\"ghost\"></a>\n\n  <a id=\"inside\"></a>\n\n1. one\n\n   <a id=\"numbered\"></a>\n", []string{"inside", "numbered"}, []string{"ghost"})
	has("    ```\n# After the indented marker\n", []string{"after-the-indented-marker"}, nil)
	has("## Title <a id=\"in-heading\"></a>\n", []string{"in-heading", "title"}, nil)
}

// A full or collapsed reference link in a heading renders as its text, whatever the label of its
// definition; an undefined label stays literal text. An attribute value is read with its character
// references decoded, and an HTML comment holds no element.
func TestMarkdownAnchorsReferencesEntitiesComments(t *testing.T) {
	has := func(text string, want []string, unwanted []string) {
		t.Helper()
		anchors := markdownAnchors(text)
		for _, name := range want {
			if !anchors[name] {
				t.Errorf("%q: anchor %q missing from %v", text, name, anchors)
			}
		}
		for _, name := range unwanted {
			if anchors[name] {
				t.Errorf("%q: anchor %q must not exist: %v", text, name, anchors)
			}
		}
	}
	has("## [Install][guide]\n\n[guide]: https://example.invalid/\n", []string{"install"}, []string{"installguide"})
	has("## [Install][Guide Label] now\n\n[guide  label]: <https://example.invalid/> \"t\"\n", []string{"install-now"}, nil)
	has("## [Install][] now\n\n[install]: https://example.invalid/\n", []string{"install-now"}, nil)
	has("## [Install] now\n\n[install]: https://example.invalid/\n", []string{"install-now"}, nil)
	has("## ![Logo][img] here\n\n[img]: logo.png\n", []string{"logo-here"}, nil)
	has("## [Install][undefined]\n", []string{"installundefined"}, []string{"install"})
	has("<a id=\"real&#45;target\"></a>\n<a name='x&amp;y'></a> <b id=a&#x2d;b></b>\n", []string{"real-target", "x&y", "a-b"}, []string{"real&#45;target"})
	has("<div>\n<!-- <a id=\"gone\"></a> -->\n<a id=\"kept\"></a>\n</div>\n", []string{"kept"}, []string{"gone"})
	has("<div>\n<!--\n<a id=\"gone\"></a>\n-->\n<a id=\"kept\"></a>\n</div>\n", []string{"kept"}, []string{"gone"})
	has("<div>\n<!-- never closed <a id=\"gone\"></a>\n</div>\n", nil, []string{"gone"})
	has("Text <!-- <a id=\"gone\"></a> --> <a id=\"kept\"></a>\n", []string{"kept"}, []string{"gone"})
}

// A line is a link reference definition only when the whole CommonMark form holds: a destination
// (closed if in <>, parentheses balanced), an optional title after white space, and nothing after
// them on the line; otherwise it is paragraph text and a reference link to its label stays literal.
// An HTML comment starts only where markup can start, never inside a quoted attribute value.
func TestMarkdownAnchorsDefinitionSyntaxAndCommentContext(t *testing.T) {
	has := func(text string, want []string, unwanted []string) {
		t.Helper()
		anchors := markdownAnchors(text)
		for _, name := range want {
			if !anchors[name] {
				t.Errorf("%q: anchor %q missing from %v", text, name, anchors)
			}
		}
		for _, name := range unwanted {
			if anchors[name] {
				t.Errorf("%q: anchor %q must not exist: %v", text, name, anchors)
			}
		}
	}
	literal := func(definition string) {
		t.Helper()
		has("## [Install][guide]\n\n"+definition, []string{"installguide"}, []string{"install"})
	}
	reference := func(definition string) {
		t.Helper()
		has("## [Install][guide]\n\n"+definition, []string{"install"}, []string{"installguide"})
	}
	// Not definitions: trailing text, an unclosed destination or title, unbalanced parentheses.
	literal("[guide]: https://example.invalid/ trailing\n")
	literal("[guide]: <https://example.invalid/\n")
	literal("[guide]: <https://example.invalid/>trailing\n")
	literal("[guide]: /url \"title\" trailing\n")
	literal("[guide]: /url \"unclosed title\n")
	literal("[guide]: /url 'title\n\nstill open'\n")
	literal("[guide]: /u(rl\n")
	literal("[guide]: <bar>(baz)\n")
	literal("[guide]:\n")
	literal("[guide]:\n\n/url\n")
	literal("[guide]: <a\nb>\n")
	literal("[guide] : /url\n")
	literal("Text first\n[guide]: /url\n")
	literal("[other]: /url\n[guide]: /url trailing\n")
	literal("[]: /url\n")
	// Definitions: the destination or the title on the next line, a title over lines, an empty <>,
	// a title that fails on its own line leaving the definition before it, a chain of definitions.
	reference("[guide]: https://example.invalid/\n")
	reference("[guide]:\n/url\n")
	reference("[guide]: /url\n'multi\nline title'\n")
	reference("[guide]: /url (title)   \n")
	reference("[guide]: <>\n")
	reference("[guide]: /u(r)l\n")
	reference("[guide]: /url\n\"title\" trailing\n")
	reference("[other]: /a\n[guide]: /b \"t\"\n")
	has("## [Install][gui de]\n\n[gui\nde]: /url\n", []string{"install"}, []string{"installgui-de"})
	has("## [Install][guide]\n\n[guide]: /url\n\"title\" ok\n", []string{"install"}, nil)

	// Comment markers inside quoted attribute values are part of the value.
	has("Text <a title='<!--' id=\"kept\"></a> and <a id=\"after\"></a> -->\n", []string{"kept", "after"}, nil)
	has("<div>\n<a title='<!--' id=\"kept\"></a>\n<a id=\"after\"></a>\n</div>\n", []string{"kept", "after"}, nil)
	has("Text <a id='a<!--x-->b'></a>\n", []string{"a<!--x-->b"}, []string{"ab", "a b"})
	has("<div>\n<a id='a<!--x-->b'></a>\n</div>\n", []string{"a<!--x-->b"}, []string{"ab", "a b"})
	// A backslash-escaped < in a paragraph is a literal character: it opens no comment and no element.
	has("Text \\<!-- <a id=\"shown\"></a> -->\n", []string{"shown"}, nil)
	has("Text \\<a id=\"literal\"></a>\n", nil, []string{"literal"})
	// Real comments still hide what they hold.
	has("Text <a title='x'></a><!-- <a id=\"gone\"></a> --> <a id=\"kept\"></a>\n", []string{"kept"}, []string{"gone"})
	has("<div>\n<a title=\"-->\" id=\"kept\"></a>\n<!-- <a id=\"gone\"></a>\n</div>\n", []string{"kept"}, []string{"gone"})
}

// A link label holds at most 999 characters, counted as characters, not UTF-8 bytes: a label of 999
// non-ASCII characters defines its reference, one of 1000 characters does not; a backslash escape
// counts as its two characters.
func TestMarkdownAnchorsReferenceLabelLength(t *testing.T) {
	for _, row := range []struct {
		char string
		size int
		ok   bool
	}{
		{"a", 999, true}, {"a", 1000, false},
		{"한", 334, true}, {"한", 400, true}, {"한", 999, true}, {"한", 1000, false},
		{`\]a`, 333, true}, {`\]a`, 334, false},
	} {
		label := strings.Repeat(row.char, row.size)
		anchors := markdownAnchors("## [Install][" + label + "]\n\n[" + label + "]: /url\n")
		if anchors["install"] != row.ok {
			t.Errorf("label of %d x %q: install anchor %v, want %v", row.size, row.char, anchors["install"], row.ok)
		}
	}
	label := strings.Repeat("한", 400)
	errs := anchorFixture(t, map[string]string{
		"docs/source.md": "[valid](target.md#install)\n",
		"docs/target.md": "## [Install][" + label + "]\n\n[" + label + "]: /url\n",
	})
	expectEqual(t, "errors", errs, []string(nil))
}

// The 999-character limit holds for the label of a full or collapsed reference too, counted on the
// label as written, before white space is collapsed: a usage label of 1000 characters or more is no
// reference, even when it normalizes to the label of a definition, so the heading keeps its text.
func TestMarkdownAnchorsOversizedReferenceUsage(t *testing.T) {
	korean := strings.TrimSpace(strings.Repeat("한 ", 400))        // 799 characters
	koreanUsage := strings.TrimSpace(strings.Repeat("한   ", 400)) // 1597 characters, the same label once normalized
	ascii := strings.TrimSpace(strings.Repeat("a ", 334))         // 667 characters
	asciiUsage := strings.TrimSpace(strings.Repeat("a  ", 334))   // 1000 characters, the same label once normalized
	for _, row := range []struct{ name, heading, definition string }{
		{"full", "[Install][" + koreanUsage + "]", korean},
		{"collapsed", "[" + koreanUsage + "][]", korean}, // read as text or as a link, its slug is the same
		{"full 1000", "[Install][" + asciiUsage + "]", ascii},
	} {
		target := "## " + row.heading + "\n\n[" + row.definition + "]: /url\n"
		literal := anchorSlug(row.heading)
		anchors := markdownAnchors(target)
		if !anchors[literal] || anchors["install"] {
			t.Errorf("%s: anchor of the literal heading present=%v, spurious install=%v", row.name, anchors[literal], anchors["install"])
		}
		errs := anchorFixture(t, map[string]string{
			"docs/source.md": "[valid](target.md#" + literal + ")\n",
			"docs/target.md": target,
		})
		expectEqual(t, row.name+" errors", errs, []string(nil))
	}
}

// A list marker needs a space, a tab or the line end after it, the content of a list item and of a
// block quote is read as blocks of its own, raw HTML blocks hold no Markdown, and a code span runs
// over the lines of its paragraph.
func TestMarkdownAnchorsContainers(t *testing.T) {
	has := func(text string, want []string, unwanted []string) {
		t.Helper()
		anchors := markdownAnchors(text)
		for _, name := range want {
			if !anchors[name] {
				t.Errorf("%q: anchor %q missing from %v", text, name, anchors)
			}
		}
		for _, name := range unwanted {
			if anchors[name] {
				t.Errorf("%q: anchor %q must not exist: %v", text, name, anchors)
			}
		}
	}
	// No list marker: these are setext headings.
	has("**Bold** title\n=====\n", []string{"bold-title"}, nil)
	has("*em* title\n=====\n", []string{"em-title"}, nil)
	has("-flag\n=====\n", []string{"-flag"}, nil)
	has("+1\n=====\n", []string{"1"}, nil)
	has("1.5\n=====\n", []string{"15"}, nil)
	has("- item\n---\n", nil, []string{"item"})
	has("1. item\n===\n", nil, []string{"item"})
	// The first line of a list item and of a block quote is a block.
	has("- # Heading\n\n- ```md\n  # Not a heading\n  ```\n", []string{"heading"}, []string{"not-a-heading"})
	has("1. ## Numbered\n2) ### Other\n", []string{"numbered", "other"}, nil)
	has("- ```\n  # In fence\n  ```\n- # After fence\n", []string{"after-fence"}, []string{"in-fence"})
	has("- item\n\n  ```\n  # Nested fence\n  ```\n\n  # Nested real\n", []string{"nested-real"}, []string{"nested-fence"})
	has("> ```\n> # Quoted fence\n> ```\n> # Quoted real\n", []string{"quoted-real"}, []string{"quoted-fence"})
	has("> - # Quoted item\n", []string{"quoted-item"}, nil)
	has("- > # Item quote\n", []string{"item-quote"}, nil)
	has(">     # Quoted code\n", nil, []string{"quoted-code"})
	// Raw HTML blocks.
	has("<div>\n# Not a heading\n</div>\n\n# Real\n\n# Not a heading\n", []string{"real", "not-a-heading"}, []string{"not-a-heading-1"})
	has("<div>\n\n# Inside\n\n</div>\n", []string{"inside"}, nil)
	has("<!--\n# Commented\n-->\n# After comment\n", []string{"after-comment"}, []string{"commented"})
	has("<pre>\n# Pre\n\n# Still pre\n</pre>\n# After pre\n", []string{"after-pre"}, []string{"pre", "still-pre"})
	has("<details>\n<summary>S</summary>\n<a id=\"in-html\"></a>\n</details>\n", []string{"in-html"}, nil)
	has("<custom-tag>\nNot setext\n===\n", nil, []string{"not-setext"})
	has("Text\n<span>\n# After span\n", []string{"after-span"}, []string{"text-span"})
	// Code spans over lines.
	has("Text `code\n<a id=ghost></a>\nend` here\n", nil, []string{"ghost"})
	has("Para `one\ntwo` <a id=\"live\"></a>\n", []string{"live"}, nil)
	has("Open `tick\n\n<a id=\"next\"></a> `\n", []string{"next"}, nil)
	has("Escaped \\`not code <a id=\"esc\"></a> `\n", []string{"esc"}, nil)
}

// Links in code (fenced, indented, code spans over one line or several), in raw HTML blocks and in
// list-item fences are not links; a link on a later line of a paragraph is reported on its line.
func TestLinksOutsideCodeOnly(t *testing.T) {
	r := newRepo(t)
	r.write("docs/a.md", "# Real\n\nUse `[x](#not-a-link)` here.\n\n    [x](#not-a-link)\n\n"+
		"A `multi\n[x](#not-a-link)\nline` span.\n\n<div>\n[x](#not-a-link)\n</div>\n\n"+
		"- ```md\n  [x](#not-a-link)\n  [y](missing-in-code.md)\n  ```\n\n`[z](missing-in-span.md)`\n\n"+
		"First line\n[bad](#gone) second line\n\n- item [ok](#real) [bad](#gone-too)\n")
	errs, err := LinkErrors(r.root, "docs/a.md")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "errors", errs, []string{"docs/a.md:23: invalid anchor #gone", "docs/a.md:25: invalid anchor #gone-too"})
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
	t.Run("fixture: a target that cannot be read fails instead of passing", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file")
		}
		r := newRepo(t)
		r.write("docs/source.md", "[a](target.md#missing)\n[b](dir.md#x)\n[c](gone.md#x)\n")
		r.write("docs/target.md", "# Target\n")
		if err := os.MkdirAll(filepath.Join(r.root, "docs", "dir.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(r.root, "docs", "target.md"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(r.root, "docs", "target.md"), 0o644) })
		errs, err := anchorErrors(r.root, []string{"docs/source.md"})
		if err != nil {
			t.Fatal(err)
		}
		expectEqual(t, "errors", errs, []string{
			"docs/source.md:1: unreadable anchor target target.md#missing",
			"docs/source.md:2: unreadable anchor target dir.md#x",
		})
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
