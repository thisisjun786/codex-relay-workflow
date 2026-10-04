package goalplan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func writeTestPlan() *Goalplan {
	return BuildGoalplan(NewGoalplanInput{Objective: "Hello, World!!", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
}
func writeTestRequire(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func writeTestFile(t *testing.T, p string) []byte {
	t.Helper()
	b, e := os.ReadFile(p)
	writeTestRequire(t, e)
	return b
}

func TestWriteBuildRecordedOracle(t *testing.T) {
	var records struct {
		Builds []struct {
			ID     string
			Oracle json.RawMessage
		}
	}
	writeTestRequire(t, json.Unmarshal(writeTestFile(t, "testdata/write/oracle.json"), &records))
	if len(records.Builds) != 11 {
		t.Fatal("missing recorded cases")
	}
	for _, c := range records.Builds {
		t.Run(c.ID, func(t *testing.T) {
			in := NewGoalplanInput{Objective: "Hello, World!!", Now: func() string { return "2026-01-01T00:00:00.000Z" }}
			values := map[string]float64{"zero": 0, "negative": -7, "nan": math.NaN(), "inf": math.Inf(1), "negative-inf": math.Inf(-1), "two": 2, "fraction": 2.9, "three": 3, "future": 99}
			if n, ok := values[c.ID]; ok {
				in.SchemaVersion = &n
			}
			if c.ID == "criteria-host" {
				desktop, empty := SurfaceDesktop, CriterionSurface("")
				at := "2025-12-31T00:00:00.000Z"
				in.Objective = "Ship & <export> \u2028\\u2028"
				in.Host = &GoalplanHostLink{Armed: true, ArmedAt: &at, Source: HostSourceFreeze}
				in.Criteria = []NewGoalplanCriterion{{Scenario: "CSV export works"}, {Scenario: "Native shell", ExpectedEvidence: "screen", Surface: &desktop, Presented: PresentedNative}, {Scenario: "empty surface", Surface: &empty}}
			}
			got := BuildGoalplan(in)
			encoded, e := writeEncodePlan(*got)
			writeTestRequire(t, e)
			var want bytes.Buffer
			writeTestRequire(t, json.Compact(&want, c.Oracle))
			var actual bytes.Buffer
			writeTestRequire(t, json.Compact(&actual, encoded))
			if actual.String() != want.String() {
				t.Fatalf("got %s\nwant %s", actual.String(), want.String())
			}
		})
	}
	calls := 0
	p := BuildGoalplan(NewGoalplanInput{Objective: "o", Host: &GoalplanHostLink{Source: "invalid"}, Criteria: []NewGoalplanCriterion{{Scenario: "s", Presented: "invalid"}}, Now: func() string { calls++; return "t" }})
	if calls != 1 || p.CreatedAt != "t" || p.UpdatedAt != "t" || p.Host.Source != HostSourceNone || p.Criteria[0].Presented != "" {
		t.Fatalf("builder %+v calls%d", p, calls)
	}
}

func TestWriteRoundTripOrderModesAndCaller(t *testing.T) { // TS:452-458, 67-82
	cwd := t.TempDir()
	p := writeTestPlan()
	old := p.UpdatedAt
	writeTestRequire(t, WriteGoalplan(cwd, p))
	dir, e := GoalplanDir(cwd, p.Slug)
	writeTestRequire(t, e)
	raw := writeTestFile(t, filepath.Join(dir, GoalplanFile))
	if raw[len(raw)-1] == '\n' || bytes.Index(raw, []byte(`"schemaVersion"`)) > bytes.Index(raw, []byte(`"createdAt"`)) {
		t.Fatalf("fresh order/format %s", raw)
	}
	if p.UpdatedAt != old {
		t.Fatal("writer mutated its caller")
	}
	revived := ReadGoalplan(cwd, p.Slug)
	if revived == nil || revived.Objective != p.Objective || revived.UpdatedAt == old {
		t.Fatalf("roundtrip %+v", revived)
	}
	writeTestRequire(t, WriteGoalplan(cwd, revived))
	raw = writeTestFile(t, filepath.Join(dir, GoalplanFile))
	if bytes.Index(raw, []byte(`"schemaVersion"`)) < bytes.Index(raw, []byte(`"host"`)) {
		t.Fatalf("revived order %s", raw)
	}
	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Dir(dir): 0700, filepath.Join(dir, GoalplanFile): 0600} {
		info, e := os.Stat(path)
		writeTestRequire(t, e)
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s %o", path, info.Mode().Perm())
		}
	}
	if string(writeTestFile(t, filepath.Join(cwd, crwdir.DirName, ".gitignore"))) != crwdir.GitignoreText {
		t.Fatal("ignore bytes")
	}
	entries, e := os.ReadDir(dir)
	writeTestRequire(t, e)
	if len(entries) != 1 {
		t.Fatalf("orphan temp %v", entries)
	}
}

func TestWriteFailuresPreserveDestination(t *testing.T) {
	cwd := t.TempDir()
	p := writeTestPlan()
	p.Slug = "../../escape"
	if WriteGoalplan(cwd, p) == nil {
		t.Fatal("traversal accepted")
	}
	if _, e := os.Stat(filepath.Join(cwd, crwdir.DirName)); !os.IsNotExist(e) {
		t.Fatal("invalid slug created state")
	}
	p = writeTestPlan()
	dir, e := GoalplanDir(cwd, p.Slug)
	writeTestRequire(t, e)
	writeTestRequire(t, os.MkdirAll(filepath.Join(dir, GoalplanFile), 0700))
	writeTestRequire(t, os.WriteFile(filepath.Join(dir, GoalplanFile, "sentinel"), []byte("keep"), 0600))
	if WriteGoalplan(cwd, p) == nil {
		t.Fatal("replaced directory")
	}
	if string(writeTestFile(t, filepath.Join(dir, GoalplanFile, "sentinel"))) != "keep" {
		t.Fatal("destination lost")
	}
	entries, e := os.ReadDir(dir)
	writeTestRequire(t, e)
	if len(entries) != 1 {
		t.Fatalf("orphan temp %v", entries)
	}
}

func TestWriteLedgerStandaloneAndCallerLock(t *testing.T) { // TS:569-577
	cwd := t.TempDir()
	slug := "led"
	for i, event := range []GoalplanLedgerEvent{EventCreated, EventTaskDone} {
		writeTestRequire(t, AppendGoalplanLedger(cwd, slug, GoalplanLedgerEntry{Ts: fmt.Sprint(i), Slug: slug, Event: event, Detail: "<&>\u2028\\u2028\n"}))
	}
	path := filepath.Join(cwd, crwdir.DirName, GoalplansSubdir, slug, GoalplanLedgerFile)
	rows := strings.Split(strings.TrimSuffix(string(writeTestFile(t, path)), "\n"), "\n")
	if len(rows) != 2 {
		t.Fatal(rows)
	}
	for i, row := range rows {
		var r GoalplanLedgerEntry
		writeTestRequire(t, json.Unmarshal([]byte(row), &r))
		if r.Ts != fmt.Sprint(i) || r.Detail != "<&>\u2028\\u2028\n" {
			t.Fatal(r)
		}
	}
	if strings.Contains(rows[0], `\u003c`) || strings.Contains(rows[0], `\u2028"`) {
		t.Fatal("JSON escaping differs")
	}
	p := writeTestPlan()
	writeTestRequire(t, WriteGoalplan(cwd, p))
	result, e := WithGoalplanWriteLock(cwd, p.Slug, func(plan *Goalplan) (bool, error) {
		plan.Objective = "updated"
		if e := WriteGoalplan(cwd, plan); e != nil {
			return false, e
		}
		return true, AppendGoalplanLedger(cwd, plan.Slug, GoalplanLedgerEntry{Ts: "t", Slug: plan.Slug, Event: EventTaskDone, Detail: "under lock"})
	}, nil)
	writeTestRequire(t, e)
	if result.Kind != "ok" || result.Value == nil || !*result.Value {
		t.Fatalf("nested lock %+v", result)
	}
	empty := t.TempDir()
	if AppendGoalplanLedger(empty, "safe", GoalplanLedgerEntry{Slug: "other"}) == nil {
		t.Fatal("mismatch accepted")
	}
	if _, e := os.Stat(filepath.Join(empty, crwdir.DirName)); !os.IsNotExist(e) {
		t.Fatal("mismatch created state")
	}
}

func TestWriteLedgerRepairsUnterminatedRecords(t *testing.T) {
	for _, prior := range []string{`{"ts":"t1","slug":"led","event":"created","detail":"a"}`, `{"partial":`} {
		t.Run(prior, func(t *testing.T) {
			cwd := t.TempDir()
			dir, e := GoalplanDir(cwd, "led")
			writeTestRequire(t, e)
			writeTestRequire(t, os.MkdirAll(dir, 0700))
			path := filepath.Join(dir, GoalplanLedgerFile)
			writeTestRequire(t, os.WriteFile(path, []byte(prior), 0600))
			entry := GoalplanLedgerEntry{Ts: "t2", Slug: "led", Event: EventTaskDone, Detail: "b"}
			writeTestRequire(t, AppendGoalplanLedger(cwd, "led", entry))
			raw := string(writeTestFile(t, path))
			want := prior + "\n" + compact(t, entry) + "\n"
			if raw != want {
				t.Fatalf("lost row: %q, want%q", raw, want)
			}
		})
	}
}

func TestWriteLedgerConcurrentRows(t *testing.T) {
	cwd := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			errs <- AppendGoalplanLedger(cwd, "concurrent", GoalplanLedgerEntry{Ts: fmt.Sprint(i), Slug: "concurrent", Event: EventCreated, Detail: strings.Repeat("x", 2048)})
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		writeTestRequire(t, e)
	}
	raw := writeTestFile(t, filepath.Join(cwd, crwdir.DirName, GoalplansSubdir, "concurrent", GoalplanLedgerFile))
	// Without a caller-owned plan lock, a conservative recovery LF can add a
	// blank separator when an appender observes another write in flight. Every
	// nonempty row must still be a complete, distinct record.
	rows := strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\n' })
	if len(rows) != 16 {
		t.Fatalf("rows%d", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		var e GoalplanLedgerEntry
		writeTestRequire(t, json.Unmarshal([]byte(row), &e))
		seen[e.Ts] = true
	}
	if len(seen) != 16 {
		t.Fatalf("lost rows%d", len(seen))
	}
}

func TestWriteRejectsLinkedDirectories(t *testing.T) {
	for _, level := range []string{crwdir.DirName, filepath.Join(crwdir.DirName, GoalplansSubdir), filepath.Join(crwdir.DirName, GoalplansSubdir, "hello-world")} {
		for _, dangling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dangling=%v", level, dangling), func(t *testing.T) {
				cwd, outside := t.TempDir(), t.TempDir()
				target := outside
				if dangling {
					target = filepath.Join(outside, "missing")
				}
				link := filepath.Join(cwd, level)
				writeTestRequire(t, os.MkdirAll(filepath.Dir(link), 0700))
				writeTestRequire(t, os.Symlink(target, link))
				err := WriteGoalplan(cwd, writeTestPlan())
				if err == nil {
					t.Fatal("linked directory accepted by writer")
				}
				if dangling && os.IsNotExist(err) {
					t.Fatal("existing dangling writer link was treated as an absent path")
				}
				err = AppendGoalplanLedger(cwd, "hello-world", GoalplanLedgerEntry{Slug: "hello-world", Event: EventCreated})
				if err == nil {
					t.Fatal("linked directory accepted by append")
				}
				if dangling && os.IsNotExist(err) {
					t.Fatal("existing dangling append link was treated as an absent path")
				}
				entries, e := os.ReadDir(outside)
				writeTestRequire(t, e)
				if len(entries) != 0 {
					t.Fatalf("outside written %v", entries)
				}
			})
		}
	}
}

func TestWriteRejectsPlansRootSwappedAfterCheck(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	checked, e := GoalplanDir(cwd, "hello-world")
	writeTestRequire(t, e)
	plans := filepath.Dir(checked)
	writeTestRequire(t, os.MkdirAll(checked, 0700))
	writeTestRequire(t, os.Rename(plans, plans+"-old"))
	writeTestRequire(t, os.Symlink(outside, plans))
	dir, _, e := writeOpenCheckedDir(cwd, checked)
	if dir != nil {
		dir.Close()
	}
	if e == nil {
		t.Fatal("after-check link was followed")
	}
	entries, e := os.ReadDir(outside)
	writeTestRequire(t, e)
	if len(entries) != 0 {
		t.Fatalf("outside directory created %v", entries)
	}
}

func TestWriteRefusesRelocatedDescriptor(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	checked, e := GoalplanDir(cwd, "hello-world")
	writeTestRequire(t, e)
	dir, real, e := writeOpenCheckedDir(cwd, checked)
	writeTestRequire(t, e)
	defer dir.Close()
	writeTestRequire(t, os.Rename(checked, filepath.Join(outside, "moved")))
	if writePublishAt(dir, real, []byte("{}")) == nil {
		t.Fatal("publication to relocated descriptor")
	}
	if writeAppendAt(dir, real, []byte("{}\n")) == nil {
		t.Fatal("append to relocated descriptor")
	}
	entries, e := os.ReadDir(filepath.Join(outside, "moved"))
	writeTestRequire(t, e)
	if len(entries) != 0 {
		t.Fatalf("outside written %v", entries)
	}
}

func TestWriteLeafLinks(t *testing.T) { // TS:494-514; rename replaces the plan link itself (:933).
	cwd, outside := t.TempDir(), t.TempDir()
	p := writeTestPlan()
	dir, e := GoalplanDir(cwd, p.Slug)
	writeTestRequire(t, e)
	writeTestRequire(t, os.MkdirAll(dir, 0700))
	target := filepath.Join(outside, "target")
	writeTestRequire(t, os.WriteFile(target, []byte("keep"), 0600))
	writeTestRequire(t, os.Symlink(target, filepath.Join(dir, GoalplanFile)))
	writeTestRequire(t, WriteGoalplan(cwd, p))
	if string(writeTestFile(t, target)) != "keep" {
		t.Fatal("outside plan leaf written")
	}
	writeTestRequire(t, os.Symlink(target, filepath.Join(dir, GoalplanLedgerFile)))
	if AppendGoalplanLedger(cwd, p.Slug, GoalplanLedgerEntry{Slug: p.Slug}) == nil {
		t.Fatal("ledger leaf link accepted")
	}
	if string(writeTestFile(t, target)) != "keep" {
		t.Fatal("outside ledger leaf written")
	}
}
