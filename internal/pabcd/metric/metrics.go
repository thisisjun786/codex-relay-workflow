// Package metric is the objective metric ledger of the PABCD loop (CXC v0.2.40 pabcd-state/src/metrics.ts). This first commit is the
// red step: the API with empty bodies, so that the tests of the next commit fail by assertion.
package metric

import "time"

type ObjectiveMetricSource string

const (
	OperatorEntered ObjectiveMetricSource = "operator-entered"
	EvaluateSh      ObjectiveMetricSource = "evaluate.sh"
)

type ObjectiveKind string

const (
	Satisfy  ObjectiveKind = "satisfy"
	Maximize ObjectiveKind = "maximize"
)

const (
	MetricsFile        = "metrics.jsonl"
	ObjectiveKindDir   = "objective-kind"
	DefaultWorkPhaseID = "default"
)

type Record struct {
	TS, SessionID, WorkPhaseID, MetricName string
	Value, Baseline, Best                  float64
	Source                                 ObjectiveMetricSource
}

type RecordInput struct {
	SessionID, MetricName string
	Value                 float64
	Source                ObjectiveMetricSource
	WorkPhaseID           *string
	Now                   func() string
}

type TextInput struct {
	SessionID, Text string
	Source          ObjectiveMetricSource
	WorkPhaseID     *string
	Now             func() string
}

type PlateauCheck struct {
	Flat       bool
	MetricName *string
	Values     []float64
}

type PlateauOptions struct{ MinRecords, NoiseFloor float64 }

func objectiveKindDir(cwd string) string { return "" }

func objectiveKindPath(cwd, sessionID string) string { return "" }

func rowQuote(s string) string { return "" }

func ReadObjectiveMetrics(cwd, sessionID string) []Record { return nil }

func RecordObjectiveMetric(cwd string, in RecordInput) (Record, error) { return Record{}, nil }

func RecordMetricsFromText(cwd string, in TextInput) ([]Record, error) { return nil, nil }

func Encode(r Record) string { return "" }

func WriteObjectiveKind(cwd, sessionID string, kind ObjectiveKind) error { return nil }

func writeObjectiveKind(cwd, sessionID string, kind ObjectiveKind, now time.Time, rename func(tmp, finalPath string) error) error {
	return nil
}

func ReadExplicitObjectiveKind(cwd, sessionID string) (ObjectiveKind, bool) { return "", false }

func ReadObjectiveKind(cwd, sessionID string) ObjectiveKind { return "" }

func ParseMetricLine(line string) (metricName string, value float64, ok bool) { return "", 0, false }

func CheckObjectivePlateau(cwd, sessionID string, opts PlateauOptions) PlateauCheck {
	return PlateauCheck{}
}
