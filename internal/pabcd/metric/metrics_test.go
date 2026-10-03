package metric

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// The first five tests are the library tests of metrics.test.ts at CXC v0.2.40 (its sixth drives the metric CLI); then come the cases
// the Go port adds, and the replay of the answers the oracle gave (testdata/record-metrics-oracle.mjs; no Node runs here).

func metricsRecord(t *testing.T, cwd, session, name string, value float64, workPhase *string) Record {
	t.Helper()
	rec, err := RecordObjectiveMetric(cwd, RecordInput{SessionID: session, MetricName: name, Value: value, Source: OperatorEntered, WorkPhaseID: workPhase})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func metricsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// metricsUnreadable makes path unreadable and skips the test where that is not possible (running as root).
func metricsUnreadable(t *testing.T, path string) {
	t.Helper()
	metricsMust(t, os.Chmod(path, 0o200))
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("the file is readable despite mode 0200 (running as root)")
	}
}

func metricsWrite(t *testing.T, cwd, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(cwd, crwdir.DirName, rel)
	metricsMust(t, os.MkdirAll(filepath.Dir(path), 0o777))
	metricsMust(t, os.WriteFile(path, data, 0o666))
}

func TestRecordObjectiveMetricAppendsRowsWithBaselineAndBest(t *testing.T) { // metrics.test.ts:23
	cwd := t.TempDir()
	in := RecordInput{SessionID: "s1", MetricName: "score", Source: OperatorEntered}
	in.Value, in.Now = 10, func() string { return "2026-07-01T00:00:00.000Z" }
	first, err := RecordObjectiveMetric(cwd, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Value, in.Now = 12, func() string { return "2026-07-01T00:01:00.000Z" }
	second, err := RecordObjectiveMetric(cwd, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Baseline != 10 || first.Best != 10 || second.Baseline != 10 || second.Best != 12 {
		t.Errorf("rows %+v then %+v", first, second)
	}
	if got := len(ReadObjectiveMetrics(cwd, "s1")); got != 2 {
		t.Errorf("%d rows read, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName, "metrics.jsonl")); err != nil {
		t.Error(err)
	}
}

func TestObjectiveKindDefaultsToSatisfyInfersMaximizeAndExplicitWins(t *testing.T) { // metrics.test.ts:51
	cwd := t.TempDir()
	if got := ReadObjectiveKind(cwd, "s2"); got != Satisfy {
		t.Fatalf("default %q", got)
	}
	metricsMust(t, os.WriteFile(filepath.Join(cwd, "evaluate.sh"), []byte("#!/bin/sh\n"), 0o666))
	if got := ReadObjectiveKind(cwd, "s2"); got != Satisfy {
		t.Errorf("a stale cwd-level harness flipped the session to %q", got)
	}
	metricsMust(t, WriteObjectiveKind(cwd, "s2", Satisfy))
	if kind, ok := ReadExplicitObjectiveKind(cwd, "s2"); !ok || kind != Satisfy || ReadObjectiveKind(cwd, "s2") != Satisfy {
		t.Errorf("explicit %q %v", kind, ok)
	}
	metricsRecord(t, cwd, "s3", "score", 1, nil)
	if got := ReadObjectiveKind(cwd, "s3"); got != Maximize {
		t.Errorf("inferred %q", got)
	}
}

func TestParseMetricLineAndIngestRecordOnlyMetricLines(t *testing.T) { // metrics.test.ts:68
	if name, value, ok := ParseMetricLine("METRIC score=12.5"); !ok || name != "score" || value != 12.5 {
		t.Errorf("parsed %q %v %v", name, value, ok)
	}
	if _, _, ok := ParseMetricLine("not a metric"); ok {
		t.Error("parsed a line that is not a metric")
	}
	cwd := t.TempDir()
	records, err := RecordMetricsFromText(cwd, TextInput{SessionID: "s4", Text: "hello\nMETRIC score=2\nMETRIC win-rate=0.75\n", Source: EvaluateSh})
	if err != nil || len(records) != 2 {
		t.Fatalf("%d records, %v", len(records), err)
	}
	var names []string
	for _, r := range ReadObjectiveMetrics(cwd, "s4") {
		names = append(names, r.MetricName)
	}
	if !reflect.DeepEqual(names, []string{"score", "win-rate"}) {
		t.Errorf("names %v", names)
	}
}

func TestCheckObjectivePlateauFlatOrFallingWindowArmsAndImprovingDoesNot(t *testing.T) { // metrics.test.ts:85
	cwd := t.TempDir()
	for _, v := range []float64{10, 10} {
		metricsRecord(t, cwd, "flat", "score", v, nil)
	}
	for _, v := range []float64{10, 11} {
		metricsRecord(t, cwd, "up", "score", v, nil)
	}
	if !CheckObjectivePlateau(cwd, "flat", PlateauOptions{}).Flat || CheckObjectivePlateau(cwd, "up", PlateauOptions{}).Flat {
		t.Error("the flat window did not arm or the improving window did")
	}
}

func TestCheckObjectivePlateauIgnoresRecordsFromEarlierWorkPhases(t *testing.T) { // metrics.test.ts:100
	cwd := t.TempDir()
	one, two := "phase-1", "phase-2"
	metricsRecord(t, cwd, "phased", "score", 10, &one)
	metricsRecord(t, cwd, "phased", "score", 10, &two)
	name := "score"
	want := PlateauCheck{Flat: false, MetricName: &name, Values: []float64{10}}
	if got := CheckObjectivePlateau(cwd, "phased", PlateauOptions{}); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v, want %+v", got, want)
	}
}

func TestDefaultClockWritesIsoTimestamps(t *testing.T) {
	cwd := t.TempDir()
	iso := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	rec := metricsRecord(t, cwd, "s", "m", 1, nil)
	metricsMust(t, WriteObjectiveKind(cwd, "s", Maximize))
	raw, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, ObjectiveKindDir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	var kind struct{ UpdatedAt string }
	if json.Unmarshal(raw, &kind) != nil || !iso.MatchString(rec.TS) || !iso.MatchString(kind.UpdatedAt) {
		t.Errorf("row ts %q, kind file %s", rec.TS, raw)
	}
}

func TestRecordStartsANewLineAfterALedgerItCanNotReadToCheck(t *testing.T) {
	cwd, row := t.TempDir(), "{\"ts\":\"t\",\"sessionId\":\"s\",\"workPhaseId\":\"default\",\"metricName\":\"m\",\"value\":1,\"baseline\":1,\"best\":1,\"source\":\"evaluate.sh\"}"
	metricsWrite(t, cwd, MetricsFile, []byte(row)) // a valid final row without its newline
	metricsUnreadable(t, metricsPath(cwd))
	_, recordErr := RecordObjectiveMetric(cwd, RecordInput{SessionID: "s", MetricName: "m", Value: 2, Source: EvaluateSh})
	metricsMust(t, os.Chmod(metricsPath(cwd), 0o600))
	if rows := ReadObjectiveMetrics(cwd, "s"); recordErr != nil || len(rows) != 2 {
		t.Errorf("record error %v, %d rows read after the append, want 2", recordErr, len(rows))
	}
}

func TestWriteObjectiveKindRemovesItsTempFileWhenTheRenameFails(t *testing.T) {
	cwd := t.TempDir()
	metricsMust(t, WriteObjectiveKind(cwd, "s", Maximize))
	dir, boom := objectiveKindDir(cwd), errors.New("rename failed")
	before, _ := os.ReadFile(filepath.Join(dir, "s.json"))
	err := writeObjectiveKind(cwd, "s", Satisfy, time.Now(), func(string, string) error { return boom })
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if after, _ := os.ReadFile(filepath.Join(dir, "s.json")); !errors.Is(err, boom) || len(left) != 0 || string(after) != string(before) {
		t.Errorf("err %v, temp files %v, final file now %q", err, left, after)
	}
}

func TestWriteObjectiveKindDoesNotReplaceAFileItCanNotRead(t *testing.T) {
	cwd := t.TempDir()
	metricsMust(t, WriteObjectiveKind(cwd, "s", Maximize))
	path := objectiveKindPath(cwd, "s")
	before, _ := os.ReadFile(path)
	metricsUnreadable(t, path)
	refused := WriteObjectiveKind(cwd, "s", Satisfy)
	metricsMust(t, os.Chmod(path, 0o600))
	if after, _ := os.ReadFile(path); refused == nil || string(after) != string(before) {
		t.Errorf("write error %v, kind file now %q", refused, after)
	}
}

func TestWriteObjectiveKindLeavesANonRegularFileAloneInsteadOfBlockingOnIt(t *testing.T) {
	cwd := t.TempDir()
	metricsMust(t, os.MkdirAll(objectiveKindDir(cwd), 0o777))
	metricsMust(t, unix.Mkfifo(objectiveKindPath(cwd, "s"), 0o666))
	done := make(chan error, 1)
	go func() { done <- WriteObjectiveKind(cwd, "s", Maximize) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was replaced")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write blocked on the FIFO")
	}
}

func TestWriteObjectiveKindDoesNotFollowASymlinkAtItsTempName(t *testing.T) {
	cwd, victim, now := t.TempDir(), filepath.Join(t.TempDir(), "victim"), time.UnixMilli(1_790_000_000_000)
	metricsMust(t, os.WriteFile(victim, []byte("keep"), 0o666))
	metricsMust(t, os.MkdirAll(objectiveKindDir(cwd), 0o777))
	tmp := fmt.Sprintf("%s.%d.%d.tmp", objectiveKindPath(cwd, "s"), os.Getpid(), now.UnixMilli())
	metricsMust(t, os.Symlink(victim, tmp))
	err := writeObjectiveKind(cwd, "s", Maximize, now, crwdir.Rename)
	if got, _ := os.ReadFile(victim); err == nil || string(got) != "keep" {
		t.Errorf("write error %v, the file the link points at now holds %q", err, got)
	}
	if _, lerr := os.Lstat(tmp); lerr != nil {
		t.Errorf("the entry that was already there was removed: %v", lerr)
	}
}

func TestWriteObjectiveKindKeepsTheFileOfAnOwnerItCanNotTellApart(t *testing.T) {
	cwd := t.TempDir()
	// The file belongs to a session whose id holds a lone surrogate, which a Go string reads as U+FFFD.
	owned := "{\"sessionId\": \"a\\ud800b\", \"kind\": \"maximize\"}"
	metricsWrite(t, cwd, ObjectiveKindDir+"/a-b.json", []byte(owned))
	if err := WriteObjectiveKind(cwd, "a\ufffdb", Satisfy); err == nil {
		t.Error("a session replaced the file of an owner it can not tell apart from itself")
	}
	if raw, _ := os.ReadFile(objectiveKindPath(cwd, "a-b")); string(raw) != owned {
		t.Errorf("kind file now %q", raw)
	}
}

func TestWriteObjectiveKindOfAliasedSessionsAtOnceKeepsExactlyOneFile(t *testing.T) {
	for round := 0; round < 50; round++ { // without the lock about one round in ten lets two writers through
		cwd, start, winners := t.TempDir(), make(chan struct{}), make(chan string, 8)
		var wg sync.WaitGroup
		for _, id := range []string{"a/b", "a?b", "a b", "a:b", "a*b", "a|b", "a<b", "a>b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if WriteObjectiveKind(cwd, id, Maximize) == nil {
					winners <- id
				}
			}()
		}
		close(start)
		wg.Wait()
		close(winners)
		var won []string
		for id := range winners {
			won = append(won, id)
		}
		raw, _ := os.ReadFile(objectiveKindPath(cwd, "a-b"))
		if len(won) != 1 || !strings.Contains(string(raw), rowQuote(won[0])) {
			t.Fatalf("round %d: writes that succeeded: %q; kind file %s", round, won, raw)
		}
	}
}

// metricsOp is one step of a scenario in testdata/scenarios-metrics.json, metricsAnswer what the oracle answered. A scenario with
// Changed is an oracle case the port intentionally answers differently: its Go answers are written out there, with the reason.
type metricsOp struct {
	Op, Session, Name, Source, Now, Path, Text, B64, Line, Set string
	WorkPhase                                                  *string
	Value, MinRecords, NoiseFloor                              any
}

type metricsAnswer struct {
	Results []any
	Files   map[string]string
}

type metricsScenario struct {
	ID      string
	Capture bool
	Changed *struct {
		Reason string
		metricsAnswer
	}
	Ops []metricsOp
}

// metricsNumber reads a scenario number, which is a JSON number or one of the string tokens JSON has no number for.
func metricsNumber(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case string:
		return map[string]float64{"NaN": math.NaN(), "Infinity": math.Inf(1), "-Infinity": math.Inf(-1), "-0": math.Copysign(0, -1)}[v]
	}
	return 0
}

// metricsShown spells -0, NaN and the infinities as those tokens, as the recorder does.
func metricsShown(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0 && math.Signbit(f):
		return "-0"
	}
	return f
}

func metricsRows(rows []Record) map[string]any {
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"ts": r.TS, "sessionId": r.SessionID, "workPhaseId": r.WorkPhaseID, "metricName": r.MetricName,
			"value": metricsShown(r.Value), "baseline": metricsShown(r.Baseline), "best": metricsShown(r.Best), "source": r.Source})
	}
	return map[string]any{"rows": out}
}

// metricsStep runs one op against the port and answers as the recorder does: the row of a record, {"rows": ...}, {"error": true} for
// a call that failed, null for a file or directory op.
func metricsStep(t *testing.T, cwd string, op metricsOp) any {
	t.Helper()
	failed := map[string]any{"error": true}
	clock := func() string {
		if op.Now == "" {
			t.Fatalf("op %s has no now", op.Op)
		}
		return op.Now
	}
	source := OperatorEntered
	if op.Source != "" {
		source = ObjectiveMetricSource(op.Source)
	}
	switch op.Op {
	case "record":
		rec, err := RecordObjectiveMetric(cwd, RecordInput{op.Session, op.Name, metricsNumber(op.Value), source, op.WorkPhase, clock})
		if err != nil {
			return failed
		}
		return metricsRows([]Record{rec})["rows"].([]any)[0]
	case "ingest":
		if op.Source == "" {
			source = EvaluateSh
		}
		rows, err := RecordMetricsFromText(cwd, TextInput{op.Session, op.Text, source, op.WorkPhase, clock})
		if err != nil {
			return failed
		}
		return metricsRows(rows)
	case "read":
		return metricsRows(ReadObjectiveMetrics(cwd, op.Session))
	case "kind":
		if op.Set != "" && WriteObjectiveKind(cwd, op.Session, ObjectiveKind(op.Set)) != nil {
			return failed
		}
		var explicit any
		if kind, ok := ReadExplicitObjectiveKind(cwd, op.Session); ok {
			explicit = kind
		}
		return map[string]any{"kind": ReadObjectiveKind(cwd, op.Session), "explicit": explicit}
	case "plateau":
		check := CheckObjectivePlateau(cwd, op.Session, PlateauOptions{metricsNumber(op.MinRecords), metricsNumber(op.NoiseFloor)})
		var name any
		if check.MetricName != nil {
			name = *check.MetricName
		}
		values := []any{}
		for _, v := range check.Values {
			values = append(values, metricsShown(v))
		}
		return map[string]any{"flat": check.Flat, "metricName": name, "values": values}
	case "parse":
		name, value, ok := ParseMetricLine(op.Line)
		if !ok {
			return map[string]any{"parsed": nil}
		}
		return map[string]any{"parsed": map[string]any{"metricName": name, "value": metricsShown(value)}}
	case "file":
		data := []byte(op.Text)
		if op.B64 != "" {
			var err error
			if data, err = base64.StdEncoding.DecodeString(op.B64); err != nil {
				t.Fatal(err)
			}
		}
		metricsWrite(t, cwd, op.Path, data)
		return nil
	case "dir":
		metricsMust(t, os.MkdirAll(filepath.Join(cwd, crwdir.DirName, op.Path), 0o777))
		return nil
	}
	t.Fatalf("unknown op %q", op.Op)
	return nil
}

// metricsFiles is what the run left under the state directory but its ignore file, by relative path, with updatedAt a placeholder.
func metricsFiles(t *testing.T, cwd string) map[string]string {
	t.Helper()
	root, stamp, files := filepath.Join(cwd, crwdir.DirName), regexp.MustCompile(`("updatedAt": ")[^"]*"`), map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() == ".gitignore" {
			return err
		}
		raw, err := os.ReadFile(path)
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = stamp.ReplaceAllString(string(raw), "$1<TS>\"")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

func metricsJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func TestMetricsMatchTheRecordedOracle(t *testing.T) {
	var scenarios []metricsScenario
	var oracle map[string]metricsAnswer
	for name, into := range map[string]any{"scenarios-metrics.json": &scenarios, "oracle-metrics.json": &oracle} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(oracle) != len(scenarios) {
		t.Fatalf("%d scenarios, %d recorded answers", len(scenarios), len(oracle))
	}
	for _, sc := range scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			cwd, got := t.TempDir(), metricsAnswer{Results: []any{}}
			for _, op := range sc.Ops {
				got.Results = append(got.Results, metricsStep(t, cwd, op))
			}
			if sc.Capture {
				got.Files = metricsFiles(t, cwd)
			}
			want := oracle[sc.ID]
			if c := sc.Changed; c != nil {
				if c.Reason == "" || metricsJSON(c.metricsAnswer) == metricsJSON(want) {
					t.Fatal("an intentionally-changed case needs a reason and an answer that differs from the oracle's")
				}
				want = c.metricsAnswer
			}
			if len(got.Results) != len(want.Results) {
				t.Fatalf("%d answers, want %d", len(got.Results), len(want.Results))
			}
			for i := range want.Results {
				if g, w := metricsJSON(got.Results[i]), metricsJSON(want.Results[i]); g != w {
					t.Errorf("op %d (%s): %s, want %s", i, sc.Ops[i].Op, g, w)
				}
			}
			if len(got.Files)+len(want.Files) > 0 && !reflect.DeepEqual(got.Files, want.Files) {
				t.Errorf("files %q, want %q", got.Files, want.Files)
			}
		})
	}
}
