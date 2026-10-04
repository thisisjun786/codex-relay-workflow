package recall

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"
)

type indexQueryOracle struct {
	Seeds struct{ Files, Msgs [][]any }
	Cases []oracleCase
}

func queryOracle(t *testing.T) indexQueryOracle {
	t.Helper()
	data, err := os.ReadFile("testdata/indexquery/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var result indexQueryOracle
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func queryOracleOptions(t *testing.T, c oracleCase) resolvedQuery {
	t.Helper()
	v := arg[struct {
		IndexQueryOptions
		RepoThreadIDs    []string `json:"repoThreadIds"`
		HasRepoKeyColumn bool     `json:"hasRepoKeyColumn"`
	}](t, c, 0)
	return resolvedQuery{v.IndexQueryOptions, v.RepoThreadIDs, v.HasRepoKeyColumn}
}

func queryOracleNumber(t *testing.T, c oracleCase, i int) float64 {
	t.Helper()
	if c.In[i][0] != '"' {
		return arg[float64](t, c, i)
	}
	v, err := strconv.ParseFloat(arg[string](t, c, i), 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIndexQueryOracle(t *testing.T) {
	seen := map[string]int{}
	for i, c := range queryOracle(t).Cases {
		if c.Fn == "candidates" {
			continue
		}
		seen[c.Fn]++
		var got any
		switch c.Fn {
		case "constants":
			got = []any{maxRepoThreadIDs, RRFK, LaneWeightFTS, LaneWeightTri, float64(RecencyWeight), RecencyHalfLifeHours, relaxedPool, maxOptionalLaneWords}
		case "pool", "planPool":
			n := 0.0
			if c.Fn == "pool" {
				n = poolSize(queryOracleNumber(t, c, 0))
			} else {
				n = planPoolSize(arg[MatchPlan](t, c, 0), queryOracleNumber(t, c, 1))
			}
			if !math.IsNaN(n) {
				got = n // JSON.stringify(NaN) is null in the recorded oracle.
			}
		case "lane":
			words, anyMode := laneQuery(arg[MatchPlan](t, c, 0))
			got = map[string]any{"words": words, "anyMode": anyMode}
		case "text":
			got = textMatches(arg[string](t, c, 0), arg[MatchPlan](t, c, 1))
		case "quote":
			word := arg[string](t, c, 0)
			got = []string{ftsQuote(word), escapeLike(word)}
		case "word":
			params := []any{}
			where := wordCondition(arg[string](t, c, 0), &params)
			got = []any{where, params}
		case "group":
			params := []any{"preceding"}
			where := groupCondition(arg[QueryGroup](t, c, 0), &params)
			got = []any{where, params}
		case "filter":
			q, withWords, fold := queryOracleOptions(t, c), arg[bool](t, c, 1), arg[bool](t, c, 2)
			where, params := candidateFilterFor(q, withWords, fold)
			if fold == FoldCwdCase() {
				nativeWhere, nativeParams := candidateFilter(q, withWords)
				if nativeWhere != where || !reflect.DeepEqual(nativeParams, params) {
					t.Fatal("runtime platform wrapper disagrees with its recorded branch")
				}
			}
			got = []any{where, params}
		case "origin":
			meta := ThreadMetaResult{ByID: map[string]ThreadMeta{}}
			for _, entry := range arg[[][]json.RawMessage](t, c, 0) {
				var id string
				var v struct {
					GitOriginURL *string `json:"gitOriginUrl"`
				}
				if err := json.Unmarshal(entry[0], &id); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(entry[1], &v); err != nil {
					t.Fatal(err)
				}
				meta.IDs = append(meta.IDs, id)
				meta.ByID[id] = ThreadMeta{GitOriginURL: v.GitOriginURL}
			}
			got = sameOriginThreadIDs(meta, arg[string](t, c, 1))
		case "originCap":
			origin := "git@example.test:org/repo.git"
			meta := ThreadMetaResult{ByID: map[string]ThreadMeta{}}
			for j := 0; j < 5003; j++ {
				id := "t" + strconv.Itoa(j)
				meta.IDs = append(meta.IDs, id)
				meta.ByID[id] = ThreadMeta{GitOriginURL: &origin}
			}
			ids := sameOriginThreadIDs(meta, "example.test/org/repo")
			got = []any{len(ids), ids[0], ids[len(ids)-1]}
		default:
			t.Fatalf("unknown oracle function %q", c.Fn)
		}
		var want any
		if err := json.Unmarshal(c.Out, &want); err != nil {
			t.Fatal(err)
		}
		if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
			t.Errorf("case %d %s %s: got %v, oracle %v", i, c.Fn, c.In, actual, want)
		}
	}
	if len(seen) != 11 || seen["filter"] < 100 {
		t.Fatalf("incomplete oracle families: %v", seen)
	}
}

func TestIndexQueryCandidates(t *testing.T) {
	oracle := queryOracle(t)
	for _, legacy := range []bool{false, true} {
		db, path := indexTestDB(t)
		for _, row := range oracle.Seeds.Files {
			if _, err := recallStmt(t, db, "INSERT INTO files(path,mtime_ms,size,thread_id,cwd,source,date,repo_key) VALUES(?,?,?,?,?,?,?,?)").Run(row...); err != nil {
				t.Fatal(err)
			}
		}
		for _, row := range oracle.Seeds.Msgs {
			if _, err := recallStmt(t, db, "INSERT INTO msgs(id,path,ord,ts,role,match_field,synthetic,text) VALUES(?,?,?,?,?,?,?,?)").Run(row...); err != nil {
				t.Fatal(err)
			}
		}
		if legacy {
			if err := db.Exec("DROP INDEX idx_files_repo_key; ALTER TABLE files DROP COLUMN repo_key"); err != nil {
				t.Fatal(err)
			}
		}
		reader, err := openIndexReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		if filesHasColumn(reader, "repo_key") == legacy {
			t.Fatal("wrong test schema")
		}
		seen := 0
		for i, c := range oracle.Cases {
			if c.Fn != "candidates" || arg[bool](t, c, 3) != legacy {
				continue
			}
			seen++
			q := queryOracleOptions(t, c)
			where, params := candidateFilterFor(q, arg[bool](t, c, 1), arg[bool](t, c, 2))
			stmt, err := reader.Prepare("SELECT m.id,m.text FROM msgs m JOIN files f ON f.path=m.path WHERE " + where + " ORDER BY m.id")
			if err != nil {
				t.Fatal(err)
			}
			rows, err := stmt.All(params...)
			got := map[string]any{"ids": []any{}, "hits": []any{}, "error": nil}
			if err != nil {
				got["error"] = err.Error()
			} else {
				for _, r := range rows {
					got["ids"] = append(got["ids"].([]any), r["id"])
					if textMatches(r["text"].(string), q.Plan) {
						got["hits"] = append(got["hits"].([]any), r["id"])
					}
				}
			}
			var want any
			if err := json.Unmarshal(c.Out, &want); err != nil {
				t.Fatal(err)
			}
			if actual := canon(t, got); !reflect.DeepEqual(actual, want) {
				t.Errorf("case %d legacy=%v: got %v oracle %v", i, legacy, actual, want)
			}
		}
		if seen < 100 {
			t.Fatalf("legacy=%v: only %d recorded candidate cases", legacy, seen)
		}
		if n := recallRow(t, recallStmt(t, reader, "SELECT COUNT(*) AS n FROM msgs"))["n"].(float64); int(n) != len(oracle.Seeds.Msgs) {
			t.Fatal("hostile input changed the indexed rows")
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
