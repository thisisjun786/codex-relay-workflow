package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func memoryPtr[T any](v T) *T { return &v }

func memoryFloatEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.IsNaN(want) {
		holds(t, math.IsNaN(got), "got %v, want NaN", got)
	} else {
		holds(t, got == want || !math.IsNaN(got) && math.Abs(got-want) < 1e-9, "got %.17g, want %.17g", got, want)
	}
}

// ranking.test.ts:24-65; the search/rank integration at :67-99 is outside this port.
func TestMemoryKindsAndRecency(t *testing.T) {
	for path, kind := range map[string]MemoryKind{
		"memory_summary.md": MemorySummary, "MEMORY.md": MemoryHandbook,
		"raw_memories.md": MemoryRaw, "skills/deploy/SKILL.md": MemorySkill,
		"extensions/notes.md": MemoryExtension, "rollout_summaries/abc.md": MemoryRollout,
		"random.md": MemoryOther,
	} {
		holds(t, KindOfRelpath(path) == kind, "%s kind", path)
	}
	holds(t, KindOfRelpath("anything", "stage1") == MemoryStage1, "stage1 origin")
	holds(t, KindOfRelpath("MEMORY.md", "chat") == MemoryChat, "chat origin wins over path")
	const hour, now = 3_600_000.0, 1_783_382_400_000.0
	for _, kind := range []MemoryKind{MemorySummary, MemoryHandbook, MemorySkill} {
		memoryFloatEqual(t, RecencyBoost(kind, memoryPtr(now-1000*hour), now), 0)
	}
	memoryFloatEqual(t, RecencyBoost(MemoryRollout, memoryPtr(now), now), 1.5)
	memoryFloatEqual(t, RecencyBoost(MemoryRollout, memoryPtr(now-HalfLifeHours(MemoryRollout)*hour), now), 0.75)
	memoryFloatEqual(t, RecencyBoost(MemoryRollout, memoryPtr(now+500*hour), now), 1.5)
	memoryFloatEqual(t, RecencyBoost(MemoryStage1, nil, now), 0)
	stale := RecencyBoost(MemoryRollout, memoryPtr(now-5*HalfLifeHours(MemoryRollout)*hour), now)
	holds(t, stale < 0, "stale penalty: %v", stale)
	ancient := RecencyBoost(MemoryRollout, memoryPtr(now-100*HalfLifeHours(MemoryRollout)*hour), now)
	holds(t, ancient >= -2 && ancient < -1.9, "capped penalty: %v", ancient)
	for _, stamp := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		memoryFloatEqual(t, RecencyBoost(MemoryRaw, &stamp, now), 0)
	}
	memoryFloatEqual(t, RecencyBoost(MemoryRollout, memoryPtr(0.0), math.NaN()), math.NaN())
	memoryFloatEqual(t, RecencyBoost("invalid", nil, now), 0)
	memoryFloatEqual(t, RecencyBoost("invalid", memoryPtr(now), now), math.NaN())
	old := now - 365*24*hour
	summary := FinalScore(6, MemorySummary, &old, now)
	handbook := FinalScore(6, MemoryHandbook, &old, now)
	skill := FinalScore(6, MemorySkill, &old, now)
	rollout := FinalScore(6, MemoryRollout, &old, now)
	holds(t, summary > handbook && handbook > skill && skill > rollout, "kind ordering")
	memoryFloatEqual(t, summary-rollout, KindPriority(MemorySummary)-(KindPriority(MemoryRollout)-2))
	holds(t, DefaultMemoryLimit == 20 && perFileCap == 2 && relaxedPenalty == 2 && CwdBoost == 2, "constants")
}

func TestMemoryChunkScoresAndPresence(t *testing.T) {
	lsp := ExpandQueryWords([]string{"LSP"})
	memoryFloatEqual(t, ScoreChunk(Lower("NaiControlsPanel and NaiControlsPanel again"), lsp, "lsp"), 0)
	holds(t, ScoreChunk("the lsp server restarted", lsp, "lsp") > 0, "standalone boundary score")
	groups := ExpandQueryWords([]string{"결정", "세션"})
	memoryFloatEqual(t, ScoreChunk("# decision session", groups, "결정 세션"), 5)
	memoryFloatEqual(t, ScoreChunk("결정 세션", groups, "결정 세션"), 9)
	memoryFloatEqual(t, ScoreChunk("deploy deploy deploy", ExpandQueryWords([]string{"배포"}), "배포"), 4)
	memoryFloatEqual(t, ScoreChunk(strings.Repeat("deploy ", 20), ExpandQueryWords([]string{"배포"}), ""), 6)
	present := []bool{false, false}
	markGroupPresence("decision", groups, present)
	markGroupPresence("session", groups, present)
	holds(t, slices.Equal(present, []bool{true, true}), "presence accumulates: %v", present)
	holds(t, firstPresentMember("a session precedes decision", groups).Text == "decision", "group order anchors excerpt")
	holds(t, firstPresentMember("no terms", groups) == groups[0][0], "absent member falls back to lead")
	holds(t, firstMatchStartLine("precision\r\n\r\nwe ran CI\r\n", ExpandQueryWords([]string{"CI"})) == 3, "boundary-aware source line")
	holds(t, firstMatchStartLine("nothing", groups) == 1, "no-match line fallback")
}

func TestMemoryParagraphsAndExcerpts(t *testing.T) {
	lf := "a\nb\n\nc\n\n\nd\n"
	want := []ParagraphChunk{{"a\nb", 1}, {"c", 4}, {"d", 7}}
	for _, doc := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		holds(t, reflect.DeepEqual(ParagraphChunks(doc), want), "chunks: %+v", ParagraphChunks(doc))
	}
	holds(t, reflect.DeepEqual(ParagraphChunks("intro\r\n\r\nsecond para\r\n\r\nthird\r\n"), []ParagraphChunk{{"intro", 1}, {"second para", 3}, {"third", 5}}), "CRLF source lines")
	holds(t, len(ParagraphChunks("\ufeff \r\n\t\n")) == 0, "JS spaces are blank")
	holds(t, len(ParagraphChunks("\u0085\n")) == 1, "NEL is content")
	holds(t, ParagraphChunks("a\rb")[0].Text == "a\rb", "lone CR stays content")
	holds(t, excerptAround("precision then CI here", bounded("ci"), 6) == "en CI ", "excerpt anchors boundary match")
	holds(t, excerptAround("가나다 CI 라마", bounded("ci"), 6) == "나다 CI ", "Korean UTF-16 slicing")
	holds(t, excerptAround("İabcdefgh CI xyz", bounded("ci"), 4) == "h CI", "the excerpt is cut at the original offset (CRW-1128, known-defects.md :593)")
	holds(t, excerptAround("ab😀cd", loose("absent"), 3) == "ab\ufffd", "split surrogate platform boundary")
	holds(t, excerptAround("abcdef", loose("absent"), -2) == "abcd", "negative slice end")
}

func TestMemoryMarkdownTreeAndFrontmatter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	root := filepath.Join(home, "memories")
	files := []string{"MEMORY.md", "rollout_summaries/a.md", "skills/z/SKILL.md", ".hidden.md", ".hidden/a.md", "ignored.MD", "other.txt"}
	for _, name := range files {
		writeRolloutTestFile(t, root, name, "thread_id: one\r\ncwd: /proj/alpha\r\n\r\n# 배포\r\n")
	}
	if err := os.Symlink(filepath.Join(root, "MEMORY.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "skills"), filepath.Join(root, "link-dir")); err != nil {
		t.Fatal(err)
	}
	got, err := listMarkdownFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "MEMORY.md"), filepath.Join(root, "rollout_summaries/a.md"), filepath.Join(root, "skills/z/SKILL.md")}
	wantStrings(t, "tree", got, want...)
	for _, p := range got {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		holds(t, *frontmatterThreadID(string(b)) == "one" && *frontmatterCwd(string(b)) == "/proj/alpha", "frontmatter of %s", p)
	}
	empty, err := listMarkdownFiles(filepath.Join(root, "missing"))
	holds(t, err == nil && empty != nil && len(empty) == 0, "missing root returns []")
	_, err = listMarkdownFiles(filepath.Join(root, "MEMORY.md"))
	holds(t, err != nil, "existing file root propagates readdir error")
	link := filepath.Join(home, "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	got, err = listMarkdownFiles(link)
	holds(t, err == nil && len(got) == 3 && strings.HasPrefix(got[0], link), "root symlink followed")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	got, err = listMarkdownFiles(relative)
	holds(t, err == nil && len(got) == 3 && !filepath.IsAbs(got[0]), "relative root stays relative")
	holds(t, frontmatterCwd("# header\ncwd: /other") == nil, "body cwd ignored")
	holds(t, frontmatterCwd("thread_id: one\n\ncwd: /other") == nil, "blank ends leading block")
	holds(t, frontmatterCwd("Cwd: /bad\ncwd: /later") == nil, "uppercase key ends block")
	holds(t, *frontmatterCwd("cwd: /a b\n") == "/a b", "a cwd with a space is read whole (CRW-1128, known-defects.md :592)")
	holds(t, frontmatterThreadID("intro\nthread_id:\n# next") == nil, "the id is on the line of its key (CRW-1128, known-defects.md :591)")
	for _, sep := range []string{"\r", "\n", "\u2028", "\u2029"} {
		// Only the leading key lines name a memory (CRW-1128, known-defects.md :591).
		holds(t, frontmatterThreadID("intro"+sep+"thread_id: one") == nil, "a key after a body line %q", sep)
	}
	holds(t, frontmatterThreadID(strings.Repeat("😀", 1000)+"\nthread_id: late") == nil, "UTF-16 prefix limit")
}

func TestMemoryJSONContracts(t *testing.T) {
	hit := MemoryHit{Origin: "file", Kind: MemoryHandbook, Relpath: "MEMORY.md", Excerpt: "x", Score: 3}
	holds(t, reflect.DeepEqual(canon(t, hit), map[string]any{
		"origin": "file", "kind": "handbook", "relpath": "MEMORY.md", "threadId": nil,
		"updatedAt": nil, "excerpt": "x", "startLine": nil, "cwd": nil, "score": float64(3),
	}), "nullable hit JSON")
	result := MemorySearchResult{Hits: []MemoryHit{hit}, Warnings: []string{}, ScannedFiles: 1, ElapsedMs: 0.5}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var back MemorySearchResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	holds(t, reflect.DeepEqual(result, back), "result JSON roundtrip")
}

type memoryOracleCase struct {
	oracleCase
	Number         string
	Error          string
	Classification string
	Reason         string
	GoExpected     json.RawMessage
}

// Recorded from the real exported/private CXC helpers, not from this Go implementation.
func TestMemoryOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/memoryrank/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []memoryOracleCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	for _, p := range []string{"mem/MEMORY.md", "mem/nested/a.md", "mem/.hidden.md", "mem/.hidden/a.md", "mem/skip.MD", "mem/other.txt"} {
		writeRolloutTestFile(t, root, p, "x")
	}
	for link, target := range map[string]string{"mem/link.md": "mem/MEMORY.md", "mem/link-dir": "mem/nested", "root-link": "mem"} {
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	seen, changed := map[string]int{}, 0
	fixes := portFixed(t, "memoryrank")
	for i, c := range cases {
		seen[c.Fn]++
		str := func(n int) string { return arg[string](t, c.oracleCase, n) }
		groups := func(n int) []QueryGroup { return arg[[]QueryGroup](t, c.oracleCase, n) }
		var got any
		switch c.Fn {
		case "constants":
			got = []int{DefaultMemoryLimit, perFileCap, relaxedPenalty, CwdBoost}
		case "kind":
			got = KindOfRelpath(str(0), str(1))
		case "priority":
			got = KindPriority(MemoryKind(str(0)))
		case "halfLife":
			got = HalfLifeHours(MemoryKind(str(0)))
		case "recency":
			got = RecencyBoost(MemoryKind(str(0)), arg[*float64](t, c.oracleCase, 1), arg[float64](t, c.oracleCase, 2))
		case "final":
			got = FinalScore(arg[float64](t, c.oracleCase, 0), MemoryKind(str(1)), arg[*float64](t, c.oracleCase, 2), arg[float64](t, c.oracleCase, 3))
		case "score":
			got = ScoreChunk(str(0), groups(1), str(2))
		case "presence":
			p := arg[[]bool](t, c.oracleCase, 2)
			markGroupPresence(str(0), groups(1), p)
			got = p
		case "line":
			got = firstMatchStartLine(str(0), groups(1))
		case "member":
			got = firstPresentMember(str(0), groups(1))
		case "group":
			got = groupHit(str(0), arg[QueryGroup](t, c.oracleCase, 1))
		case "threadId":
			got = frontmatterThreadID(str(0))
		case "cwd":
			got = frontmatterCwd(str(0))
		case "chunks":
			got = ParagraphChunks(str(0))
		case "excerpt":
			got = excerptAround(str(0), arg[QueryTerm](t, c.oracleCase, 1), arg[int](t, c.oracleCase, 2))
		case "list":
			files, err := listMarkdownFiles(strings.ReplaceAll(str(0), "$R", root))
			if c.Error != "" {
				holds(t, err != nil, "case %d: expected listing error", i)
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for j, p := range files {
				files[j] = strings.ReplaceAll(filepath.ToSlash(p), filepath.ToSlash(root), "$R")
			}
			got = files
		default:
			t.Fatalf("unknown oracle function %q", c.Fn)
		}
		if f, ok := got.(float64); ok {
			want := 0.0
			switch c.Number {
			case "nan":
				want = math.NaN()
			case "infinity":
				want = math.Inf(1)
			default:
				if err := json.Unmarshal(c.Out, &want); err != nil {
					t.Fatal(err)
				}
			}
			memoryFloatEqual(t, f, want)
			continue
		}
		expected := c.Out
		if fix, ok := fixes.lookup(strconv.Itoa(i)); ok {
			expected = fix // port: fixed (docs/port-cxc/known-defects/CRW-1128.md)
		}
		if c.Classification != "" {
			holds(t, c.Classification == "intentionally-changed" && c.Reason != "" && len(c.GoExpected) > 0, "classified boundary needs evidence")
			changed++
			expected = c.GoExpected
		}
		var want any
		if err := json.Unmarshal(expected, &want); err != nil {
			t.Fatal(err)
		}
		if g := canon(t, got); !reflect.DeepEqual(g, want) {
			portFixedDump("memoryrank", strconv.Itoa(i), g)
			t.Errorf("case %d %s%s: got %v, oracle %v", i, c.Fn, c.In, g, want)
		}
	}
	holds(t, len(seen) == 16 && len(cases) >= 700 && changed > 0, "oracle grid coverage: %d functions, %d cases, %d classified boundaries", len(seen), len(cases), changed)
	t.Logf("replayed %d cases of %d functions; %d explicitly classified UTF-16 boundaries", len(cases), len(seen), changed)
}
