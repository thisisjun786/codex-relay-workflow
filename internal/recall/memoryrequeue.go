// Memory job requeue from CXC v0.2.40 recall/src/memory-requeue.ts.
package recall

import (
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

type RequeueCandidate struct {
	Kind   string `json:"kind"`
	JobKey string `json:"jobKey"`
	Cause  string `json:"cause"`
}

type RequeueOptions struct {
	Apply                bool     `json:"apply"`
	IncludeContextWindow bool     `json:"includeContextWindow"`
	Kind                 string   `json:"kind"`
	Limit                *float64 `json:"limit"`
	Retries              *float64 `json:"retries"`
}

type RequeueResult struct {
	State          MemoryStatusState  `json:"state"`
	Detail         string             `json:"detail"`
	StorePath      *string            `json:"storePath"`
	Applied        bool               `json:"applied"`
	Selected       []RequeueCandidate `json:"selected"`
	SkippedByCause CauseCounts        `json:"skippedByCause"`
	Changed        float64            `json:"changed"`
	Retries        float64            `json:"retries"`
}

// TransientCauses returns the upstream set without mutable package state.
func TransientCauses() []string {
	return []string{"capacity", "incomplete-response", "stream-closed", "unknown", "other"}
}

func emptyRequeue(state MemoryStatusState, detail string, path *string, retries float64) RequeueResult {
	return RequeueResult{State: state, Detail: detail, StorePath: path,
		Selected: []RequeueCandidate{}, SkippedByCause: CauseCounts{}, Retries: retries}
}

func addRequeueCause(counts *CauseCounts, cause string) {
	count := float64(1)
	for _, pair := range *counts {
		if pair.Cause == cause {
			count += pair.Count
			break
		}
	}
	counts.set(cause, count)
}

// RequeueExhaustedMemoryJobs only resolves the supplied home. Without Apply,
// selection uses a read-only connection and never opens the store for writing.
func RequeueExhaustedMemoryJobs(home string, options ...RequeueOptions) RequeueResult {
	var opts RequeueOptions
	if len(options) != 0 {
		opts = options[0]
	}
	retries := float64(3)
	if n := opts.Retries; n != nil && !math.IsNaN(*n) && !math.IsInf(*n, 0) && *n > 0 {
		retries = math.Floor(*n)
	}
	path, err := memoriesDbPath(home)
	if err != nil {
		path = ""
	}
	if _, err := os.Stat(path); path == "" || err != nil {
		return emptyRequeue(MemoryStatusUnavailable, "no memories store found under "+home, nil, retries)
	}
	db, err := openDbReadOnly(path)
	if err != nil {
		return emptyRequeue(MemoryStatusUnavailable, "could not open "+path+": "+err.Error(), &path, retries)
	}
	r, err := readMemoryRequeue(db, path, retries, opts)
	_ = db.Close() // Close before opening a write connection, including on read failures.
	if err != nil {
		return emptyRequeue(MemoryStatusUnsupported, "could not read the jobs table: "+err.Error(), &path, retries)
	}
	if r.State != MemoryStatusOK || !opts.Apply || len(r.Selected) == 0 {
		return r
	}
	return applyMemoryRequeue(r)
}

func readMemoryRequeue(db *RwDb, path string, retries float64, opts RequeueOptions) (RequeueResult, error) {
	read := func(sql string) ([]map[string]any, error) {
		stmt, err := db.Prepare(sql)
		if err != nil {
			return nil, err
		}
		return stmt.All()
	}
	r := emptyRequeue(MemoryStatusOK, "", &path, retries)
	columns, err := read("PRAGMA table_info(jobs)")
	if err != nil {
		return r, err
	}
	if len(columns) == 0 {
		return emptyRequeue(MemoryStatusUnsupported, "no jobs table in this store", &path, retries), nil
	}
	present := map[string]bool{}
	for _, column := range columns {
		present[memoryStatusString(column["name"])] = true
	}
	missing := []string{}
	for _, name := range []string{"kind", "job_key", "status", "retry_remaining", "last_error"} {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return emptyRequeue(MemoryStatusUnsupported, "jobs table is missing column(s): "+strings.Join(missing, ", "), &path, retries), nil
	}
	rows, err := read("SELECT kind, job_key, last_error FROM jobs WHERE status = 'error' AND retry_remaining = 0 ORDER BY kind, job_key")
	if err != nil {
		return r, err
	}
	for _, row := range rows {
		kind, cause := memoryStatusString(row["kind"]), ClassifyMemoryError(row["last_error"])
		wantedKind := opts.Kind == "" || opts.Kind == kind
		wantedCause := slices.Contains(TransientCauses(), cause) || (opts.IncludeContextWindow && cause == "context-window")
		if wantedKind && wantedCause {
			r.Selected = append(r.Selected, RequeueCandidate{kind, memoryStatusString(row["job_key"]), cause})
		} else {
			addRequeueCause(&r.SkippedByCause, cause)
		}
	}
	if n := opts.Limit; n != nil && !math.IsNaN(*n) && !math.IsInf(*n, 0) && *n >= 0 && *n < float64(len(r.Selected)) {
		r.Selected = r.Selected[:int(math.Trunc(*n))]
	}
	return r, nil
}

func applyMemoryRequeue(r RequeueResult) RequeueResult {
	db, err := openDbReadWrite(*r.StorePath)
	if err != nil {
		r.State, r.Detail = MemoryStatusUnavailable, "could not open for writing: "+err.Error()
		return r
	}
	defer db.Close()
	changed, err := writeMemoryRequeue(db, r)
	if err != nil {
		_ = db.Exec("ROLLBACK") // BEGIN/COMMIT may themselves have failed.
		r.State, r.Detail = MemoryStatusUnavailable, "requeue failed and was rolled back: "+err.Error()
		return r
	}
	r.Applied, r.Changed = true, changed
	return r
}

func writeMemoryRequeue(db *RwDb, r RequeueResult) (float64, error) {
	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	// A candidate the host has picked up since selection must not be clobbered.
	stmt, err := db.Prepare("UPDATE jobs SET retry_remaining = ?, retry_at = NULL WHERE kind = ? AND job_key = ? AND status = 'error' AND retry_remaining = 0")
	if err != nil {
		return 0, err
	}
	changed := float64(0)
	for _, c := range r.Selected {
		info, err := stmt.Run(r.Retries, c.Kind, c.JobKey)
		if err != nil {
			return 0, err
		}
		changed += info.Changes
	}
	if err := db.Exec("COMMIT"); err != nil {
		return 0, err
	}
	return changed, nil
}

func requeueCauseText(counts CauseCounts) string {
	counts = slices.Clone(counts)
	slices.SortStableFunc(counts, func(a, b CauseCount) int {
		if a.Count > b.Count {
			return -1
		}
		if a.Count < b.Count {
			return 1
		}
		return 0
	})
	parts := make([]string, len(counts))
	for i, pair := range counts {
		parts[i] = pair.Cause + "=" + memoryNumberText(pair.Count)
	}
	return strings.Join(parts, ", ")
}

// FormatRequeue reproduces the upstream report, including stable cause-count ties.
func FormatRequeue(r RequeueResult) string {
	if r.State != MemoryStatusOK {
		return "memory requeue: " + string(r.State) + " — " + r.Detail + "\n"
	}
	path := "null"
	if r.StorePath != nil {
		path = *r.StorePath
	}
	counts := CauseCounts{}
	for _, c := range r.Selected {
		addRequeueCause(&counts, c.Cause)
	}
	selected := "  selected: " + strconv.Itoa(len(r.Selected))
	if text := requeueCauseText(counts); text != "" {
		selected += " [" + text + "]"
	}
	lines := []string{"memory requeue: " + path, selected}
	if len(r.SkippedByCause) != 0 {
		lines = append(lines, "  left alone: "+requeueCauseText(r.SkippedByCause))
		for _, pair := range r.SkippedByCause {
			if pair.Cause == "context-window" && pair.Count != 0 {
				lines = append(lines, "    context-window failures repeat unless the extraction input changes; --include-context-window overrides")
			}
		}
	}
	if r.Applied {
		lines = append(lines, "  applied: "+memoryNumberText(r.Changed)+" row(s) given "+memoryNumberText(r.Retries)+" retries")
	} else {
		lines = append(lines, "  dry run — nothing written (pass --apply)")
	}
	return strings.Join(lines, "\n") + "\n"
}
