// Package metric is the objective metric ledger of the PABCD loop: the Go form of CXC v0.2.40 pabcd-state/src/metrics.ts (commit
// 3c1459ac) under the CRW names of contract/schema/cxc/name-substitution.json. The ledger is .crw/metrics.jsonl, one JSON row per
// reading; the optional per-session kind file is .crw/objective-kind/<session>.json.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md, "Found by the objective metrics port"), except
// two data-loss defects that are fixed: an append that finds the ledger not ending in a newline, or can not read it to see, starts its
// row on a new line (the oracle fuses the two into one line the reader drops), and WriteObjectiveKind does not replace a kind file
// that holds another session's id, holds U+FFFD, is not a regular file or can not be read (a file read without a string sessionId is
// replaced). The appender locks the ledger and the kind writer the kind directory, which excludes other writers of this package only.
//
// Not literal: a string with a lone surrogate (an escape such as \ud800 in a ledger row) can not exist in Go and reads as U+FFFD, and
// the oracle's RangeError for a plateau window of more than about 1.2e5 rows is not reproduced. Rows are spelled as JSON.stringify
// spells them: HTML is not escaped, U+2028 and U+2029 are written literally, NaN and the infinities are null.
package metric

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ObjectiveMetricSource records how a number was obtained; the reader drops a row with any other value.
type ObjectiveMetricSource string

// The two sources a row can carry.
const (
	OperatorEntered ObjectiveMetricSource = "operator-entered"
	EvaluateSh      ObjectiveMetricSource = "evaluate.sh"
)

// ObjectiveKind is whether a session's goal is met by a spec or by maximizing a metric.
type ObjectiveKind string

// The two kinds a kind file can hold.
const (
	Satisfy  ObjectiveKind = "satisfy"
	Maximize ObjectiveKind = "maximize"
)

// MetricsFile, ObjectiveKindDir and DefaultWorkPhaseID are METRICS_FILE, OBJECTIVE_KIND_DIR and DEFAULT_WORK_PHASE_ID.
const (
	MetricsFile        = "metrics.jsonl"
	ObjectiveKindDir   = "objective-kind"
	DefaultWorkPhaseID = "default"
)

// timestampLayout is Date.prototype.toISOString.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// Record is one ledger row (ObjectiveMetricRecord). Baseline is the first reading's baseline and Best the largest reading seen, both
// carried from row to row.
type Record struct {
	TS          string
	SessionID   string
	WorkPhaseID string
	MetricName  string
	Value       float64
	Baseline    float64
	Best        float64
	Source      ObjectiveMetricSource
}

// RecordInput is a reading to record (RecordObjectiveMetricInput). A nil WorkPhaseID is the default work phase and an empty one is
// kept as written. A nil Now reads the clock.
type RecordInput struct {
	SessionID   string
	MetricName  string
	Value       float64
	Source      ObjectiveMetricSource
	WorkPhaseID *string
	Now         func() string
}

// TextInput is the input of RecordMetricsFromText.
type TextInput struct {
	SessionID   string
	Text        string
	Source      ObjectiveMetricSource
	WorkPhaseID *string
	Now         func() string
}

// PlateauCheck is the answer of CheckObjectivePlateau: whether the latest metric stopped improving, its name (nil when the session has
// no rows) and the window judged; Values is never nil.
type PlateauCheck struct {
	Flat       bool
	MetricName *string
	Values     []float64
}

// PlateauOptions are the options of CheckObjectivePlateau. Each zero value is the oracle's default (2 rows, no noise floor): a smaller
// MinRecords and a negative NoiseFloor give the same, and NaN and the infinities behave as they do in the oracle.
type PlateauOptions struct {
	MinRecords float64
	NoiseFloor float64
}

func metricsPath(cwd string) string { return filepath.Join(cwd, crwdir.DirName, MetricsFile) }

func objectiveKindDir(cwd string) string { return filepath.Join(cwd, crwdir.DirName, ObjectiveKindDir) }

func objectiveKindPath(cwd, sessionID string) string {
	return filepath.Join(objectiveKindDir(cwd), state.SanitizeKey(sessionID)+".json")
}

func isObjectiveMetricSource(s string) bool {
	return s == string(OperatorEntered) || s == string(EvaluateSh)
}

func isObjectiveKind(s string) bool { return s == string(Satisfy) || s == string(Maximize) }

// normalizeMetricName is the file-name key with every run of dots cut to one and none left at the ends; a name with nothing left is
// "metric". ParseMetricLine and RecordObjectiveMetric both apply it and it is not idempotent: ".-a" is "-a" and then "a".
func normalizeMetricName(name string) string {
	if s := strings.Join(strings.FieldsFunc(state.SanitizeKey(name), func(r rune) bool { return r == '.' }), "."); s != "" {
		return s
	}
	return "metric"
}

// ledgerDoc is JSON.parse of a document that must be an object: numbers stay json.Number (so 1e999 parses), anything after the first
// value is an error, and nil stands for every other shape.
func ledgerDoc(doc string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// ledgerRow is one ledger line as the reader accepts it: an object whose ts, sessionId, workPhaseId, metricName and source are strings
// (the source one of the two known) and whose value, baseline and best are finite numbers. A number that overflows is infinite and
// refuses the row, one that underflows is 0. Keys match exactly, the last of a repeated key wins, other keys are ignored.
func ledgerRow(line string) (Record, bool) {
	m := ledgerDoc(line)
	var strs [5]string
	for i, key := range [...]string{"ts", "sessionId", "workPhaseId", "metricName", "source"} {
		s, ok := m[key].(string)
		if strs[i] = s; !ok {
			return Record{}, false
		}
	}
	var nums [3]float64
	for i, key := range [...]string{"value", "baseline", "best"} {
		n, ok := m[key].(json.Number)
		f, err := n.Float64()
		if nums[i] = f; !ok || err != nil {
			return Record{}, false
		}
	}
	if !isObjectiveMetricSource(strs[4]) {
		return Record{}, false
	}
	return Record{strs[0], strs[1], strs[2], strs[3], nums[0], nums[1], nums[2], ObjectiveMetricSource(strs[4])}, true
}

// readAll is every row the ledger holds that the reader accepts, in file order. A ledger that can not be read holds none.
func readAll(cwd string) []Record {
	rows := []Record{}
	raw, err := os.ReadFile(metricsPath(cwd))
	if err != nil {
		return rows
	}
	for _, line := range text.SplitLines(ledgerUTF8(raw)) {
		if r, ok := ledgerRow(line); ok {
			rows = append(rows, r)
		}
	}
	return rows
}

// ReadObjectiveMetrics is the accepted rows of a session, or of every session when sessionID is empty (the oracle's falsy test).
// The result is never nil.
func ReadObjectiveMetrics(cwd, sessionID string) []Record {
	all := readAll(cwd)
	if sessionID == "" {
		return all
	}
	return slices.DeleteFunc(all, func(r Record) bool { return r.SessionID != sessionID })
}

// RecordObjectiveMetric appends a reading to the ledger and returns its row. Baseline is the first accepted row's baseline for the
// session and metric, else the value; Best is the larger of the last accepted row's best and the value. The value is not checked: NaN
// and the infinities are written as null, the reader then drops the row, and the caller is told it was recorded.
func RecordObjectiveMetric(cwd string, in RecordInput) (Record, error) {
	name := normalizeMetricName(in.MetricName)
	baseline, best := in.Value, in.Value
	if prior := slices.DeleteFunc(ReadObjectiveMetrics(cwd, in.SessionID), func(r Record) bool { return r.MetricName != name }); len(prior) > 0 {
		baseline, best = prior[0].Baseline, prior[len(prior)-1].Best
	}
	ts := time.Now().UTC().Format(timestampLayout)
	if in.Now != nil {
		ts = in.Now()
	}
	rec := Record{ts, in.SessionID, DefaultWorkPhaseID, name, in.Value, baseline, math.Max(best, in.Value), in.Source}
	if in.WorkPhaseID != nil {
		rec.WorkPhaseID = *in.WorkPhaseID
	}
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return Record{}, err
	}
	return rec, appendRow(metricsPath(cwd), Encode(rec))
}

// appendRow adds the row as one line, after a newline when the ledger does not end in one (the oracle appends the row to the
// unterminated tail, and neither is read again). A ledger that can not be read may end that way too, so it gets the newline as well
// (a blank line is skipped by the reader). The check and the write run under a lock on the ledger, so a writer that fails after part
// of a row is followed by one that sees the fragment.
func appendRow(path, row string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { // dropped by the close
		return errors.Join(err, f.Close())
	}
	line := row + "\n"
	if raw, err := os.ReadFile(path); err != nil || len(raw) > 0 && raw[len(raw)-1] != '\n' {
		line = "\n" + line
	}
	_, err = f.WriteString(line)
	return errors.Join(err, f.Close())
}

// Encode is JSON.stringify of the row, without a newline: keys in the oracle's order, numbers as ES6 spells them (-0 is 0, NaN and the
// infinities null), strings with U+2028 and HTML left as they are.
func Encode(r Record) string {
	return fmt.Sprintf("{\"ts\":%s,\"sessionId\":%s,\"workPhaseId\":%s,\"metricName\":%s,\"value\":%s,\"baseline\":%s,\"best\":%s,\"source\":%s}",
		rowQuote(r.TS), rowQuote(r.SessionID), rowQuote(r.WorkPhaseID), rowQuote(r.MetricName), rowNumber(r.Value), rowNumber(r.Baseline),
		rowNumber(r.Best), rowQuote(string(r.Source)))
}

// rowQuote is JSON.stringify of a string; an invalid UTF-8 byte is written as U+FFFD.
func rowQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// rowNumber is JSON.stringify of a number; encoding/json spells a finite one as ES6 does, except that -0 is 0.
func rowNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// WriteObjectiveKind records a session's explicit kind (writeObjectiveKind): a temp file beside the kind file, renamed over it, holding
// JSON.stringify of {sessionId, kind, updatedAt} with two-space indents and no final newline. The file is named by SanitizeKey of the
// session id, so two ids can share one. The write is refused, and the file kept, when the file holds another session's id, holds an id
// with U+FFFD (not told from a lone surrogate), can not be read for any reason but its absence, or is not a regular file (the data-loss
// fix); a file read without a string sessionId is replaced. A lock on the kind directory, held from the read to the rename, excludes
// other writers.
func WriteObjectiveKind(cwd, sessionID string, kind ObjectiveKind) error {
	return writeObjectiveKind(cwd, sessionID, kind, time.Now(), crwdir.Rename)
}

func writeObjectiveKind(cwd, sessionID string, kind ObjectiveKind, now time.Time, rename func(tmp, finalPath string) error) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	if err := os.MkdirAll(objectiveKindDir(cwd), 0o777); err != nil {
		return err
	}
	dir, err := os.Open(objectiveKindDir(cwd))
	if err != nil {
		return err
	}
	defer dir.Close() // drops the lock
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	finalPath := objectiveKindPath(cwd, sessionID)
	if info, err := os.Stat(finalPath); err == nil && !info.Mode().IsRegular() { // a FIFO would block the read below
		return fmt.Errorf("%s is not a regular file", finalPath)
	}
	raw, err := os.ReadFile(finalPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if owner, named := ledgerDoc(string(raw))["sessionId"].(string); named && (owner != sessionID || strings.ContainsRune(owner, utf8.RuneError)) {
		return fmt.Errorf("%s holds the objective kind of session %q, not %q", finalPath, owner, sessionID)
	}
	body := fmt.Sprintf("{\n  \"sessionId\": %s,\n  \"kind\": %s,\n  \"updatedAt\": %s\n}", rowQuote(sessionID), rowQuote(string(kind)), rowQuote(now.UTC().Format(timestampLayout)))
	// The oracle's temp name, so a long id fails or fits as it does there; the lock keeps two writers from sharing it. It is created
	// exclusively (the oracle's write follows a symlink at that name and truncates its target), so an entry already there is refused.
	tmp := fmt.Sprintf("%s.%d.%d.tmp", finalPath, os.Getpid(), now.UnixMilli())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // best effort; gone after a rename
	_, err = f.WriteString(body)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return rename(tmp, finalPath)
}

// ReadExplicitObjectiveKind is the kind a session's file names, or false when the file is missing, is not one JSON document, is not an
// object, or its kind is not one of the two. The sessionId inside the file is not looked at.
func ReadExplicitObjectiveKind(cwd, sessionID string) (ObjectiveKind, bool) {
	raw, err := os.ReadFile(objectiveKindPath(cwd, sessionID))
	if err != nil {
		return "", false
	}
	kind, _ := ledgerDoc(string(raw))["kind"].(string)
	return ObjectiveKind(kind), isObjectiveKind(kind)
}

// ReadObjectiveKind is the explicit kind when there is one, else maximize when the session has any row (the ledger of an empty session
// id is every session's), else satisfy.
func ReadObjectiveKind(cwd, sessionID string) ObjectiveKind {
	if kind, ok := ReadExplicitObjectiveKind(cwd, sessionID); ok {
		return kind
	}
	if len(ReadObjectiveMetrics(cwd, sessionID)) > 0 {
		return Maximize
	}
	return Satisfy
}

// ParseMetricLine reads "METRIC <name>=<number>" (parseMetricLine): the word in either case, JavaScript whitespace around the parts, a
// name of ASCII letters, digits, dot, underscore and dash, and a decimal number that is finite. The name is normalised. The pattern is
// the oracle's with \s spelled out and METRIC and e folded by hand, since RE2's (?i) also folds U+017F and U+212A.
func ParseMetricLine(line string) (metricName string, value float64, ok bool) {
	const space = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	pattern := `^` + space + `*[Mm][Ee][Tt][Rr][Ii][Cc]` + space + `+([A-Za-z0-9._-]+)` + space + `*=` + space +
		`*(-?(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)` + space + `*$`
	match := regexp.MustCompile(pattern).FindStringSubmatch(line)
	if match == nil {
		return "", 0, false
	}
	f, err := strconv.ParseFloat(match[2], 64)
	if err != nil { // an overflow is infinite, which Number.isFinite refuses
		return "", 0, false
	}
	return normalizeMetricName(match[1]), f, true
}

// RecordMetricsFromText records every METRIC line of the text, split on LF only (a CR stays in its line and is whitespace to the
// parser), one RecordObjectiveMetric each. A failure returns the rows recorded before it with the error.
func RecordMetricsFromText(cwd string, in TextInput) ([]Record, error) {
	records := []Record{}
	for _, line := range strings.Split(in.Text, "\n") {
		name, value, ok := ParseMetricLine(line)
		if !ok {
			continue
		}
		rec, err := RecordObjectiveMetric(cwd, RecordInput{in.SessionID, name, value, in.Source, in.WorkPhaseID, in.Now})
		if err != nil {
			return records, err
		}
		records = append(records, rec)
	}
	return records, nil
}

// CheckObjectivePlateau judges the latest row's metric within its work phase: the last MinRecords rows of it are flat when none beats
// the first by more than NoiseFloor, so a falling window is flat too. Fewer rows than MinRecords are never flat.
func CheckObjectivePlateau(cwd, sessionID string, opts PlateauOptions) PlateauCheck {
	minRecords, noiseFloor := math.Max(2, math.Floor(opts.MinRecords)), math.Max(0, opts.NoiseFloor)
	records := ReadObjectiveMetrics(cwd, sessionID)
	if len(records) == 0 {
		return PlateauCheck{Values: []float64{}}
	}
	latest := records[len(records)-1]
	same := slices.DeleteFunc(records, func(r Record) bool { return r.MetricName != latest.MetricName || r.WorkPhaseID != latest.WorkPhaseID })
	start := 0
	if minRecords < float64(len(same)) { // false for NaN and +Inf too: the whole history
		start = len(same) - int(minRecords)
	}
	values := make([]float64, 0, len(same)-start)
	for _, r := range same[start:] {
		values = append(values, r.Value)
	}
	check := PlateauCheck{MetricName: &latest.MetricName, Values: values}
	if !(float64(len(values)) < minRecords) { // not >=, which is false for NaN: the oracle's length gate lets NaN through
		check.Flat = slices.Max(values) <= values[0]+noiseFloor
	}
	return check
}

// ledgerUTF8 is Node's utf8 decoding of a file: each maximal invalid subpart becomes one U+FFFD (the WHATWG decoder; utf8.DecodeRune
// alone replaces every byte). host, source and projectcfg carry the same private copy.
func ledgerUTF8(b []byte) string {
	runes := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			n = ledgerSubpart(b)
		}
		runes, b = append(runes, r), b[n:]
	}
	return string(runes)
}

// ledgerSubpart is the length of the lead byte of b and the continuation bytes that could still have completed it (Unicode table 3-7).
func ledgerSubpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		lo, need = 0xA0, 2
	case lead == 0xED:
		hi, need = 0x9F, 2
	case lead >= 0xE1 && lead <= 0xEF:
		need = 2
	case lead == 0xF0:
		lo, need = 0x90, 3
	case lead == 0xF4:
		hi, need = 0x8F, 3
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(b) && b[n] >= lo && b[n] <= hi; n++ {
		lo, hi = 0x80, 0xBF
	}
	return n
}
