package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const requeueTestSchema = "CREATE TABLE jobs (kind TEXT NOT NULL, job_key TEXT NOT NULL, status TEXT NOT NULL, retry_remaining INTEGER NOT NULL, retry_at INTEGER, last_error TEXT, input_watermark INTEGER, last_success_watermark INTEGER)"

func requeueTestHome(t *testing.T, sql ...string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRW_HOME", t.TempDir())
	return memoryStatusHome(t, memoryStatusOracle{SQL: sql})
}

type requeueOracleCase struct {
	ID, Mode, Text, SnapshotSQL string
	SQL, Newer                  []string
	Options                     map[string]any
	Result                      json.RawMessage
	Rows                        []map[string]any
}

func requeueOracleOptions(t *testing.T, values map[string]any) RequeueOptions {
	t.Helper()
	opts := RequeueOptions{}
	opts.Apply, _ = values["apply"].(bool)
	opts.IncludeContextWindow, _ = values["includeContextWindow"].(bool)
	opts.Kind, _ = values["kind"].(string)
	for key, target := range map[string]**float64{"limit": &opts.Limit, "retries": &opts.Retries} {
		if value, ok := values[key]; ok {
			n, ok := value.(float64)
			if !ok {
				var err error
				n, err = strconv.ParseFloat(value.(string), 64)
				if err != nil {
					t.Fatal(err)
				}
			}
			*target = &n
		}
	}
	return opts
}

func TestMemoryRequeueOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/memoryrequeue/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Transient []string
		Cases     []requeueOracleCase
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if len(oracle.Cases) != 44 || !reflect.DeepEqual(TransientCauses(), oracle.Transient) {
		t.Fatal("incomplete oracle grid")
	}
	for _, c := range oracle.Cases {
		t.Run(c.ID, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CRW_HOME", t.TempDir())
			home := memoryStatusHome(t, memoryStatusOracle{SQL: c.SQL, Newer: c.Newer, Mode: c.Mode})
			before := memoryStatusFiles(t, home)
			r := RequeueExhaustedMemoryJobs(home, requeueOracleOptions(t, c.Options))
			if c.Mode == "directory" {
				// port: fixed (docs/port-cxc/known-defects/CRW-1123.md): a directory named like the store is no store.
				if r.State != MemoryStatusUnavailable || r.StorePath != nil || r.Applied || r.Detail != "no memories store found under "+home {
					t.Fatalf("a directory is no memories store: %+v", r)
				}
				return
			}
			got, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var actual, want any
			if err := json.Unmarshal([]byte(strings.ReplaceAll(string(got), home, "<HOME>")), &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(c.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("result: got %s oracle %s", got, c.Result)
			}
			if text := strings.ReplaceAll(FormatRequeue(r), home, "<HOME>"); text != c.Text {
				t.Fatalf("text: got %q oracle %q", text, c.Text)
			}
			if !r.Applied && !reflect.DeepEqual(memoryStatusFiles(t, home), before) {
				t.Fatal("unsuccessful/dry requeue changed store bytes")
			}
			if c.SnapshotSQL == "" {
				return
			}
			path, err := memoriesDbPath(home)
			if err != nil {
				t.Fatal(err)
			}
			db, err := openDbReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := recallStmt(t, db, c.SnapshotSQL).All()
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				for key, value := range row {
					if blob, ok := value.([]byte); ok {
						obj := map[string]any{}
						for i, b := range blob {
							obj[strconv.Itoa(i)] = float64(b)
						}
						row[key] = obj // JSON.stringify(Uint8Array) is an indexed object.
					}
				}
			}
			if !reflect.DeepEqual(rows, c.Rows) {
				t.Fatalf("post-state: got %v oracle %v", rows, c.Rows)
			}
		})
	}
}

func TestMemoryRequeueLockedTransaction(t *testing.T) {
	home := requeueTestHome(t, requeueTestSchema, requeueTestRow("a", "capacity"))
	before := requeueTestRows(t, home)
	db, err := openDbReadWrite(filepath.Join(home, "memories_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recallSQL(t, db, "BEGIN IMMEDIATE")
	r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: true})
	recallSQL(t, db, "ROLLBACK")
	if r.State != MemoryStatusUnavailable || r.Applied || r.Changed != 0 || len(r.Selected) != 1 || !strings.Contains(r.Detail, "database is locked") {
		t.Fatalf("lock: %+v", r)
	}
	if !reflect.DeepEqual(requeueTestRows(t, home), before) {
		t.Fatal("locked apply changed rows")
	}
}

func requeueTestRow(key, cause string) string {
	return "INSERT INTO jobs VALUES ('memory_stage1','" + key + "','error',0,999,'" + cause + "',10,5)"
}

func requeueTestRows(t *testing.T, home string) []map[string]any {
	t.Helper()
	db, err := openDbReadOnly(filepath.Join(home, "memories_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := recallStmt(t, db, "SELECT * FROM jobs ORDER BY job_key").All()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func requeueKeys(result RequeueResult) []string {
	keys := []string{}
	for _, c := range result.Selected {
		keys = append(keys, c.JobKey)
	}
	return keys
}

func requeueNumber(n float64) *float64 { return &n }

// The eight B-class cases from upstream memory-requeue.test.ts:48-155.
func TestMemoryRequeueB(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		home := requeueTestHome(t, requeueTestSchema, requeueTestRow("a", "stream closed early"), requeueTestRow("b", "context window exceeded"))
		before := memoryStatusFiles(t, home)
		r := RequeueExhaustedMemoryJobs(home)
		if r.State != MemoryStatusOK || r.Applied || r.Changed != 0 || !reflect.DeepEqual(requeueKeys(r), []string{"a"}) || !reflect.DeepEqual(r.SkippedByCause, CauseCounts{{"context-window", 1}}) {
			t.Fatalf("dry run: %+v", r)
		}
		if !reflect.DeepEqual(memoryStatusFiles(t, home), before) {
			t.Fatal("dry run wrote store")
		}
		if !strings.Contains(FormatRequeue(r), "dry run") {
			t.Fatal("missing dry run report")
		}
	})
	t.Run("apply preserves evidence", func(t *testing.T) {
		home := requeueTestHome(t, requeueTestSchema, requeueTestRow("a", "429 rate limit"))
		r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: true, Retries: requeueNumber(2)})
		if !r.Applied || r.Changed != 1 {
			t.Fatalf("apply: %+v", r)
		}
		row := requeueTestRows(t, home)[0]
		for key, want := range map[string]any{"retry_remaining": float64(2), "retry_at": nil, "status": "error", "last_error": "429 rate limit", "input_watermark": float64(10), "last_success_watermark": float64(5)} {
			if !reflect.DeepEqual(row[key], want) {
				t.Errorf("%s: got %v want %v", key, row[key], want)
			}
		}
	})
	t.Run("context window requires opt in", func(t *testing.T) {
		home := requeueTestHome(t, requeueTestSchema, requeueTestRow("a", "context window exceeded"))
		if len(RequeueExhaustedMemoryJobs(home).Selected) != 0 {
			t.Fatal("context selected by default")
		}
		r := RequeueExhaustedMemoryJobs(home, RequeueOptions{IncludeContextWindow: true})
		if len(r.Selected) != 1 || r.Selected[0].Cause != "context-window" {
			t.Fatalf("opt in: %+v", r)
		}
	})
	t.Run("only exhausted errors", func(t *testing.T) {
		home := requeueTestHome(t, requeueTestSchema,
			"INSERT INTO jobs VALUES ('memory_stage1','done','done',0,NULL,NULL,1,1)",
			"INSERT INTO jobs VALUES ('memory_stage1','running','running',0,NULL,NULL,1,1)",
			"INSERT INTO jobs VALUES ('memory_stage1','retryable','error',2,5,'capacity',1,1)", requeueTestRow("target", "capacity"))
		r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: true})
		if !reflect.DeepEqual(requeueKeys(r), []string{"target"}) || r.Changed != 1 {
			t.Fatalf("selection: %+v", r)
		}
		rows := requeueTestRows(t, home)
		if rows[1]["retry_at"] != float64(5) || rows[2]["retry_remaining"] != float64(0) {
			t.Fatalf("non-target rows changed: %v", rows)
		}
	})
	t.Run("kind and limit", func(t *testing.T) {
		home := requeueTestHome(t, requeueTestSchema, requeueTestRow("a", "capacity"), requeueTestRow("b", "capacity"), "INSERT INTO jobs VALUES ('memory_consolidate_global','c','error',0,999,'capacity',10,5)")
		if r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Kind: "memory_stage1"}); !reflect.DeepEqual(requeueKeys(r), []string{"a", "b"}) {
			t.Fatalf("kind: %+v", r)
		}
		if len(RequeueExhaustedMemoryJobs(home, RequeueOptions{Limit: requeueNumber(1)}).Selected) != 1 {
			t.Fatal("limit not honored")
		}
	})
	t.Run("schema guard", func(t *testing.T) {
		home := requeueTestHome(t, "CREATE TABLE jobs (kind TEXT NOT NULL, status TEXT NOT NULL)")
		before := memoryStatusFiles(t, home)
		r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: true})
		if r.State != MemoryStatusUnsupported || r.Applied || !strings.Contains(r.Detail, "retry_remaining") {
			t.Fatalf("schema: %+v", r)
		}
		if !reflect.DeepEqual(memoryStatusFiles(t, home), before) {
			t.Fatal("schema guard wrote store")
		}
	})
	t.Run("missing store", func(t *testing.T) {
		home := requeueTestHome(t)
		r := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: true})
		if r.State != MemoryStatusUnavailable || r.Applied {
			t.Fatalf("missing: %+v", r)
		}
		if len(memoryStatusFiles(t, home)) != 0 {
			t.Fatal("missing store created files")
		}
	})
	t.Run("transient cause set", func(t *testing.T) {
		if slices.Contains(TransientCauses(), "context-window") {
			t.Fatal("context is not transient")
		}
		for _, c := range []string{"capacity", "incomplete-response", "stream-closed", "unknown", "other"} {
			if !slices.Contains(TransientCauses(), c) {
				t.Error("missing transient cause:", c)
			}
		}
	})
}
