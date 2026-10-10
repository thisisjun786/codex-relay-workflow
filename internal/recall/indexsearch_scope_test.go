package recall

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// CRW-1087 (A8-03): the lanes rank the eligible rows, so rows outside the scope cannot fill the pool.

const scopeNoise = 600 // More than the 500-row pool of a limit of 50.

type scopeCorpus struct {
	db        *RwDb
	files     int
	file, msg *Stmt
}

func newScopeCorpus(t *testing.T) *scopeCorpus {
	t.Helper()
	db, _ := indexTestDB(t)
	recallSQL(t, db, "BEGIN") // One commit for the corpus; scopeSearch ends it.
	return &scopeCorpus{db: db, file: recallStmt(t, db, "INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,?,?,?,?,?,?,?)"), msg: recallStmt(t, db, "INSERT INTO msgs(path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?,?,?,?,?)")}
}

type scopeRow struct {
	role, field, ts, text     string
	synthetic                 int
	cwd, source, repoKey, tid string
}

// add inserts one file holding one message. Texts of one corpus have the same number of tokens, so
// the order of their relevance does not depend on the corpus average length.
func (c *scopeCorpus) add(t *testing.T, r scopeRow) {
	t.Helper()
	c.files++
	path := fmt.Sprintf("/rollouts/%d.jsonl", c.files)
	if r.role == "" {
		r.role = "user"
	}
	if r.field == "" {
		r.field = "content"
	}
	if r.cwd == "" {
		r.cwd = "/proj/here"
	}
	if r.source == "" {
		r.source = "main"
	}
	if r.ts == "" {
		r.ts = "2026-03-01T00:00:00.000Z"
	}
	var repoKey any
	if r.repoKey != "" {
		repoKey = r.repoKey
	}
	if r.tid == "" {
		r.tid = fmt.Sprintf("thread-%d", c.files)
	}
	if _, err := c.file.Run(path, 1, 1, r.tid, r.cwd, r.source, "2026-03-01", repoKey); err != nil {
		t.Fatal(err)
	}
	if _, err := c.msg.Run(path, 0, r.ts, r.role, r.field, r.synthetic, r.text); err != nil {
		t.Fatal(err)
	}
}

func scopeText(tf int, tag string) string {
	words := make([]string, 0, 9)
	for i := 0; i < tf; i++ {
		words = append(words, "quokka")
	}
	for i := len(words); i < 9; i++ {
		words = append(words, fmt.Sprintf("%s%d", tag, i))
	}
	return strings.Join(words, " ")
}

func scopeSearch(t *testing.T, c *scopeCorpus, opts IndexQueryOptions) []string {
	t.Helper()
	recallSQL(t, c.db, "COMMIT")
	now := float64(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).UnixMilli())
	opts.Plan, opts.Limit, opts.NowMs, opts.Home = ChatMatchPlan("quokka", false, false), 50, &now, t.TempDir()
	if opts.Source == "" {
		opts.Source = RolloutMain
	}
	if !opts.IncludeTools {
		opts.IncludeTools = false
	}
	r, err := queryIndex(c.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		out[i] = h.Text
	}
	return out
}

func TestIndexRankScopeExcludesRowsBeforeThePoolIsCut(t *testing.T) {
	// Each case: the options that scope the search, the eligible rows (an old strong match and a recent weak one),
	// and one noise row outside the scope that matches better than either.
	old, recent := "2026-01-01T00:00:00.000Z", "2026-03-01T00:00:00.000Z"
	cases := []struct {
		name   string
		opts   IndexQueryOptions
		strong scopeRow // eligible, old, relevant
		weak   scopeRow // eligible, recent, less relevant
		noise  scopeRow // outside the scope
	}{
		{"source", IndexQueryOptions{Source: RolloutMain}, scopeRow{ts: old}, scopeRow{ts: recent}, scopeRow{source: "subagent", ts: recent}},
		{"role", IndexQueryOptions{Role: "assistant"}, scopeRow{ts: old, role: "assistant"}, scopeRow{ts: recent, role: "assistant"}, scopeRow{ts: recent, role: "user"}},
		{"cwd sibling", IndexQueryOptions{Cwd: "/proj/here"}, scopeRow{ts: old, cwd: "/proj/here/sub"}, scopeRow{ts: recent, cwd: "/proj/here"}, scopeRow{ts: recent, cwd: "/proj/there"}},
		{"same origin checkout", IndexQueryOptions{Cwd: "/worktrees/task", RepoKey: "example.test/team/repo"}, scopeRow{ts: old, cwd: "/proj/main", repoKey: "example.test/team/repo"}, scopeRow{ts: recent, cwd: "/proj/main2", repoKey: "example.test/team/repo"}, scopeRow{ts: recent, cwd: "/proj/other", repoKey: "example.test/team/other"}},
		{"old date", IndexQueryOptions{CutoffISO: "2026-02-01T00:00:00.000Z"}, scopeRow{ts: "2026-02-02T00:00:00.000Z"}, scopeRow{ts: recent}, scopeRow{ts: old}},
		{"synthetic", IndexQueryOptions{}, scopeRow{ts: old}, scopeRow{ts: recent}, scopeRow{ts: recent, synthetic: 1}},
		{"tool log", IndexQueryOptions{}, scopeRow{ts: old}, scopeRow{ts: recent}, scopeRow{ts: recent, field: "tool_log"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			build := func(withNoise bool) []string {
				corpus := newScopeCorpus(t)
				strong, weak := c.strong, c.weak
				strong.text, weak.text = scopeText(3, "pad"), scopeText(1, "pad")
				corpus.add(t, strong)
				corpus.add(t, weak)
				if withNoise {
					for i := range scopeNoise {
						noise := c.noise
						noise.text = scopeText(5, fmt.Sprintf("n%d_", i))
						corpus.add(t, noise)
					}
				}
				return scopeSearch(t, corpus, c.opts)
			}
			quiet, noisy := build(false), build(true)
			if len(quiet) != 2 || quiet[0] != scopeText(3, "pad") {
				t.Fatalf("the fixture is not the eligible corpus: %q", quiet)
			}
			if !reflect.DeepEqual(quiet, noisy) {
				t.Fatalf("rows outside the scope changed the page\nwithout: %q\nwith:    %q", quiet, noisy)
			}
		})
	}
}

// TestIndexRankLaneCost compares the lane query before this change (global pool, no join) with the
// one that carries the eligibility predicate, on a larger index. It records timings and the plan;
// it is skipped unless CRW_RECALL_PERF is set, so the suite stays quick.
func TestIndexRankLaneCost(t *testing.T) {
	if os.Getenv("CRW_RECALL_PERF") == "" {
		t.Skip("set CRW_RECALL_PERF=1 to time the lane queries")
	}
	const rows = 60000
	for _, every := range []int{10, 1} { // one row in ten is eligible; every row is eligible
		t.Run(fmt.Sprintf("eligible 1 in %d", every), func(t *testing.T) {
			c := newScopeCorpus(t)
			for i := range rows {
				r := scopeRow{ts: fmt.Sprintf("2026-02-%02dT00:00:00.000Z", 1+i%27), text: scopeText(1+i%4, fmt.Sprintf("w%d_", i%50))}
				if i%every != 0 {
					r.source = "subagent"
				}
				c.add(t, r)
			}
			recallSQL(t, c.db, "COMMIT")
			now := float64(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).UnixMilli())
			opts := resolvedQuery{IndexQueryOptions: IndexQueryOptions{Plan: ChatMatchPlan("quokka", false, false), Limit: 50, Source: RolloutMain, NowMs: &now}, hasRepoKeyColumn: true}
			where, params := candidateFilter(opts, false)
			old := func() { indexRankOld(t, c.db, "msgs_fts") }
			cur := func() { indexRankLaneRanks(c.db, "msgs_fts", []string{"quokka"}, false, 500, where, params) }
			oldTri := func() { indexRankOld(t, c.db, "msgs_tri") }
			curTri := func() { indexRankLaneRanks(c.db, "msgs_tri", []string{"quokka"}, false, 500, where, params) }
			median := func(f func()) time.Duration {
				runs := make([]time.Duration, 7)
				for i := range runs {
					start := time.Now()
					f()
					runs[i] = time.Since(start)
				}
				sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })
				return runs[len(runs)/2]
			}
			t.Logf("rows=%d eligible=%d", rows, rows/every)
			t.Logf("unicode61 lane: before %v, after %v", median(old), median(cur))
			t.Logf("trigram lane:   before %v, after %v", median(oldTri), median(curTri))
			plan := indexRows(t, c.db, "EXPLAIN QUERY PLAN SELECT msgs_fts.rowid AS id FROM msgs_fts JOIN msgs m ON m.id = msgs_fts.rowid JOIN files f ON f.path = m.path WHERE msgs_fts MATCH '\"quokka\"' AND f.source = 'main' ORDER BY bm25(msgs_fts) LIMIT 500")
			for _, row := range plan {
				t.Logf("plan: %v", row["detail"])
			}
		})
	}
}

func indexRankOld(t *testing.T, db *RwDb, table string) {
	t.Helper()
	if _, err := indexRankRead(db, "SELECT rowid AS id FROM "+table+" WHERE "+table+" MATCH ? ORDER BY bm25("+table+") LIMIT ?", `"quokka"`, 500); err != nil {
		t.Fatal(err)
	}
}
