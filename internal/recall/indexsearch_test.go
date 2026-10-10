package recall

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type indexRankOracleCase struct {
	Name, Query, Error string
	Options            IndexQueryOptions
	SQL                []string
	Special            map[string]string
	Closed             bool
	Out                any
}
type indexRankOracleCorpus struct {
	Name  string
	Files map[string]string
	Cases []indexRankOracleCase
}
type indexRankOracleData struct {
	Now float64
	RRF []struct {
		Rank        *float64
		Weight, Out float64
	}
	Recency []struct {
		TS       *float64
		Now, Out float64
	}
	Corpora []indexRankOracleCorpus
}

func indexRankOracle(t *testing.T) indexRankOracleData {
	t.Helper()
	b, err := os.ReadFile("testdata/indexsearch/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var data indexRankOracleData
	if err = json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
func indexRankCorpus(t *testing.T, name string) (string, *RwDb, indexRankOracleCorpus, float64) {
	t.Helper()
	data := indexRankOracle(t)
	for _, c := range data.Corpora {
		if c.Name != name {
			continue
		}
		home := t.TempDir()
		if name == "fixture" {
			home = buildRecallCodexHome(t, time.UnixMilli(int64(data.Now)).UTC())
		}
		t.Setenv("CODEX_HOME", home)
		t.Setenv("CRW_HOME", home)
		for path, body := range c.Files {
			writeRolloutTestFile(t, home, path, body)
		}
		db, err := openIndex(filepath.Join(home, "sidecar", "index.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err = ingest(home, db, 0); err != nil {
			t.Fatal(err)
		}
		return home, db, c, data.Now
	}
	t.Fatal("missing corpus", name)
	return "", nil, indexRankOracleCorpus{}, 0
}
func indexRankOptions(home, query string, now float64) IndexQueryOptions {
	return IndexQueryOptions{Home: home, Plan: ChatMatchPlan(query, false, false), Limit: 50, Source: RolloutMain, IncludeTools: true, NowMs: &now}
}
func indexRankSearch(t *testing.T, db *RwDb, opts IndexQueryOptions) ChatSearchResult {
	t.Helper()
	r, err := queryIndex(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func indexRankKeys(hits []ChatHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.TS + "|" + string(h.Source) + "|" + h.Text
	}
	sort.Strings(out)
	return out
}

// The eight B scenarios of index-rank.test.ts:101-189, with its ingested corpus.
func TestIndexRankRRF(t *testing.T) {
	for _, c := range indexRankOracle(t).RRF {
		if got := RRFScore(c.Rank, c.Weight); got != c.Out {
			t.Fatalf("RRF %v %g: %g != %g", c.Rank, c.Weight, got, c.Out)
		}
	}
	if RRFK != 60 || LaneWeightFTS != 1 || LaneWeightTri != .8 {
		t.Fatal("lane constants changed")
	}
}
func TestIndexRankRecency(t *testing.T) {
	for _, c := range indexRankOracle(t).Recency {
		if got := RecencyScore(c.TS, c.Now); math.Abs(got-c.Out) > 1e-15 {
			t.Fatalf("recency %v: %g != %g", c.TS, got, c.Out)
		}
	}
	for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if RecencyScore(&n, 0) != 0 {
			t.Fatal("nonfinite timestamp contributes")
		}
	}
	if math.Abs(RecencyWeight-(1.0/61-1.0/62)) > 1e-15 {
		t.Fatal("recency exceeds one adjacent head gap")
	}
}
func TestIndexRankDenseOldDefault(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	opts := indexRankOptions(home, "quokka", now)
	r := indexRankSearch(t, db, opts)
	if r.Mode != "index" || len(r.Hits) != 3 || !strings.Contains(r.Hits[0].Text, "quokka deployment plan") {
		t.Fatal(r)
	}
	for i, h := range r.Hits {
		if h.Score == nil || (i > 0 && *r.Hits[i-1].Score < *h.Score) {
			t.Fatal("not score descending")
		}
	}
	opts.Order = ChatRecent
	r = indexRankSearch(t, db, opts)
	if !strings.Contains(r.Hits[0].Text, "quokka reference at the end") {
		t.Fatal(r.Hits)
	}
	for i, h := range r.Hits {
		if h.Score != nil || (i > 0 && r.Hits[i-1].TS < h.TS) {
			t.Fatal("recent mode changed")
		}
	}
}
func TestIndexRankSameSet(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	for _, q := range []string{"quokka", "wombat", "트라이그램"} {
		opts := indexRankOptions(home, q, now)
		a := indexRankSearch(t, db, opts)
		opts.Order = ChatRecent
		b := indexRankSearch(t, db, opts)
		if !reflect.DeepEqual(indexRankKeys(a.Hits), indexRankKeys(b.Hits)) {
			t.Fatal(q)
		}
	}
}
func TestIndexRankRecencyTie(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	r := indexRankSearch(t, db, indexRankOptions(home, "wombat", now))
	if len(r.Hits) != 2 || r.Hits[0].TS <= r.Hits[1].TS || *r.Hits[0].Score <= *r.Hits[1].Score {
		t.Fatal(r.Hits)
	}
}
func TestIndexRankKorean(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	r := indexRankSearch(t, db, indexRankOptions(home, "트라이그램", now))
	if len(r.Hits) != 2 || !strings.Contains(r.Hits[0].Text, "트라이그램 색인") {
		t.Fatal(r.Hits)
	}
	opts := indexRankOptions(home, "한글", now)
	r = indexRankSearch(t, db, opts)
	scan, err := searchViaScan("한글", ChatSearchOptions{Home: &home}, chatScanShared{Home: home, Days: 0, Limit: 50, Plan: opts.Plan, Source: RolloutMain}, time.UnixMilli(int64(now)))
	if err != nil || len(r.Hits) == 0 || len(r.Hits) != len(scan.Hits) {
		t.Fatal(r, scan, err)
	}
}
func TestIndexRankDeterministic(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	opts := indexRankOptions(home, "quokka", now)
	a := indexRankSearch(t, db, opts)
	b := indexRankSearch(t, db, opts)
	if !reflect.DeepEqual(a.Hits, b.Hits) {
		t.Fatal("repeated query changed")
	}
}
func TestIndexRankLimit(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "rank")
	opts := indexRankOptions(home, "quokka", now)
	opts.Limit = 1
	r := indexRankSearch(t, db, opts)
	if len(r.Hits) != 1 || !strings.Contains(r.Hits[0].Text, "quokka deployment plan") || !strings.Contains(strings.Join(r.Warnings, "\n"), "truncated at limit 1") {
		t.Fatal(r)
	}
}

func indexRankNormalized(t *testing.T, r ChatSearchResult, home string) map[string]any {
	t.Helper()
	r.ElapsedMs = 0
	nonfinite := map[int]bool{}
	for i, h := range r.Hits {
		if h.Score != nil && (math.IsNaN(*h.Score) || math.IsInf(*h.Score, 0)) {
			nonfinite[i] = true
			r.Hits[i].Score = nil
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal([]byte(strings.ReplaceAll(string(b), home, "$R")), &out); err != nil {
		t.Fatal(err)
	}
	for i := range nonfinite {
		out["hits"].([]any)[i].(map[string]any)["score"] = nil
	}
	return out
}

// indexRankCompare compares a normalized result with the recorded one; key names the case for a regenerated port-fixed file.
func indexRankCompare(t *testing.T, key string, got, want any) {
	t.Helper()
	g, w := got.(map[string]any), want.(map[string]any)
	gh, wh := g["hits"].([]any), w["hits"].([]any)
	if len(gh) != len(wh) {
		portFixedDump("indexsearch", key, map[string]any{"out": got})
		t.Fatalf("hit count %d != %d", len(gh), len(wh))
	}
	for i := range gh {
		a, b := gh[i].(map[string]any), wh[i].(map[string]any)
		if expected, ok := b["score"].(float64); ok {
			actual, ok := a["score"].(float64)
			if !ok || math.Abs(actual-expected) > 1e-15 {
				t.Fatalf("hit %d score %v != %g", i, a["score"], expected)
			}
			a["score"] = expected
		}
	}
	if !reflect.DeepEqual(g, w) {
		portFixedDump("indexsearch", key, map[string]any{"out": g})
		t.Fatalf("Node oracle differs\ngot: %v\nwant: %v", g, w)
	}
}

// indexRankLaneScoreFixes lists, by case, the hit scores that the eligibility-first lanes change: the
// oracle ranked a row behind rows the search excludes (another source, an older date), the port does not.
var indexRankLaneScoreFixes = map[string]map[int]float64{
	"fixture/trigram/5":  {0: 0.029513232968891315, 1: 0.029040652044353788, 2: 0.028727260331116933, 3: 0.028587823063299003},
	"fixture/zebra/13":   {0: 0.02977260708619778},
	"fixture/trigram/21": {1: 0.029040652044353788, 2: 0.028727260331116933, 3: 0.028587823063299003},
}

func TestIndexRankRecordedOracle(t *testing.T) {
	// The recorded dates were made under UTC; pin the local zone so a non-UTC host (TZ=Asia/Seoul) reads the
	// zone-less timestamps the same way.
	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })
	data := indexRankOracle(t)
	count := 0
	fixes := portFixed(t, "indexsearch")
	for _, corpus := range data.Corpora {
		for i, c := range corpus.Cases {
			count++
			t.Run(corpus.Name+"/"+c.Name+"/"+memoryNumberText(float64(i)), func(t *testing.T) {
				key := corpus.Name + "/" + c.Name + "/" + memoryNumberText(float64(i))
				if fix, ok := fixes.lookup(key); ok {
					// port: fixed (docs/port-cxc/known-defects/CRW-1128.md): the port's answer in place of the recorded one.
					var fixed struct {
						Error string
						Out   any
					}
					if err := json.Unmarshal(fix, &fixed); err != nil {
						t.Fatal(err)
					}
					c.Error, c.Out = fixed.Error, fixed.Out
				}
				home, db, _, _ := indexRankCorpus(t, corpus.Name)
				opts := c.Options
				opts.Home = home
				for key, value := range c.Special {
					n := hitCountNumber(value)
					switch key {
					case "limit":
						opts.Limit = n
					case "nowMs":
						opts.NowMs = &n
					default:
						t.Fatal(key)
					}
				}
				for _, sql := range c.SQL {
					if err := db.Exec(sql); err != nil {
						t.Fatal(err)
					}
				}
				if c.Closed {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				r, err := queryIndex(db, opts)
				if c.Error != "" {
					if err == nil {
						portFixedDump("indexsearch", key, map[string]any{"out": indexRankNormalized(t, r, home)})
						t.Fatalf("error %v != %q", err, c.Error)
					}
					if err.Error() != c.Error {
						portFixedDump("indexsearch", key, map[string]any{"error": err.Error()})
						t.Fatalf("error %v != %q", err, c.Error)
					}
					return
				}
				if err != nil {
					portFixedDump("indexsearch", key, map[string]any{"error": err.Error()})
					t.Fatal(err)
				}
				// port: fixed (docs/port-cxc/known-defects/CRW-1087.md): lane ranks are taken among the eligible rows.
				for hit, score := range indexRankLaneScoreFixes[key] {
					c.Out.(map[string]any)["hits"].([]any)[hit].(map[string]any)["score"] = score
				}
				normalized := indexRankNormalized(t, r, home)
				indexRankCompare(t, key, normalized, c.Out)
			})
		}
	}
	if count != 66 {
		t.Fatalf("oracle case set changed: %d", count)
	}
}

// index.test.ts:51-138: exact recent shapes, relevance sets, relaxed plans/context.
func TestIndexRankIndexEqualsScan(t *testing.T) {
	home, db, corpus, now := indexRankCorpus(t, "fixture")
	for i, c := range corpus.Cases {
		t.Run(memoryNumberText(float64(i))+"/"+c.Query, func(t *testing.T) {
			opts := c.Options
			opts.Home = home
			r := indexRankSearch(t, db, opts)
			scanOpts := ChatSearchOptions{Home: &home, IncludeSynthetic: opts.IncludeSynthetic, IncludeTools: &opts.IncludeTools, Role: &opts.Role, Cwd: &opts.Cwd}
			scan, err := searchViaScan(c.Query, scanOpts, chatScanShared{Home: home, Days: 0, Limit: opts.Limit, ContextN: opts.ContextN, Plan: opts.Plan, Source: opts.Source, RepoKey: opts.RepoKey}, time.UnixMilli(int64(now)))
			if err != nil {
				t.Fatal(err)
			}
			if opts.Order == ChatRecent {
				for j := range r.Hits {
					r.Hits[j].Score = nil
				}
				if !reflect.DeepEqual(r.Hits, scan.Hits) {
					t.Fatalf("recent/scan hits differ\n%v\n%v", r.Hits, scan.Hits)
				}
			} else if !reflect.DeepEqual(indexRankKeys(r.Hits), indexRankKeys(scan.Hits)) {
				t.Fatal("rank/scan set differs")
			}
		})
	}
}

func TestIndexRankSameOriginAndLegacy(t *testing.T) {
	home, db, _, now := indexRankCorpus(t, "fixture")
	for _, legacy := range []bool{false, true} {
		if legacy {
			if err := db.Exec("DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key"); err != nil {
				t.Fatal(err)
			}
		}
		opts := indexRankOptions(home, "trigram", now)
		opts.Source = RolloutAll
		opts.Cwd = "/different/checkout"
		opts.RepoKey = "github.com/example/alpha"
		r := indexRankSearch(t, db, opts)
		if len(r.Hits) != 3 {
			t.Fatalf("legacy=%t origin filtering: %v", legacy, r.Hits)
		}
		for _, h := range r.Hits {
			if h.Title == nil || h.GitBranch == nil && h.Source == RolloutMain {
				t.Fatal("metadata missing", h)
			}
		}
	}
}
