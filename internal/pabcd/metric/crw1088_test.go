package metric

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// CRW-1088 end condition 4: a kind file that names another session is not this session's explicit kind, so it does not arm
// maximize; a legacy file without a sessionId keeps its kind.
func TestCRW1088AKindFileOfAnotherSessionIsNotExplicit(t *testing.T) {
	cwd := t.TempDir()
	metricsMust(t, os.MkdirAll(objectiveKindDir(cwd), 0o777))
	metricsMust(t, os.WriteFile(objectiveKindPath(cwd, "mine"), []byte(`{"sessionId":"other","kind":"maximize"}`), 0o666))
	if kind, ok := ReadExplicitObjectiveKind(cwd, "mine"); ok {
		t.Errorf("another session's kind file read as explicit %q", kind)
	}
	if got := ReadObjectiveKind(cwd, "mine"); got != Satisfy {
		t.Errorf("another session's kind file armed %q", got)
	}
	metricsMust(t, os.WriteFile(filepath.Join(objectiveKindDir(cwd), "legacy.json"), []byte(`{"kind":"maximize"}`), 0o666))
	if kind, ok := ReadExplicitObjectiveKind(cwd, "legacy"); !ok || kind != Maximize {
		t.Errorf("a legacy kind file without sessionId: %q %v", kind, ok)
	}
	metricsMust(t, WriteObjectiveKind(cwd, "own", Maximize))
	if kind, ok := ReadExplicitObjectiveKind(cwd, "own"); !ok || kind != Maximize {
		t.Errorf("the session's own kind file: %q %v", kind, ok)
	}
}

// JudgeNewRows compares each new row only with the earlier row of its own metric and work phase.
func TestCRW1088JudgeNewRows(t *testing.T) {
	row := func(wp, name string, v float64) Record { return Record{WorkPhaseID: wp, MetricName: name, Value: v} }
	cases := []struct {
		name string
		rows []Record
		from int
		want Judgment
	}{
		{"no new row", []Record{row("w", "a", 1)}, 1, JudgmentUnknown},
		{"one singleton", []Record{row("w", "a", 1)}, 0, JudgmentUnknown},
		{"alternating singletons", []Record{row("w", "a", 1), row("w", "b", 1), row("w", "c", 1)}, 1, JudgmentUnknown},
		{"rising against an old row", []Record{row("w", "a", 1), row("w", "b", 5), row("w", "a", 2)}, 2, JudgmentImproving},
		{"two new rising rows", []Record{row("w", "a", 1), row("w", "a", 2)}, 0, JudgmentImproving},
		{"equal", []Record{row("w", "a", 1), row("w", "a", 1)}, 1, JudgmentNonImproving},
		{"falling", []Record{row("w", "a", 2), row("w", "a", 1)}, 1, JudgmentNonImproving},
		{"another work phase is no predecessor", []Record{row("old", "a", 1), row("new", "a", 2)}, 1, JudgmentUnknown},
		{"another metric is no predecessor", []Record{row("w", "a", 1), row("w", "b", 2)}, 1, JudgmentUnknown},
		{"from past the end", []Record{row("w", "a", 1), row("w", "a", 2)}, 5, JudgmentUnknown},
	}
	for _, c := range cases {
		if got := JudgeNewRows(c.rows, c.from, 0); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// PlateauOf is CheckObjectivePlateau on rows already read, and leaves them as they were.
func TestCRW1088PlateauOfMatchesTheLedgerJudgmentAndKeepsItsInput(t *testing.T) {
	cwd := t.TempDir()
	for _, v := range []float64{1, 3, 3} {
		metricsRecord(t, cwd, "s", "score", v, nil)
	}
	metricsRecord(t, cwd, "s", "other", 1, nil)
	metricsRecord(t, cwd, "s", "score", 2, nil)
	rows := ReadObjectiveMetrics(cwd, "s")
	before := append([]Record(nil), rows...)
	got, want := PlateauOf(rows, PlateauOptions{}), CheckObjectivePlateau(cwd, "s", PlateauOptions{})
	if !reflect.DeepEqual(got, want) || !got.Flat {
		t.Errorf("PlateauOf %+v, CheckObjectivePlateau %+v", got, want)
	}
	if !reflect.DeepEqual(rows, before) {
		t.Errorf("PlateauOf changed its input: %+v", rows)
	}
	if InferObjectiveKind(cwd, "s", rows) != Maximize || InferObjectiveKind(cwd, "s", nil) != Satisfy {
		t.Errorf("InferObjectiveKind does not follow the rows it is given")
	}
}
