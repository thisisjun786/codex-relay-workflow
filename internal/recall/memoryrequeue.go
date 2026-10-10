// Memory job requeue from CXC v0.2.40 recall/src/memory-requeue.ts.
package recall

import (
	"cmp"
	"errors"
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
	// key holds the values of the primary key columns of a table without rowids, in the order of RequeueResult.keyColumns.
	key []any
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
	// keyColumns are the primary key columns that name a row of a table without rowids; noIdentity is set when the rows have neither a
	// rowid nor a primary key, so that no write can name one row (known-defects.md :624).
	keyColumns []string
	noIdentity bool
	// rowidAlias is the name of the rowid that no column of the table shadows, the name the write uses for the selected row.
	rowidAlias string
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
	// The store file is held open from selection to the end of the apply: while it is open its inode cannot be given to a file put in its
	// place, so the identity the apply compares names this file and no later one (known-defects.md :623).
	var selectedFile os.FileInfo
	if hold, err := os.Open(path); err == nil {
		defer hold.Close()
		selectedFile, _ = hold.Stat()
	}
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
	// A column named rowid, _rowid_ or oid hides the row's own rowid under that name and need not be unique, so the row is named by an
	// alias no column shadows (known-defects.md :624).
	alias := ""
	for _, name := range []string{"rowid", "_rowid_", "oid"} {
		if !presentFold(present, name) {
			alias = name
			break
		}
	}
	withRowid := alias != ""
	var rows []map[string]any
	if withRowid {
		rows, err = read("SELECT CAST(" + alias + " AS TEXT) AS rid, typeof(kind) AS kt, typeof(job_key) AS jt, kind, job_key, last_error" + where + ", " + alias)
		withRowid = err == nil
	}
	var keyColumns []string
	if !withRowid {
		// A table without rowids, or one whose every rowid alias is a column, is addressed by its primary key: two rows can share a kind
		// and a key, and only the key names the one that was selected.
		type pkColumn struct {
			order float64
			name  string
		}
		var pk []pkColumn
		for _, column := range columns {
			if order, ok := column["pk"].(float64); ok && order > 0 {
				pk = append(pk, pkColumn{order, memoryStatusString(column["name"])})
			}
		}
		slices.SortFunc(pk, func(a, b pkColumn) int { return cmp.Compare(a.order, b.order) })
		query := "SELECT typeof(kind) AS kt, typeof(job_key) AS jt, kind, job_key, last_error"
		for i, c := range pk {
			quoted := `"` + strings.ReplaceAll(c.name, `"`, `""`) + `"`
			query += ", " + quoted + " AS pk" + strconv.Itoa(i) + ", typeof(" + quoted + ") AS pt" + strconv.Itoa(i) + ", CAST(" + quoted + " AS TEXT) AS ps" + strconv.Itoa(i)
			keyColumns = append(keyColumns, quoted)
		}
		if rows, err = read(query + where); err != nil {
			return r, err
		}
		r.keyColumns, r.noIdentity = keyColumns, len(pk) == 0
	} else {
		r.rowidAlias = alias
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
				// The rowid is read as text: a number read as a float64 loses the low digits of a rowid past 2^53.
				if text, ok := row["rid"].(string); ok {
					if id, err := strconv.ParseInt(text, 10, 64); err == nil {
						c.rowid, c.hasRowid = id, true
					}
				}
			}
			for i := range keyColumns {
				// A key column is bound back as it was read; a value of another type than text or integer cannot be bound back exactly.
				value, typ := row["pk"+strconv.Itoa(i)], row["pt"+strconv.Itoa(i)]
				if text, ok := row["ps"+strconv.Itoa(i)].(string); ok && typ == "integer" {
					number, err := strconv.ParseInt(text, 10, 64)
					if err != nil {
						addRequeueCause(&r.SkippedByCause, "untyped-key")
						c.key = nil
						break
					}
					value = number
				} else if _, ok := value.(string); !ok || typ != "text" {
					addRequeueCause(&r.SkippedByCause, "untyped-key")
					c.key = nil
					break
				}
				c.key = append(c.key, value)
			}
			if len(keyColumns) != 0 && len(c.key) != len(keyColumns) {
				continue
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

// presentFold reports whether the table has a column of that name; SQLite compares column names without regard to ASCII case.
func presentFold(present map[string]bool, name string) bool {
	for column := range present {
		if strings.EqualFold(column, name) {
			return true
		}
	}
	return false
}

// requeueIsConsolidation is a job kind of the memory consolidation pass, which is not an extraction that can be retried unchanged.
func requeueIsConsolidation(kind string) bool { return strings.Contains(Lower(kind), "consolidat") }

func applyMemoryRequeue(r RequeueResult) RequeueResult {
	if r.noIdentity {
		r.State, r.Detail = MemoryStatusUnavailable, "the jobs table has neither rowids nor a primary key to name a row by; nothing was changed"
		return r
	}
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
	var byRow *Stmt
	if r.rowidAlias != "" {
		if byRow, err = db.Prepare(set + r.rowidAlias + " = ? AND kind = ? AND job_key = ?"); err != nil {
			return 0, true, err
		}
	}
	var byPK *Stmt
	if len(r.keyColumns) != 0 {
		if byPK, err = db.Prepare(set + "kind = ? AND job_key = ? AND " + strings.Join(r.keyColumns, " = ? AND ") + " = ?"); err != nil {
			return 0, true, err
		}
	}
	for _, c := range r.Selected {
		var info RunResult
		switch {
		case c.hasRowid && byRow != nil:
			info, err = byRow.Run(r.Retries, c.rowid, c.Kind, c.JobKey)
		case byPK != nil && len(c.key) == len(r.keyColumns):
			info, err = byPK.Run(append([]any{r.Retries, c.Kind, c.JobKey}, c.key...)...)
		default:
			// A row that was selected without an identity is not written by its kind and key, which other rows can share.
			err = errors.New("a selected row has no rowid or primary key to be written by")
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
