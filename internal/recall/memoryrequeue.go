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

	// rowid names the row that was selected, when the table has one; the write touches that row and no other that shares its kind and key
	// (known-defects.md :624).
	rowid    int64
	hasRowid bool
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

	// selectedFile is the store file as it was when the rows were selected; the write opens the same file or none (known-defects.md :623).
	selectedFile os.FileInfo
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
		if *n < 1 {
			// An allowance below one floors to zero: the backoff would be cleared and the row still not eligible (known-defects.md :617).
			return emptyRequeue(MemoryStatusUnavailable, "--retries "+memoryNumberText(*n)+" must be a whole number of at least 1", nil, retries)
		}
		retries = math.Floor(*n)
	}
	path, err := memoriesDbPath(home)
	if err != nil {
		path = ""
	}
	if _, err := os.Stat(path); path == "" || err != nil {
		return emptyRequeue(MemoryStatusUnavailable, "no memories store found under "+home, nil, retries)
	}
	selectedFile, _ := os.Stat(path)
	db, err := openDbReadOnly(path)
	if err != nil {
		return emptyRequeue(MemoryStatusUnavailable, "could not open "+path+": "+err.Error(), &path, retries)
	}
	r, err := readMemoryRequeue(db, path, retries, opts)
	r.selectedFile = selectedFile
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
	// Every column the apply writes or matches is checked here, so a store that passes selection cannot fail the write (known-defects.md :618).
	for _, name := range []string{"kind", "job_key", "status", "retry_remaining", "retry_at", "last_error"} {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return emptyRequeue(MemoryStatusUnsupported, "jobs table is missing column(s): "+strings.Join(missing, ", "), &path, retries), nil
	}
	const where = " FROM jobs WHERE status = 'error' AND retry_remaining = 0 ORDER BY kind, job_key"
	withRowid := true
	rows, err := read("SELECT rowid AS rid, typeof(kind) AS kt, typeof(job_key) AS jt, kind, job_key, last_error" + where + ", rowid")
	if err != nil {
		// A table without rowids is addressed by kind and key.
		withRowid = false
		if rows, err = read("SELECT typeof(kind) AS kt, typeof(job_key) AS jt, kind, job_key, last_error" + where); err != nil {
			return r, err
		}
	}
	for _, row := range rows {
		// A kind or key that is not text (NULL, a number, a BLOB) cannot be addressed by the text the write binds; it is left alone
		// as it is, not turned into a string that names no row (known-defects.md :619).
		if row["kt"] != "text" || row["jt"] != "text" {
			addRequeueCause(&r.SkippedByCause, "untyped-key")
			continue
		}
		kind, cause := memoryStatusString(row["kind"]), ClassifyMemoryError(row["last_error"])
		// Consolidation jobs are never selected unless the kind names them (the module's own safety rule, known-defects.md :620).
		if opts.Kind == "" && requeueIsConsolidation(kind) {
			addRequeueCause(&r.SkippedByCause, "consolidation")
			continue
		}
		wantedKind := opts.Kind == "" || opts.Kind == kind
		wantedCause := slices.Contains(TransientCauses(), cause) || (opts.IncludeContextWindow && cause == "context-window")
		if wantedKind && wantedCause {
			c := RequeueCandidate{Kind: kind, JobKey: memoryStatusString(row["job_key"]), Cause: cause}
			if withRowid {
				if id, ok := row["rid"].(float64); ok {
					c.rowid, c.hasRowid = int64(id), true
				}
			}
			r.Selected = append(r.Selected, c)
		} else {
			addRequeueCause(&r.SkippedByCause, cause)
		}
	}
	if n := opts.Limit; n != nil && !math.IsNaN(*n) && !math.IsInf(*n, 0) && *n >= 0 && *n < float64(len(r.Selected)) {
		r.Selected = r.Selected[:int(math.Trunc(*n))]
	}
	return r, nil
}

// requeueIsConsolidation is a job kind of the memory consolidation pass, which is not an extraction that can be retried unchanged.
func requeueIsConsolidation(kind string) bool { return strings.Contains(Lower(kind), "consolidat") }

func applyMemoryRequeue(r RequeueResult) RequeueResult {
	// The store is opened for writing without CREATE, and it must be the file the rows were selected from: a store removed or replaced
	// since then is reported, not recreated empty or written blindly (known-defects.md :623).
	db, err := openDbReadWriteExisting(*r.StorePath)
	if err != nil {
		r.State, r.Detail = MemoryStatusUnavailable, "could not open for writing: "+err.Error()
		return r
	}
	defer db.Close()
	if now, err := os.Stat(*r.StorePath); err != nil || r.selectedFile != nil && !os.SameFile(r.selectedFile, now) {
		r.State, r.Detail = MemoryStatusUnavailable, "the store changed between selection and apply; nothing was changed"
		return r
	}
	changed, began, err := writeMemoryRequeue(db, r)
	if err != nil {
		if !began {
			// BEGIN did not succeed, so there is nothing to roll back (known-defects.md :622).
			r.State, r.Detail = MemoryStatusUnavailable, "requeue did not start and nothing was changed: "+err.Error()
			return r
		}
		detail := "requeue failed and was rolled back: " + err.Error()
		if rb := db.Exec("ROLLBACK"); rb != nil && rb.Error() != "cannot rollback - no transaction is active" {
			detail = "requeue failed and the rollback failed too (" + rb.Error() + "): " + err.Error()
		}
		r.State, r.Detail = MemoryStatusUnavailable, detail
		return r
	}
	r.Applied, r.Changed = true, changed
	return r
}

// writeMemoryRequeue writes in one transaction and says whether BEGIN succeeded. Each selected row is written by its own identity, and the
// changes reported are the changes the database made.
func writeMemoryRequeue(db *RwDb, r RequeueResult) (changed float64, began bool, err error) {
	if err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		return 0, false, err
	}
	// A candidate the host has picked up since selection must not be clobbered.
	const set = "UPDATE jobs SET retry_remaining = ?, retry_at = NULL WHERE status = 'error' AND retry_remaining = 0 AND "
	byKey, err := db.Prepare(set + "kind = ? AND job_key = ?")
	if err != nil {
		return 0, true, err
	}
	byRow, err := db.Prepare(set + "rowid = ? AND kind = ? AND job_key = ?")
	if err != nil {
		byRow = nil // a table without rowids
	}
	for _, c := range r.Selected {
		var info RunResult
		if c.hasRowid && byRow != nil {
			info, err = byRow.Run(r.Retries, c.rowid, c.Kind, c.JobKey)
		} else {
			info, err = byKey.Run(r.Retries, c.Kind, c.JobKey)
		}
		if err != nil {
			return 0, true, err
		}
		changed += info.Changes
	}
	if err := db.Exec("COMMIT"); err != nil {
		return 0, true, err
	}
	return changed, true, nil
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
			if pair.Cause == "consolidation" && pair.Count != 0 {
				lines = append(lines, "    consolidation jobs are left alone unless --kind names their kind")
			}
			if pair.Cause == "untyped-key" && pair.Count != 0 {
				lines = append(lines, "    rows whose kind or key is not text cannot be addressed and are never written")
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
