//go:build dev

package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// CRW-1433 (bundle CRW-1419, N3-4): the relay documents named their sessions by the relay role ids (the way the old
// prose introduced them: "the parent (relay role `parent`)"). The ids are names the code keeps, and the layers are
// now named by the English nouns of hierarchy.md: parent (the repository session, relay role id `supervisor`), child
// (the project session, relay role id `parent`) and an issue child (no layer, relay role id `child`). Text a person
// reads in docs/relay and its neighbours says the layer, so the phrase "relay role" and the bare word
// "supervisor" for a session leave the prose. This test catches the leftovers of that rewrite; it does not judge
// the bare words `parent` and `child`, whose sense no pattern can decide (the sentence ledger of the plan carries it).
//
// What is read. Each in-scope file is read with its code (fenced blocks and inline spans) blanked, as
// TestLayerNamesLinkTheTable does, and with the target of every link blanked (maskLinkTargets): the target
// `supervisor-channel.md` or `#through-the-supervisor-channel` is a file name or an anchor, not prose. The display
// text of a link is still read.
//
// What is a problem. (1) the phrase "relay role" or "relay roles", whatever the whitespace between the words;
// (2) the bare word "supervisor" or "supervisors" that is not joined by a hyphen or an underscore to another word
// and is not inside a permanent phrase of docsLayerPermanentPhrases; (3) the leftover phrases "relay parent",
// "relay child" and "are relay roles".
//
// Pending markers (the pattern TestRoleIdsStayOutOfProse and role_words_baseline use). A file that still holds the
// old wording is listed in a pending marker, one file per unit that rewrites it
// (testdata/docs_layer_pending/<unit>.txt, one repository-relative path per line, `#` starts a comment). The test is
// shrink only: a listed file must still hold a hit (a stale marker fails, so a marker cannot hide a clean file) and
// an unlisted file must hold none. The unit that rewrites a file first deletes its marker (red: the test names the
// file and the hits), then edits the text (green). The subtest name is the path with "/" as "__" whether the file is
// pending or enforced, so a -run never silently matches nothing.
//
// docs/flow/reporting.md is listed in testdata/docs_layer_pending/1429-deferred.txt and is not rewritten in this
// bundle: CRW-1419 defers script.md, journal.md and reporting.md until CRW-1429 lands (bundle note), and the
// reviewer-final sentences of reporting.md are the text of Z-1 and Z-2. The marker leaves with the unit that rewrites
// the file (1433-k, if the parent releases it).
//
// Exempt lines. A bare "supervisor" that names a process (the daemon pass, "a restarted supervisor", I-65) is no
// layer. A phrase cannot tell the two apart for the whole corpus, so the unit that owns the file lists the process
// uses in testdata/docs_layer_exempt/<unit>.tsv: `path<TAB>phrase<TAB>reason`, `#` starts a comment, one file per unit
// so that the units do not share one. Each occurrence of the phrase (any case, any whitespace) is blanked in that
// path. A line whose path is not in scope, whose reason is empty or whose phrase is not in the file is a problem, so
// an exemption cannot outlive the sentence it covers.

const (
	docsLayerPendingDir = "testdata/docs_layer_pending"
	docsLayerExemptDir  = "testdata/docs_layer_exempt"
)

// docsLayerScopeFiles are the in-scope documents besides docs/relay/*.md (read from the directory, so a new relay
// document is in scope the day it is added).
var docsLayerScopeFiles = []string{
	"docs/flow/reporting.md",
	"docs/flow/session-host.md",
	"docs/role-execution-policy.md",
	"docs/live-trial.md",
	"docs/runtime-install.md",
	"docs/CI.md",
	"docs/review/adjudication.md",
	"docs/review/crw-review.md",
	"README.md",
	"AGENTS.md",
}

// docsLayerPermanentPhrases are bare uses of "supervisor" that stay for good, each with its reason.
var docsLayerPermanentPhrases = map[string]string{
	"supervisor channel":    "the name of the feature and of the document (supervisor-channel.md, reporting.md#through-the-supervisor-channel)",
	"supervisor recipient":  "a heading and a term of the channel: the recipient of a supervisor-channel report",
	"initiative supervisor": "the old fourth layer, out of scope of CRW-1433 (the bundle excludes initiative-supervision documents)",
}

// docsLayerScope is the in-scope files of root, sorted, by repository-relative path.
func docsLayerScope(root string) ([]string, error) {
	files := append([]string{}, docsLayerScopeFiles...)
	entries, err := os.ReadDir(filepath.Join(root, "docs/relay"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, "docs/relay/"+e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// docsLayerSubtest is the subtest name of a file: the path with "/" as "__".
func docsLayerSubtest(rel string) string { return strings.ReplaceAll(rel, "/", "__") }

// blankText is s with every non-space character an underscore and the spaces and line breaks kept, so the blanked
// text has the length of s and every offset and line in the same place.
func blankText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteString(strings.Repeat("_", utf8.RuneLen(r)))
		}
	}
	return b.String()
}

var (
	// inlineLinkTarget is the parenthesis of `[text](target)`: from "](" to the closing parenthesis.
	inlineLinkTarget = regexp.MustCompile(`\]\(([^)]*)\)`)
	// referenceDefinition is `[ref]: target` at the start of a line.
	referenceDefinition = regexp.MustCompile(`(?m)^( {0,3}\[[^\]\n]+\]:)([ \t]*\S+)`)
	// autolinkTarget is `<scheme:target>`.
	autolinkTarget = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9+.-]*:[^>\s]*>`)
)

// maskLinkTargets is text with the target of every inline link, reference definition and autolink blanked, the
// length unchanged. maskCode does not hide link targets, and "supervisor-channel.md" in a target is a file name.
func maskLinkTargets(text string) string {
	text = inlineLinkTarget.ReplaceAllStringFunc(text, func(m string) string {
		return "](" + blankText(m[2:len(m)-1]) + ")"
	})
	text = referenceDefinition.ReplaceAllStringFunc(text, func(m string) string {
		sub := referenceDefinition.FindStringSubmatch(m)
		return sub[1] + blankText(sub[2])
	})
	return autolinkTarget.ReplaceAllStringFunc(text, blankText)
}

var (
	docsRelayRolePhrase = regexp.MustCompile(`(?i)relay\s+roles?\b`)
	docsRelayLeftover   = regexp.MustCompile(`(?i)relay\s+(?:parent|child)\b|\bare\s+relay\s+roles\b`)
	docsBareSupervisor  = regexp.MustCompile(`(?i)\bsupervisors?\b`)
	docsPermanentRegexp = func() *regexp.Regexp {
		var parts []string
		for phrase := range docsLayerPermanentPhrases {
			parts = append(parts, phraseRegexp(phrase))
		}
		sort.Strings(parts)
		return regexp.MustCompile(`(?i)` + strings.Join(parts, "|"))
	}()
)

// phraseRegexp is the regexp text that matches phrase with any run of whitespace where the phrase has a space.
func phraseRegexp(phrase string) string {
	words := strings.Fields(phrase)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(words, `\s+`)
}

// docsLayerExempt is one line of an exempt TSV.
type docsLayerExempt struct {
	file, path, phrase, reason string
	line                       int
}

// docsLayerExemptLines reads every .tsv in dir. An absent directory is no exemption. A line that has not three
// tab-separated fields, or has an empty one, is a problem.
func docsLayerExemptLines(dir string) ([]docsLayerExempt, []string) {
	var lines []docsLayerExempt
	var problems []string
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []string{err.Error()}
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".tsv") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for number, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Split(line, "\t")
			if len(fields) != 3 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
				problems = append(problems, fmt.Sprintf("%s:%d: want path<TAB>phrase<TAB>reason", f.Name(), number+1))
				continue
			}
			if strings.TrimSpace(fields[2]) == "" {
				problems = append(problems, fmt.Sprintf("%s:%d: the exemption of %q has no reason", f.Name(), number+1, strings.TrimSpace(fields[1])))
				continue
			}
			lines = append(lines, docsLayerExempt{
				file: f.Name(), path: strings.TrimSpace(fields[0]), phrase: strings.TrimSpace(fields[1]),
				reason: strings.TrimSpace(fields[2]), line: number + 1,
			})
		}
	}
	return lines, problems
}

// docsLayerMasked is the text a hit is looked for in: code blanked, link targets blanked, the literal non-layer
// phrases of nonLayerUses blanked, the permanent phrases blanked. The length never changes.
func docsLayerMasked(text string) string {
	masked := maskLinkTargets(maskCode(text))
	masked = nonLayerUse.ReplaceAllStringFunc(masked, blankText)
	return docsPermanentRegexp.ReplaceAllStringFunc(masked, blankText)
}

// docsLayerHits are the hits of one file as "rel:line: kind: excerpt", after the file's exempt lines, and the
// problems of those lines for this file (a stale phrase, a path that is not in scope is reported by the caller).
// text is the file as written.
func docsLayerHits(rel, text string, exempt []docsLayerExempt) (hits, problems []string) {
	masked := docsLayerMasked(text)
	for _, e := range exempt {
		if e.path != rel {
			continue
		}
		phrase := regexp.MustCompile(`(?i)` + phraseRegexp(e.phrase))
		if !phrase.MatchString(masked) {
			problems = append(problems, fmt.Sprintf("%s:%d: the exemption %q is not in %s: delete the line", e.file, e.line, e.phrase, rel))
			continue
		}
		masked = phrase.ReplaceAllStringFunc(masked, blankText)
	}
	type found struct {
		at  int
		msg string
	}
	var all []found
	add := func(kind string, loc []int) {
		line := 1 + strings.Count(text[:loc[0]], "\n")
		from := max(0, loc[0]-30)
		for from < loc[0] && !utf8.RuneStart(text[from]) {
			from++
		}
		to := min(len(text), loc[1]+30)
		for to < len(text) && !utf8.RuneStart(text[to]) {
			to++
		}
		all = append(all, found{loc[0], fmt.Sprintf("%s:%d: %s: ...%s...", rel, line, kind, normalized(text[from:to]))})
	}
	for _, loc := range docsRelayRolePhrase.FindAllStringIndex(masked, -1) {
		add("the phrase \"relay role\" names no layer", loc)
	}
	for _, loc := range docsRelayLeftover.FindAllStringIndex(masked, -1) {
		add("a relay-sense phrase is left", loc)
	}
	for _, loc := range docsBareSupervisor.FindAllStringIndex(masked, -1) {
		if loc[0] > 0 && masked[loc[0]-1] == '-' || loc[1] < len(masked) && masked[loc[1]] == '-' {
			continue // joined by a hyphen to another word: a name (relay-supervisor-channel/1, supervisor-obligation/1)
		}
		add("a bare \"supervisor\" for a session: name the layer", loc)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	for _, f := range all {
		hits = append(hits, f.msg)
	}
	return hits, problems
}

// docsLayerFileProblems is the verdict on one file: hits are the uses found in it and pending says a marker lists it.
// An enforced file fails on any hit; a pending one fails when it has none, since a marker for a clean file hides
// nothing but is stale.
func docsLayerFileProblems(rel string, hits []string, pending bool) []string {
	switch {
	case pending && len(hits) == 0:
		return []string{fmt.Sprintf("%s: listed in %s but holds no hit: delete the entry", rel, docsLayerPendingDir)}
	case !pending && len(hits) > 0:
		return append([]string{fmt.Sprintf("%s: %d hit(s) of the old wording and no pending marker lists the file", rel, len(hits))}, hits...)
	}
	return nil
}

// docsLayerPendingEntries reads every .txt marker in dir: path -> the marker file that lists it. A path listed
// twice is a problem. An absent directory is no marker.
func docsLayerPendingEntries(dir string) (map[string]string, []string) {
	entries := map[string]string{}
	var problems []string
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return entries, []string{err.Error()}
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".txt") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for number, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if prior, dup := entries[line]; dup {
				problems = append(problems, fmt.Sprintf("%s:%d: %q is already listed in %s", f.Name(), number+1, line, prior))
				continue
			}
			entries[line] = f.Name()
		}
	}
	return entries, problems
}

func TestRelayDocsProseNamesLayers(t *testing.T) {
	root := repoRoot()
	scope, err := docsLayerScope(root)
	if err != nil {
		t.Fatal(err)
	}
	inScope := map[string]bool{}
	for _, rel := range scope {
		inScope[rel] = true
	}
	pending, markerProblems := docsLayerPendingEntries(filepath.Join(root, "internal/dev/ci", docsLayerPendingDir))
	exempt, exemptProblems := docsLayerExemptLines(filepath.Join(root, "internal/dev/ci", docsLayerExemptDir))

	t.Run("markers", func(t *testing.T) {
		for _, p := range markerProblems {
			t.Error(p)
		}
		for _, p := range exemptProblems {
			t.Error(p)
		}
		for entry, file := range pending {
			if !inScope[entry] {
				t.Errorf("%s lists %q, which is no file in scope", file, entry)
			}
		}
		for _, e := range exempt {
			if !inScope[e.path] {
				t.Errorf("%s:%d: %q is no file in scope", e.file, e.line, e.path)
			}
		}
	})

	if len(scope) < 33 {
		t.Fatalf("%d files in scope, the plan names at least 33", len(scope))
	}
	for _, rel := range scope {
		t.Run(docsLayerSubtest(rel), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatal(err)
			}
			hits, problems := docsLayerHits(rel, string(data), exempt)
			for _, p := range problems {
				t.Error(p)
			}
			_, isPending := pending[rel]
			if verdict := docsLayerFileProblems(rel, hits, isPending); len(verdict) > 0 {
				t.Errorf("%s", strings.Join(verdict, "\n"))
			}
		})
	}

	t.Run("fixtures", func(t *testing.T) {
		const rel = "docs/relay/x.md"
		for name, c := range map[string]struct {
			text string
			want int // number of hits
		}{
			"a link target holds the file name":           {"See [the channel](supervisor-channel.md#x) for it.", 0},
			"a link target holds the anchor":              {"[reporting](../flow/reporting.md#through-the-supervisor-channel)", 0},
			"the supervisor in code passes":               {"Run `supervisor` now.\n\n```\nthe supervisor restarts\n```\n", 0},
			"the supervisor channel passes":               {"Report on the supervisor channel; a supervisor recipient reads it.", 0},
			"the channel phrase wraps":                    {"Report on the supervisor\nchannel.", 0},
			"the display text of a link is read":          {"[the supervisor](x.md) decides.", 1},
			"relay role in a link text fails":             {"[relay role](x.md) names it.", 1},
			"the relay role phrase fails":                 {"Read the relay role parent first.", 1},
			"the relay role phrase wraps":                 {"Read the relay\nrole parent first.", 1},
			"a bare supervisor fails":                     {"The supervisor restarts.", 1},
			"supervisors fail in any case":                {"Supervisors restart. A SUPERVISOR waits.", 2},
			"a hyphen joins a name":                       {"The relay-supervisor-channel/1 and supervisor-obligation/1 schemas.", 0},
			"an underscore joins a name":                  {"The supervisor_pid field and relay_supervisor.", 0},
			"a reference definition target is a name":     {"[a]\n\n[a]: supervisor-channel.md\n", 0},
			"an autolink target is a name":                {"See <https://example.org/supervisor/x>.", 0},
			"a relay parent fails":                        {"The relay parent accepts it.", 1},
			"a relay child and relay roles fail":          {"The relay child and the two are relay roles.", 3},
			"the initiative supervisor is out of scope":   {"The initiative supervisor decides.", 0},
			"a non-layer phrase of nonLayerUses passes":   {"Here the supervisor restarts the child.", 0},
			"the line of a hit is the line of the file":   {"one\ntwo\nthree supervisor\n", 1},
			"a code span cannot hide the next supervisor": {"`a` then a supervisor `b`.", 1},
		} {
			t.Run(name, func(t *testing.T) {
				hits, problems := docsLayerHits(rel, c.text, nil)
				if len(problems) > 0 {
					t.Fatalf("problems: %v", problems)
				}
				if len(hits) != c.want {
					t.Fatalf("hits = %v, want %d", hits, c.want)
				}
			})
		}

		t.Run("a hit names its line", func(t *testing.T) {
			hits, _ := docsLayerHits(rel, "one\ntwo\nthree supervisor\n", nil)
			if len(hits) != 1 || !strings.HasPrefix(hits[0], rel+":3:") {
				t.Fatalf("hits = %v, want line 3", hits)
			}
		})

		t.Run("an exempt line covers a process use only in its path", func(t *testing.T) {
			text := "The supervisor restarts. The supervisor restarts after a crash; the supervisor decides.\n"
			ex := []docsLayerExempt{{file: "u.tsv", path: rel, phrase: "the supervisor restarts", reason: "the daemon pass", line: 1}}
			hits, problems := docsLayerHits(rel, text, ex)
			if len(problems) > 0 || len(hits) != 1 || !strings.Contains(hits[0], "the supervisor decides") {
				t.Fatalf("hits = %v, problems = %v, want only the deciding supervisor", hits, problems)
			}
			if hits, _ := docsLayerHits("docs/relay/y.md", text, ex); len(hits) != 3 {
				t.Fatalf("another path is not exempt: %v", hits)
			}
		})

		t.Run("an exempt line whose phrase left the file is stale", func(t *testing.T) {
			ex := []docsLayerExempt{{file: "u.tsv", path: rel, phrase: "the supervisor restarts", reason: "the daemon pass", line: 4}}
			_, problems := docsLayerHits(rel, "The parent decides.\n", ex)
			if len(problems) != 1 || !strings.Contains(problems[0], "u.tsv:4:") || !strings.Contains(problems[0], "delete the line") {
				t.Fatalf("problems = %v, want the stale line", problems)
			}
		})

		t.Run("exempt files: comments, blanks, a missing reason and a short line", func(t *testing.T) {
			dir := t.TempDir()
			body := "# why\n\ndocs/a.md\tthe supervisor restarts\tthe daemon pass\n" +
				"docs/b.md\tphrase\t\n" + // no reason
				"docs/c.md\tonly two\n" // short
			if err := os.WriteFile(filepath.Join(dir, "u.tsv"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x\ty\tz\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			lines, problems := docsLayerExemptLines(dir)
			if len(lines) != 1 || lines[0].path != "docs/a.md" || lines[0].line != 3 {
				t.Errorf("lines = %+v", lines)
			}
			joined := strings.Join(problems, "\n")
			if len(problems) != 2 || !strings.Contains(joined, "u.tsv:4: the exemption of \"phrase\" has no reason") || !strings.Contains(joined, "u.tsv:5:") {
				t.Errorf("problems = %v", problems)
			}
			if lines, problems := docsLayerExemptLines(filepath.Join(dir, "absent")); len(lines) != 0 || len(problems) != 0 {
				t.Errorf("an absent directory is no exemption: %v %v", lines, problems)
			}
		})

		t.Run("a marker for a clean file fails", func(t *testing.T) {
			if p := docsLayerFileProblems(rel, nil, true); len(p) != 1 || !strings.Contains(p[0], "holds no hit") {
				t.Errorf("a stale marker: %v", p)
			}
		})
		t.Run("a file with hits and no marker fails", func(t *testing.T) {
			if p := docsLayerFileProblems(rel, []string{"x.md:3: hit"}, false); len(p) != 2 || !strings.Contains(p[0], "no pending marker") {
				t.Errorf("an unlisted file: %v", p)
			}
		})
		t.Run("a pending file with hits and a clean enforced one pass", func(t *testing.T) {
			if p := docsLayerFileProblems(rel, []string{"x.md:3: hit"}, true); len(p) != 0 {
				t.Errorf("a listed file with hits: %v", p)
			}
			if p := docsLayerFileProblems(rel, nil, false); len(p) != 0 {
				t.Errorf("a clean file: %v", p)
			}
		})
		t.Run("marker files: comments, blanks and duplicates", func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("a.txt", "# why\ndocs/x.md\n\ndocs/y.md\n")
			write("b.txt", "docs/y.md\n")
			write("c.md", "ignored\n")
			entries, problems := docsLayerPendingEntries(dir)
			if len(entries) != 2 || entries["docs/x.md"] != "a.txt" || entries["docs/y.md"] != "a.txt" {
				t.Errorf("entries: %v", entries)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], "b.txt:1:") {
				t.Errorf("problems: %v", problems)
			}
			if entries, problems := docsLayerPendingEntries(filepath.Join(dir, "absent")); len(entries) != 0 || len(problems) != 0 {
				t.Errorf("an absent directory is no marker: %v %v", entries, problems)
			}
		})
		t.Run("the scope holds the relay directory and the named documents", func(t *testing.T) {
			if !inScope["docs/relay/dag-scheduler.md"] || !inScope["README.md"] || !inScope["docs/flow/reporting.md"] {
				t.Errorf("scope: %v", scope)
			}
			if inScope["docs/flow/journal.md"] || inScope["docs/relay/../README.md"] {
				t.Errorf("scope holds a file the plan leaves out: %v", scope)
			}
		})
	})
}

// TestRelayDocsKeepLinkedHeadings: six headings of the documents and skills are the targets of links in other units'
// files (CRW-1433 decision 10). They stay byte-exact, so a rewrite of one unit cannot rename a heading whose inbound
// links another unit holds and leave that unit red. Each heading must exist in its file verbatim, and its slug must be
// the one the inbound links use.
func TestRelayDocsKeepLinkedHeadings(t *testing.T) {
	for _, c := range []struct{ file, heading, slug string }{
		{"docs/flow/reporting.md", "## Through the supervisor channel", "through-the-supervisor-channel"},
		{"docs/relay/operations.md", "### Child reports use the daemon", "child-reports-use-the-daemon"},
		{"docs/relay/operations.md", "## Reading a supervisor recipient's queue", "reading-a-supervisor-recipients-queue"},
		{"docs/relay/dag-scheduler.md", "## The goal-free parent's turn", "the-goal-free-parents-turn"},
		{"plugins/crw/skills/crw-run/references/merge-readiness.md", "### A base refresh the child made after the acceptance", "a-base-refresh-the-child-made-after-the-acceptance"},
		{"plugins/crw/skills/crw-run/SKILL.md", "### Hold the seat, bind the children, hand the seat over", "hold-the-seat-bind-the-children-hand-the-seat-over"},
	} {
		t.Run(docsLayerSubtest(c.file)+"/"+c.slug, func(t *testing.T) {
			text := readRepoText(t, c.file)
			found := false
			for _, line := range strings.Split(text, "\n") {
				if line == c.heading {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s has no heading line %q: it is the target of links in other files and stays as written", c.file, c.heading)
			}
			if got := anchorSlug(strings.TrimLeft(c.heading, "# ")); got != c.slug {
				t.Errorf("the slug of %q is %q, the links use %q", c.heading, got, c.slug)
			}
			if !markdownAnchors(text)[c.slug] {
				t.Errorf("%s has no anchor #%s", c.file, c.slug)
			}
		})
	}
}
