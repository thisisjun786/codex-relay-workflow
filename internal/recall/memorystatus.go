// Read-only memory pipeline status from CXC v0.2.40 recall/src/memory-status.ts.
package recall

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

type MemoryStatusState string

const (
	MemoryStatusOK          MemoryStatusState = "ok"
	MemoryStatusUnavailable MemoryStatusState = "unavailable"
	MemoryStatusUnsupported MemoryStatusState = "unsupported"
)

type MemoryJobCounts struct {
	Kind   string  `json:"kind"`
	Status string  `json:"status"`
	Count  float64 `json:"count"`
}

// CauseCounts is an insertion-ordered JSON object: stable count ties retain order.
type CauseCount struct {
	Cause string
	Count float64
}
type CauseCounts []CauseCount

func (c *CauseCounts) set(cause string, count float64) {
	for i := range *c {
		if (*c)[i].Cause == cause {
			(*c)[i].Count = count
			return
		}
	}
	*c = append(*c, CauseCount{cause, count})
}

func (c CauseCounts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, pair := range c {
		if i != 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(pair.Cause)
		if err != nil {
			return nil, err
		}
		count, err := json.Marshal(pair.Count)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(count)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (c *CauseCounts) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("memory causes must be an object")
	}
	counts := CauseCounts{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		var count float64
		if err := d.Decode(&count); err != nil {
			return err
		}
		counts.set(key.(string), count)
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	*c = counts
	return nil
}

type MemoryStatus struct {
	State                    MemoryStatusState `json:"state"`
	ObservationSource        string            `json:"observationSource"`
	EffectiveExtractionRoute string            `json:"effectiveExtractionRoute"`
	StartupGuardDecision     string            `json:"startupGuardDecision"`
	Detail                   string            `json:"detail"`
	StorePath                *string           `json:"storePath"`
	Jobs                     []MemoryJobCounts `json:"jobs"`
	Exhausted                float64           `json:"exhausted"`
	ExhaustedByCause         CauseCounts       `json:"exhaustedByCause"`
	LastSuccessAt            *float64          `json:"lastSuccessAt"`
	LastFinishedAt           *float64          `json:"lastFinishedAt"`
}

// JSON.stringify turns nonfinite timestamps into null; the in-memory NaN stays NaN.
func (s MemoryStatus) MarshalJSON() ([]byte, error) {
	type wire MemoryStatus
	w := wire(s)
	for _, at := range []**float64{&w.LastSuccessAt, &w.LastFinishedAt} {
		if *at != nil && (math.IsNaN(**at) || math.IsInf(**at, 0)) {
			*at = nil
		}
	}
	return json.Marshal(w)
}

func emptyMemoryStatus(state MemoryStatusState, path *string, detail string) MemoryStatus {
	return MemoryStatus{State: state, ObservationSource: "jobs-db", EffectiveExtractionRoute: "unknown",
		StartupGuardDecision: "unknown", Detail: detail, StorePath: path,
		Jobs: []MemoryJobCounts{}, ExhaustedByCause: CauseCounts{}}
}

// ClassifyMemoryError accepts the SQLite scalars the upstream String coercion sees.
func ClassifyMemoryError(raw any) string {
	s := ""
	if raw != nil {
		s = Lower(memoryStatusString(raw))
	}
	if s == "" {
		return "unknown"
	}
	has := func(word string) bool { return strings.Contains(s, word) }
	if has("context") && (has("window") || has("length") || has("exceed")) {
		return "context-window"
	}
	if has("rate limit") || has("quota") || has("429") || has("capacity") {
		return "capacity"
	}
	if has("incomplete") {
		return "incomplete-response"
	}
	if has("stream") && has("close") {
		return "stream-closed"
	}
	return "other"
}

// CollectMemoryStatus uses only the supplied home, existing path resolver and read-only open.
func CollectMemoryStatus(home string) MemoryStatus {
	path, err := memoriesDbPath(home)
	if err != nil {
		path = ""
	}
	if _, err := os.Stat(path); path == "" || err != nil {
		return emptyMemoryStatus(MemoryStatusUnavailable, nil, "no memories store found under "+home)
	}
	db, err := openDbReadOnly(path)
	if err != nil {
		return emptyMemoryStatus(MemoryStatusUnavailable, &path, "could not open "+path+": "+err.Error())
	}
	defer db.Close() // An ignored close failure must not change the snapshot.
	status, err := readMemoryStatus(db, path)
	if err != nil {
		return emptyMemoryStatus(MemoryStatusUnsupported, &path, "could not read the jobs table: "+err.Error())
	}
	return status
}

func readMemoryStatus(db *RwDb, path string) (MemoryStatus, error) {
	read := func(query string) ([]map[string]any, error) {
		stmt, err := db.Prepare(query)
		if err != nil {
			return nil, err
		}
		return stmt.All()
	}
	columns, err := read("PRAGMA table_info(jobs)")
	if err != nil {
		return MemoryStatus{}, err
	}
	if len(columns) == 0 {
		return emptyMemoryStatus(MemoryStatusUnsupported, &path, "no jobs table in this store"), nil
	}
	present := map[string]bool{}
	for _, column := range columns {
		present[memoryStatusString(column["name"])] = true
	}
	missing := []string{}
	for _, name := range []string{"kind", "status", "retry_remaining", "last_error", "finished_at"} {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return emptyMemoryStatus(MemoryStatusUnsupported, &path, "jobs table is missing column(s): "+strings.Join(missing, ", ")), nil
	}
	s := emptyMemoryStatus(MemoryStatusOK, &path, "")
	rows, err := read("SELECT kind, status, COUNT(*) AS count FROM jobs GROUP BY kind, status ORDER BY kind, status")
	if err != nil {
		return MemoryStatus{}, err
	}
	for _, row := range rows {
		s.Jobs = append(s.Jobs, MemoryJobCounts{memoryStatusString(row["kind"]), memoryStatusString(row["status"]), row["count"].(float64)})
	}
	rows, err = read("SELECT last_error FROM jobs WHERE status = 'error' AND retry_remaining = 0")
	if err != nil {
		return MemoryStatus{}, err
	}
	s.Exhausted = float64(len(rows))
	for _, row := range rows {
		cause, count := ClassifyMemoryError(row["last_error"]), float64(1)
		for _, pair := range s.ExhaustedByCause {
			if pair.Cause == cause {
				count += pair.Count
				break
			}
		}
		s.ExhaustedByCause.set(cause, count)
	}
	rows, err = read("SELECT MAX(finished_at) AS at FROM jobs WHERE status = 'done'")
	if err != nil {
		return MemoryStatus{}, err
	}
	s.LastSuccessAt = memoryStatusNumber(rows[0]["at"])
	rows, err = read("SELECT MAX(finished_at) AS at FROM jobs")
	if err != nil {
		return MemoryStatus{}, err
	}
	s.LastFinishedAt = memoryStatusNumber(rows[0]["at"])
	return s, nil
}

func memoryNumberText(n float64) string {
	switch {
	case math.IsNaN(n):
		return "NaN"
	case math.IsInf(n, 1):
		return "Infinity"
	case math.IsInf(n, -1):
		return "-Infinity"
	case n == 0:
		return "0"
	}
	b, _ := json.Marshal(n) // encoding/json uses ECMAScript's finite number spelling.
	return string(b)
}

func memoryStatusString(v any) string {
	switch v := v.(type) {
	case float64:
		return memoryNumberText(v)
	case []byte:
		parts := make([]string, len(v))
		for i, b := range v {
			parts[i] = strconv.Itoa(int(b))
		}
		return strings.Join(parts, ",") // Node's Uint8Array String coercion.
	}
	s, _ := rolloutString(v)
	return s
}

func memoryStatusNumber(v any) *float64 {
	if v == nil {
		return nil
	}
	if n, ok := v.(float64); ok {
		return &n
	}
	s := text.Trim(memoryStatusString(v))
	n := math.NaN()
	switch s {
	case "":
		n = 0
	case "Infinity", "+Infinity":
		n = math.Inf(1)
	case "-Infinity":
		n = math.Inf(-1)
	default:
		base := 0
		if len(s) > 2 && s[0] == '0' {
			switch s[1] {
			case 'x', 'X':
				base = 16
			case 'o', 'O':
				base = 8
			case 'b', 'B':
				base = 2
			}
		}
		if base != 0 {
			// SetString permits a sign; JS unsigned radix literals do not.
			if s[2] != '+' && s[2] != '-' {
				if integer, ok := new(big.Int).SetString(s[2:], base); ok {
					n, _ = new(big.Float).SetInt(integer).Float64()
				}
			}
		} else if decimal, _ := regexp.MatchString(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`, s); decimal {
			n, _ = strconv.ParseFloat(s, 64) // ErrRange's infinity is intentional.
		}
	}
	return &n
}

func memoryAgeLabel(at *float64, now float64) string {
	if at == nil {
		return "never"
	}
	seconds := math.Max(0, now-*at)
	if seconds < 3600 {
		return memoryNumberText(math.Floor(seconds/60)) + "m ago"
	}
	if seconds < 86400 {
		return memoryNumberText(math.Floor(seconds/3600)) + "h ago"
	}
	return memoryNumberText(math.Floor(seconds/86400)) + "d ago"
}

func memoryStatusNow(options []float64) float64 {
	if len(options) != 0 {
		return options[0]
	}
	return float64(time.Now().Unix())
}

// FormatMemoryStatus ends with a newline and never infers backlog or routing.
func FormatMemoryStatus(s MemoryStatus, now ...float64) string {
	scope := "  observation: " + s.ObservationSource + "; effective extraction route: " + s.EffectiveExtractionRoute + "; startup guard decision: " + s.StartupGuardDecision + " (job history does not establish the current route or guard decision)"
	if s.State != MemoryStatusOK {
		return "memory pipeline: " + string(s.State) + " — " + s.Detail + "\n" + scope + "\n"
	}
	path := "null"
	if s.StorePath != nil {
		path = *s.StorePath
	}
	lines := []string{"memory pipeline: " + path, scope}
	if len(s.Jobs) == 0 {
		lines = append(lines, "  jobs: none recorded")
	}
	for _, job := range s.Jobs {
		lines = append(lines, "  "+job.Kind+" "+job.Status+": "+memoryNumberText(job.Count))
	}
	lines = append(lines, "  last success: "+memoryAgeLabel(s.LastSuccessAt, memoryStatusNow(now)))
	if s.Exhausted > 0 {
		causes := slices.Clone(s.ExhaustedByCause)
		slices.SortStableFunc(causes, func(a, b CauseCount) int {
			if a.Count > b.Count {
				return -1
			}
			if a.Count < b.Count {
				return 1
			}
			return 0
		})
		parts := make([]string, len(causes))
		for i, cause := range causes {
			parts[i] = cause.Cause + "=" + memoryNumberText(cause.Count)
		}
		lines = append(lines, "  exhausted (host will not retry): "+memoryNumberText(s.Exhausted)+" ["+strings.Join(parts, ", ")+"]")
	}
	return strings.Join(lines, "\n") + "\n"
}

// MemoryStatusNotice defaults to now and a two-day stale budget; healthy/missing is silent.
func MemoryStatusNotice(s MemoryStatus, options ...float64) string {
	if s.State == MemoryStatusUnsupported {
		return "memory pipeline: unsupported store schema — " + s.Detail
	}
	if s.State == MemoryStatusUnavailable {
		return ""
	}
	now, stale := memoryStatusNow(options), float64(172800)
	if len(options) > 1 {
		stale = options[1]
	}
	parts := []string{}
	if s.LastSuccessAt == nil {
		if len(s.Jobs) > 0 {
			parts = append(parts, "no successful extraction recorded yet")
		}
	} else if now-*s.LastSuccessAt > stale {
		parts = append(parts, "last successful extraction "+memoryAgeLabel(s.LastSuccessAt, now))
	}
	if s.Exhausted > 0 {
		parts = append(parts, memoryNumberText(s.Exhausted)+" job(s) exhausted their retries")
	}
	if len(parts) == 0 {
		return ""
	}
	return "memory pipeline: " + strings.Join(parts, "; ") + " (crw recall memory status)"
}
