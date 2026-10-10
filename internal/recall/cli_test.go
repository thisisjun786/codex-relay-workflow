package recall

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

func recallCLIInvoke(t *testing.T, args []string, now time.Time) (int, string, string) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = original }()
	var out strings.Builder
	code := Run(args, &out, file, now)
	os.Stderr = original
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), string(data)
}

func recallCLIHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		dir := filepath.Join(root, key)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	return os.Getenv("CODEX_HOME")
}

// Fifteen cliMain scenarios plus the owner-level sixteenth from
// CXC recall/test/cli-arg-hygiene.test.ts:31-277, with isolated homes.
func TestRecallCLIHygiene(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		code       int
		diagnostic string
	}{
		{"index-help", []string{"chat", "index", "--help"}, 0, ""},
		{"help-before-rebuild", []string{"chat", "index", "--rebuild", "--help"}, 0, ""},
		{"memory-search-help", []string{"memory", "search", "--help"}, 0, ""},
		{"dash-h", []string{"chat", "index", "-h"}, 0, ""},
		{"index-word-help", []string{"chat", "index", "help"}, 0, ""},
		{"index-slash-help", []string{"chat", "index", "/?"}, 0, ""},
		{"memory-help-query", []string{"memory", "search", "help", "--no-chat", "--json"}, 0, ""},
		{"chat-help-query", []string{"chat", "search", "help", "--scan", "--json"}, 0, ""},
		{"dash-cwd-only", []string{"memory", "search", "q", "--cwd-only", "--no-chat"}, 1, "cwd-only"},
		{"equal-cwd-only", []string{"memory", "search", "q", "--cwd-only=--no-chat"}, 1, "path must not start"},
		{"dash-cwd-home", []string{"chat", "search", "q", "--cwd", "--json", "--scan"}, 1, "cwd"},
		{"equal-cwd-home", []string{"chat", "search", "q", "--cwd=--json", "--scan"}, 1, "path must not start"},
		{"missing-cwd-only", []string{"memory", "search", "q", "--cwd-only"}, 1, "cwd-only"},
		{"unknown", []string{"chat", "index", "--not-a-flag"}, 1, "not-a-flag"},
		{"missing-home", []string{"memory", "search", "foo", "--home", "<MISSING>", "--no-chat", "--json"}, 1, "--home not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := recallCLIHome(t)
			idx := filepath.Join(os.Getenv("CRW_HOME"), "recall", "index.sqlite")
			args := slices.Clone(c.args)
			for i, a := range args {
				if a == "<MISSING>" {
					args[i] = filepath.Join(home, "missing")
				}
			}
			args = append(args, "--index-path", idx)
			var before []byte
			if c.name == "help-before-rebuild" {
				home = buildRecallCodexHome(t, time.Now())
				if code, _, e := recallCLIInvoke(t, []string{"chat", "index", "--home", home, "--index-path", idx}, time.Now()); code != 0 {
					t.Fatal(e)
				}
				before, _ = os.ReadFile(idx)
			}
			code, out, e := recallCLIInvoke(t, args, time.Now())
			if code != c.code || !strings.Contains(e, c.diagnostic) {
				t.Fatalf("code %d, stdout %s, stderr %s", code, out, e)
			}
			if c.code != 0 && out != "" {
				t.Fatal("rejection wrote stdout", out)
			}
			if strings.Contains(c.name, "query") {
				var result map[string]any
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if _, ok := result["hits"].([]any); !ok {
					t.Fatal(result)
				}
			} else if c.code == 0 && !strings.Contains(out, "crw recall chat search") {
				t.Fatal(out)
			}
			if before != nil {
				after, _ := os.ReadFile(idx)
				if string(before) != string(after) {
					t.Fatal("help changed index")
				}
			} else if _, err := os.Stat(idx); err == nil {
				t.Fatal("help/rejection created index")
			}
			if c.name == "dash-cwd-home" || c.name == "equal-cwd-home" {
				flag := []string{"--home", "--json"}
				if c.name == "equal-cwd-home" {
					flag = []string{"--home=--json"}
				}
				code, out, e = recallCLIInvoke(t, append([]string{"memory", "search", "q"}, flag...), time.Now())
				if code != 1 || out != "" || !strings.Contains(e, "home") {
					t.Fatal(code, out, e)
				}
			}
		})
	}
	t.Run("missing-memory-roots-warn-once", func(t *testing.T) {
		home := recallCLIHome(t)
		for _, q := range []string{"anything", "foo"} {
			result, err := SearchMemory(q, MemorySearchOptions{Home: &home})
			if err != nil || len(result.Hits) != 0 {
				t.Fatal(result, err)
			}
			for _, w := range []string{"memories root not found (file search off)", "memories db not found (stage1 search off)"} {
				count := 0
				for _, got := range result.Warnings {
					if got == w {
						count++
					}
				}
				if count != 1 {
					t.Fatal(result.Warnings)
				}
			}
		}
	})
}

func TestRecallCLIRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/cli/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Argv           []string
		Code           int
		Stdout, Stderr string
	}
	if err = json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			home := recallCLIHome(t)
			idx := filepath.Join(home, "index.sqlite")
			expand := strings.NewReplacer("<HOME>", home, "<INDEX>", idx)
			args := slices.Clone(row.Argv)
			for i := range args {
				args[i] = expand.Replace(args[i])
			}
			code, out, e := recallCLIInvoke(t, args, time.Now())
			norm := strings.NewReplacer(home, "<HOME>", idx, "<INDEX>")
			out, e = norm.Replace(out), norm.Replace(e)
			if strings.Contains(out, `"elapsedMs"`) {
				var a, b map[string]any
				json.Unmarshal([]byte(out), &a)
				json.Unmarshal([]byte(row.Stdout), &b)
				if n, ok := a["elapsedMs"].(float64); !ok || n < 0 {
					t.Fatal("missing/invalid elapsedMs", out)
				}
				delete(a, "elapsedMs")
				delete(b, "elapsedMs")
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("got %s want %s", out, row.Stdout)
				}
			} else if out != row.Stdout {
				t.Fatalf("got %q want %q", out, row.Stdout)
			}
			if code != row.Code || e != row.Stderr {
				t.Fatal(code, e, row.Code, row.Stderr)
			}
		})
	}
}

// recallCLIUsageWithVerify is the recorded usage text with the lines of --verify (CRW-1083), which the oracle does not
// have: the synopsis of `chat index` lists it and a flag line follows --full. Text without the usage is returned as is.
func recallCLIUsageWithVerify(text string) string {
	text = strings.Replace(text, "chat index [--rebuild] [--status] [--json]", "chat index [--rebuild] [--status] [--verify] [--json]", 1)
	const full = "  --full       with --json: emit unclipped text fields\n"
	return strings.Replace(text, full, full+"  --verify     chat index: decide freshness from file content, not only size and mtime (with --status, report without writing)\n", 1)
}

func TestUsageListsChatIndexVerify(t *testing.T) {
	u := Usage()
	for _, want := range []string{"crw recall chat index [--rebuild] [--status] [--verify] [--json]", "  --verify     chat index: "} {
		if !strings.Contains(u, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
	skill, err := os.ReadFile(filepath.Join("..", "..", "plugins", "crw", "skills", "crw-recall", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "crw recall chat index [--rebuild] [--status] [--verify] [--json]") {
		t.Error("the crw-recall skill does not list chat index --verify")
	}
}

// recallCLILaneScoreFixes holds the scores of recorded fixtures that the eligibility-first lane ranking changes.
var recallCLILaneScoreFixes = map[string][]float64{
	"cli__chat__search_refresh_builds_index": {0.029749663773784223, 0.02901671452121108, 0.02885045852148274, 0.028563885540156295},
}

// The real binary replay has no frozen clock or bare-memory help mapping.
// These cases use the unchanged recorded givens and expectations through Run.
func TestRecallCLIRecordedCorpus(t *testing.T) {
	root := filepath.Join("..", "..")
	sub, err := cxccorpus.LoadSubstitution(root)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := cxccorpus.LoadRules(root)
	if err != nil {
		t.Fatal(err)
	}
	normal, err = normal.Renamed(sub)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, cxccorpus.FixtureDir, "cli*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if !strings.HasPrefix(id, "cli__chat__") && !strings.HasPrefix(id, "cli__memory__") && !strings.HasPrefix(id, "cli-help__chat__") && !strings.HasPrefix(id, "cli-help__memory__") {
			continue
		}
		if strings.Contains(id, "allow-write") {
			continue
		}
		fixture, err := cxccorpus.LoadFixture(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(id, func(t *testing.T) {
			c, err := cxccorpus.NewCase(t.TempDir(), "CRW_HOME", fixture.Given)
			if err != nil {
				t.Fatal(err)
			}
			pairs := []string{}
			for _, b := range c.Bindings() {
				pairs = append(pairs, b.Placeholder, b.Path)
			}
			expand := strings.NewReplacer(pairs...)
			for _, env := range c.Env {
				key, v, _ := strings.Cut(env, "=")
				if key != "PATH" {
					t.Setenv(key, v)
				}
			}
			t.Chdir(filepath.Join(c.Root, "ws"))
			write := func(p, body string) { writeRolloutTestFile(t, c.Root, p, expand.Replace(body)) }
			for p, b := range fixture.Given.Files {
				write(p, b)
			}
			for p, b := range fixture.Given.JSON {
				write(p, string(b))
			}
			for _, p := range fixture.Given.Dirs {
				if err := os.MkdirAll(filepath.Join(c.Root, p), 0755); err != nil {
					t.Fatal(err)
				}
			}
			for p, sql := range fixture.Given.SQLite {
				db, err := openDbReadWrite(filepath.Join(c.Root, p))
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range sql {
					if err = db.Exec(expand.Replace(s)); err != nil {
						t.Fatal(err)
					}
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.Given.Git != nil {
				for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", fixture.Given.Git.Origin}} {
					cmd := exec.Command("git", args...)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatal(err, string(out))
					}
				}
			}
			if err := filepath.Walk(c.Root, func(p string, info os.FileInfo, e error) error {
				if e != nil {
					return e
				}
				return os.Chtimes(p, cxccorpus.Epoch(), cxccorpus.Epoch())
			}); err != nil {
				t.Fatal(err)
			}
			session := normal.NewSession(c.Bindings())
			for i, step := range fixture.Run.Steps {
				args, ok := sub.MapArgv(step.CLI)
				if !ok {
					args = append([]string{"recall"}, step.CLI...)
				}
				if len(args) == 0 || args[0] != "recall" {
					t.Fatal("not recall", args)
				}
				args = slices.Clone(args[1:])
				for j := range args {
					args[j] = expand.Replace(args[j])
				}
				now := cxccorpus.Epoch()
				if len(args) > 1 && args[0] == "memory" && args[1] == "search" {
					now = now.Add(time.Millisecond)
				}
				code, out, e := recallCLIInvoke(t, args, now)
				out, e = session.Text(out), session.Stderr(e)
				want := fixture.Expect.Steps[i]
				if code != want.Exit || e != sub.Expected(want.Stderr) {
					t.Errorf("step%d code%d/%d stderr %q/%q", i, code, want.Exit, e, sub.Expected(want.Stderr))
				}
				if want.StdoutForm == "empty" {
					if out != "" {
						t.Error(out)
					}
					continue
				}
				if want.Stdout != nil {
					wantOut := recallCLIUsageWithVerify(sub.Expected(*want.Stdout))
					if out != wantOut {
						t.Errorf("step%d stdout got %q want %q", i, out, wantOut)
					}
					continue
				}
				var actual, expected any
				if err = json.Unmarshal([]byte(out), &actual); err != nil {
					t.Fatal(err, out)
				}
				if err = json.Unmarshal([]byte(sub.Expected(string(want.StdoutJSON))), &expected); err != nil {
					t.Fatal(err)
				}
				// port: fixed (docs/port-cxc/known-defects/CRW-1087.md): lane ranks are taken among eligible rows.
				if scores, ok := recallCLILaneScoreFixes[id]; ok {
					for i, hit := range expected.(map[string]any)["hits"].([]any) {
						hit.(map[string]any)["score"] = scores[i]
					}
				}
				if !recallCLICompareJSON(actual, expected) {
					t.Errorf("step%d got %s want %s", i, out, sub.Expected(string(want.StdoutJSON)))
				}
			}
		})
	}
}

func recallCLICompareJSON(actual, expected any) bool {
	// Only hits[].score absorbs <=1e-8: the oracle ticks Date.now() on each
	// internal read, whereas existing owners accept one per-call rank timestamp.
	a, ok := actual.(map[string]any)
	b, bok := expected.(map[string]any)
	if ok && bok {
		ah, _ := a["hits"].([]any)
		bh, _ := b["hits"].([]any)
		if len(ah) == len(bh) {
			for i := range ah {
				am, ao := ah[i].(map[string]any)
				bm, bo := bh[i].(map[string]any)
				if ao && bo {
					as, an := am["score"].(float64)
					bs, bn := bm["score"].(float64)
					if an && bn && math.Abs(as-bs) <= 1e-8 {
						am["score"] = bs
					}
				}
			}
		}
	}
	return reflect.DeepEqual(actual, expected)
}

func TestRecallCLINotices(t *testing.T) {
	home := recallCLIHome(t)
	idx := filepath.Join(os.Getenv("CRW_HOME"), "index.sqlite")
	if MemoryPipelineNotice(home) != "" || IndexStatusLine(home, idx) != "" {
		t.Fatal("missing store not silent")
	}
	db, err := openIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := IndexStatusLine(home, idx); got != "0 files / 0 messages, 0 source, 0 stale, last ingest never" {
		t.Fatal(got)
	}
	before, _ := os.ReadFile(idx)
	IndexStatusLine(home, idx)
	after, _ := os.ReadFile(idx)
	if string(before) != string(after) {
		t.Fatal("banner writes")
	}
	recallFixtureDB(t, home, "memories_1.sqlite", "CREATE TABLE jobs(kind,status,retry_remaining,last_error,finished_at)", "INSERT INTO jobs VALUES (?, ?, ?, ?, ?)", []any{"stage1", "error", 0, "capacity", nil})
	if got := MemoryPipelineNotice(home); !strings.Contains(got, "1 job(s) exhausted") {
		t.Fatal(got)
	}
	bad := filepath.Join(home, "bad")
	os.Mkdir(bad, 0700)
	os.Mkdir(filepath.Join(bad, "memories_1.sqlite"), 0700)
	if MemoryPipelineNotice(bad) != "" || IndexStatusLine(home, bad) != "" {
		t.Fatal("unreadable store not silent")
	}
}

func TestRecallCLIManagementAndSearchEdges(t *testing.T) {
	t.Run("lax-flags-do-not-accidentally-apply", func(t *testing.T) {
		home := recallCLIHome(t)
		recallFixtureDB(t, home, "memories_1.sqlite", requeueTestSchema, "INSERT INTO jobs VALUES (?, ?, ?, ?, ?, ?, ?, ?)", []any{"stage1", "job", "error", 0, 999, "capacity", 10, 5})
		before := requeueTestRows(t, home)
		for _, flags := range [][]string{{"--apply=false"}, {"--limit", "--apply"}, {"--retries"}} {
			args := append([]string{"memory", "requeue", "--json"}, flags...)
			code, out, e := recallCLIInvoke(t, args, time.Now())
			var r RequeueResult
			if json.Unmarshal([]byte(out), &r) != nil || code != 0 || e != "" || r.Applied || r.Changed != 0 || len(r.Selected) != 1 {
				t.Fatal(code, out, e)
			}
			if !reflect.DeepEqual(before, requeueTestRows(t, home)) {
				t.Fatal("dry-run changed jobs")
			}
		}
		code, out, e := recallCLIInvoke(t, []string{"memory", "requeue", "--apply", "--retries=5tail", "--json"}, time.Now())
		var r RequeueResult
		json.Unmarshal([]byte(out), &r)
		if code != 0 || e != "" || !r.Applied || r.Changed != 1 || r.Retries != 5 {
			t.Fatal(code, out, e)
		}
	})
	t.Run("json-clipping-full-and-flag-defaults", func(t *testing.T) {
		home := recallCLIHome(t)
		now := time.Now().UTC()
		recallFixtureRollout(t, home, now, 0, "01", recallThreadMain, "/example", "", false, false, [][2]string{{"user", "needle " + strings.Repeat("z", 600)}})
		for _, full := range []bool{false, true} {
			args := []string{"chat", "search", "needle", "--scan", "--json"}
			if full {
				args = append(args, "--full")
			}
			code, out, e := recallCLIInvoke(t, args, now)
			var r struct {
				Hits    []ChatHit
				Clipped bool
			}
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatal(err)
			}
			if code != 0 || e != "" || len(r.Hits) != 1 || r.Clipped == full {
				t.Fatal(code, out, e)
			}
			want := 503
			if full {
				want = 607
			}
			if len(r.Hits[0].Text) != want {
				t.Fatal(len(r.Hits[0].Text), want)
			}
		}
	})
}

func TestRecallCLIReviewerRegressions(t *testing.T) {
	home := recallCLIHome(t)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b-<&>")
	code, out, e := recallCLIInvoke(t, []string{"memory", "requeue", "--home", a, "--limit retries", "--home", b, "--json"}, time.Now())
	if code != 1 || e != "" || !strings.Contains(out, b) || strings.Contains(out, a) {
		t.Fatal("unknown option consumed home", code, out, e)
	}
	code, out, e = recallCLIInvoke(t, []string{"memory", "status", "--home", b, "--json"}, time.Now())
	if code != 1 || e != "" || !strings.Contains(out, b) || strings.Contains(out, `\u003c`) {
		t.Fatal("JSON escaped HTML", code, out, e)
	}
}

func TestRecallCLIHTMLAndLiteralEscapes(t *testing.T) {
	var out, errOut strings.Builder
	input := []string{"<&>", `\u003c\u003e\u0026`}
	if recallCLIJSON(&out, &errOut, input) != 0 || errOut.Len() != 0 {
		t.Fatal(errOut.String())
	}
	var got []string
	if json.Unmarshal([]byte(out.String()), &got) != nil || !reflect.DeepEqual(got, input) || !strings.Contains(out.String(), "<&>") {
		t.Fatal(out.String())
	}
}

// recallRebuildIndex addresses a synthetic Codex home and a sidecar index in temporary directories;
// HOME, CODEX_HOME and CRW_HOME are temporary, so nothing reaches the real ones.
type recallRebuildIndex struct{ home, idx string }

func recallRebuildNew(t *testing.T) recallRebuildIndex {
	t.Helper()
	recallCLIHome(t)
	return recallRebuildIndex{buildIngestCodexHome(t), filepath.Join(t.TempDir(), "index.sqlite")}
}

func (r recallRebuildIndex) run(t *testing.T, now time.Time, args ...string) (int, string, string) {
	t.Helper()
	return recallCLIInvoke(t, append(args, "--home", r.home, "--index-path", r.idx), now)
}

// ok runs a command that must succeed and returns stdout with the wall-clock fields replaced.
func (r recallRebuildIndex) ok(t *testing.T, now time.Time, args ...string) string {
	t.Helper()
	code, out, e := r.run(t, now, args...)
	if code != 0 || e != "" {
		t.Fatal(args, code, out, e)
	}
	return recallRebuildNormalize(out)
}

// recallRebuildNormalize replaces the elapsed milliseconds and the last-ingest time, the two wall-clock
// fields of the index command's text output.
func recallRebuildNormalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndex(l, ", "); strings.HasPrefix(l, "ingested ") && j >= 0 {
			lines[i] = l[:j] + ", Nms)"
		}
		if j := strings.Index(l, "last ingest: "); j >= 0 {
			lines[i] = l[:j] + "last ingest: T"
		}
	}
	return strings.Join(lines, "\n")
}

// search returns the --no-refresh JSON answer without its two wall-clock fields, elapsedMs (omitted
// when it is zero) and index.lastIngestAt; one fixed now keeps the score equal.
func (r recallRebuildIndex) search(t *testing.T, now time.Time) map[string]any {
	t.Helper()
	var got map[string]any
	err := json.Unmarshal([]byte(r.ok(t, now, "chat", "search", "deployed", "--no-refresh", "--days", "0", "--json")), &got)
	index, _ := got["index"].(map[string]any)
	if err != nil || got["hits"] == nil || index == nil {
		t.Fatal(got, err)
	}
	delete(index, "lastIngestAt")
	delete(got, "elapsedMs")
	return got
}

// snapshot reads every table the rebuild deletes from, including both FTS5 inverted indexes through MATCH
// (a bare rowid select reads the content table of an external-content FTS5 table and proves nothing).
func (r recallRebuildIndex) snapshot(t *testing.T, sql ...string) map[string][]map[string]any {
	t.Helper()
	db, err := openIndex(r.idx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range sql {
		recallSQL(t, db, q)
	}
	out := map[string][]map[string]any{}
	for name, q := range map[string]string{
		"files": "SELECT * FROM files ORDER BY path", "msgs": "SELECT * FROM msgs ORDER BY id",
		"msgs_fts": "SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH 'deployed' ORDER BY rowid",
		"msgs_tri": "SELECT rowid FROM msgs_tri WHERE msgs_tri MATCH 'deployed' ORDER BY rowid",
	} {
		out[name] = indexRows(t, db, q)
	}
	return out
}

// recallRebuildRequirePopulated stops a comparison of two empty indexes from passing.
func recallRebuildRequirePopulated(t *testing.T, s map[string][]map[string]any) {
	t.Helper()
	if len(s["files"]) != 4 || len(s["msgs"]) != 12 || len(s["msgs_fts"]) == 0 || len(s["msgs_tri"]) == 0 {
		t.Fatal("index is not the populated fixture", len(s["files"]), len(s["msgs"]), len(s["msgs_fts"]), len(s["msgs_tri"]))
	}
}

// A failed rebuild leaves the index exactly as it was: both deletes run in one transaction.
func TestRecallRebuildFaultKeepsIndex(t *testing.T) {
	for _, c := range []struct{ name, trigger, stderr string }{
		{"files", "BEFORE DELETE ON files BEGIN SELECT RAISE(ABORT,'fault'); END", "chat index failed: fault\n"},
		// The first statement aborts before anything changes, so this case also holds on code without the
		// transaction; it guards the symmetric failure and the error text.
		{"msgs", "BEFORE DELETE ON msgs BEGIN SELECT RAISE(ABORT,'fault'); END", "chat index failed: fault\n"},
		// A trigger raising ROLLBACK ends the transaction inside SQLite, so the rebuild's own ROLLBACK
		// finds nothing to roll back; the delete's reason must still be the one reported.
		{"files rollback", "BEFORE DELETE ON files BEGIN SELECT RAISE(ROLLBACK,'fault'); END", "chat index failed: fault\n"},
		{"msgs rollback", "BEFORE DELETE ON msgs BEGIN SELECT RAISE(ROLLBACK,'fault'); END", "chat index failed: fault\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, now := recallRebuildNew(t), time.Now()
			r.ok(t, now, "chat", "index")
			noop, search := r.ok(t, now, "chat", "index"), r.search(t, now)
			before := r.snapshot(t)
			recallRebuildRequirePopulated(t, before)
			r.snapshot(t, "CREATE TRIGGER recall_rebuild_fault "+c.trigger)
			code, out, e := r.run(t, now, "chat", "index", "--rebuild")
			if code != 1 || out != "" || !strings.HasPrefix(e, "chat index failed: ") || e != c.stderr {
				t.Fatal(code, out, e)
			}
			if !reflect.DeepEqual(before, r.snapshot(t)) || !reflect.DeepEqual(search, r.search(t, now)) {
				t.Error("failed rebuild changed the index")
			}
			r.snapshot(t, "DROP TRIGGER recall_rebuild_fault")
			if got := r.ok(t, now, "chat", "index"); got != noop || !reflect.DeepEqual(before, r.snapshot(t)) || !reflect.DeepEqual(search, r.search(t, now)) {
				t.Fatalf("index after the failed rebuild differs: %q want %q", got, noop)
			}
		})
	}
}

// A successful rebuild prints what the first build printed and re-creates the same rows.
func TestRecallRebuildSuccessOutput(t *testing.T) {
	r, now := recallRebuildNew(t), time.Now()
	first := r.ok(t, now, "chat", "index")
	before, search := r.snapshot(t), r.search(t, now)
	recallRebuildRequirePopulated(t, before)
	if got := r.ok(t, now, "chat", "index", "--rebuild"); got != first {
		t.Fatalf("rebuild printed %q, first build printed %q", got, first)
	}
	if !reflect.DeepEqual(before, r.snapshot(t)) || !reflect.DeepEqual(search, r.search(t, now)) {
		t.Fatal("rebuild did not re-create the same index")
	}
}

// CRW-1083: --verify is the explicit strong path. Without it the status is metadata-only and its
// output is the oracle's; with it a rewrite that kept size and mtime is found and replaced.
func TestRecallChatIndexVerify(t *testing.T) {
	r, now := recallRebuildNew(t), time.Now()
	r.ok(t, now, "chat", "index")
	files, err := ListRolloutFiles(r.home, 0)
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "-main.jsonl") {
			target = f.Path
		}
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(target)
	rewritten := append([]byte{}, body...)
	// Same length, same mtime: the first bytes are changed so that the message text differs.
	i := strings.Index(string(rewritten), "deployed")
	if i < 0 {
		t.Fatal("fixture has no 'deployed' message")
	}
	copy(rewritten[i:], "DEPLOYXX")
	if err := os.WriteFile(target, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	plain := r.ok(t, now, "chat", "index", "--status")
	if !strings.Contains(plain, "stale: 0,") || strings.Contains(plain, "content-verified") {
		t.Fatal("the metadata-only status changed its output:", plain)
	}
	if code, out, e := r.run(t, now, "chat", "index", "--status", "--verify", "--json"); code != 0 || e != "" || !strings.Contains(out, `"staleFiles": 1`) || !strings.Contains(out, `"freshness": "content-verified"`) {
		t.Fatal("the strong status did not see the rewrite:", code, out, e)
	}
	if code, out, e := r.run(t, now, "chat", "index", "--status", "--json"); code != 0 || e != "" || !strings.Contains(out, `"staleFiles": 0`) || strings.Contains(out, "freshness") {
		t.Fatal("the metadata-only JSON changed:", code, out, e)
	}
	text := r.ok(t, now, "chat", "index", "--verify")
	if !strings.Contains(text, "ingested 1/4 files") || !strings.Contains(text, "stale: 0 (content-verified)") {
		t.Fatal(text)
	}
	if again := r.ok(t, now, "chat", "index", "--verify"); !strings.Contains(again, "ingested 0/4 files, 0 appended") {
		t.Fatal(again)
	}
}
