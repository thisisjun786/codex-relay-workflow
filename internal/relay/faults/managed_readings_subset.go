package faults

// subset ported for todo 22; todo 24 owns omitted.py.
// Only the reporting fields consumed by faultsweep.reading_faults are projected
// here. A caller with todo 24's complete observer supplies ManagedReadingObserver
// instead. This reader never opens a writable store or creates marker files.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ManagedOmittedObserver reads the marker witness and an exact settled turn.
// Selection must be the caller's store.StateSelection, not a guessed live path.
type ManagedOmittedObserver struct{}

func (ManagedOmittedObserver) Observe(ctx context.Context, request ManagedReadingRequest) (value any, failure error) {
	selection, ok := request.Selection.(store.StateSelection)
	if !ok {
		return nil, fmt.Errorf("TypeError: managed readings need a store selection")
	}
	segment := func(s string) bool {
		return strings.TrimSpace(s) != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
	}
	if !segment(request.Session) || !segment(request.Turn) {
		return nil, fmt.Errorf("ValueError: session and turn must be valid path segments")
	}
	result := map[string]any{"schema": "reporting-observation/1", "reportingState": "unmeasured", "reason": nil, "selectors": map[string]any{"turn": request.Turn, "session": request.Session}}
	answer := func(state, reason string) (any, error) {
		result["reportingState"], result["reason"] = state, reason
		return result, nil
	}
	root, err := store.ResolvePath(request.Root)
	if err != nil {
		return answer("unmeasured", "evidence_unreadable: "+err.Error())
	}
	workspace, err := store.ResolvePath(request.Workspace)
	if err != nil {
		return answer("unmeasured", "evidence_unreadable: "+err.Error())
	}
	hash := sha256.Sum256([]byte(workspace))
	directory := filepath.Join(root, fmt.Sprintf("%x", hash), request.Assignment)
	read := func(path string) (map[string]any, error) { return managedMarkerObject(root, path) }
	intent, err := read(filepath.Join(directory, "intent.json"))
	if err != nil {
		return answer("unmeasured", err.Error())
	}
	if intent == nil {
		return answer("unmanaged", "marker_absent")
	}
	claim, err := read(filepath.Join(directory, "claims", request.Session, "claim.json"))
	if err != nil {
		return answer("unmeasured", err.Error())
	}
	dispatch, _ := claim["dispatchRequestId"].(string)
	dispatchHash := sha256.Sum256([]byte(dispatch))
	if claim["sessionId"] != request.Session || dispatch == "" || intent["dispatchRequestIdHash"] != fmt.Sprintf("%x", dispatchHash) {
		return answer("unmeasured", "dispatch_uncorrelated")
	}
	bound, err := read(filepath.Join(directory, "bound.json"))
	if err != nil {
		return answer("unmeasured", err.Error())
	}
	if bound["sessionId"] != request.Session {
		return answer("unmeasured", "session_unbound_or_foreign")
	}
	declaredWork, _ := intent["workspace"].(string)
	declaredDB, _ := intent["dbPath"].(string)
	work, e1 := store.ResolvePath(declaredWork)
	dbPath, e2 := store.ResolvePath(declaredDB)
	selected, e3 := store.ResolvePath(selection.DBPath())
	if intent["dispatchRequestIdHash"] != request.Assignment || declaredWork == "" || declaredDB == "" || e1 != nil || e2 != nil || e3 != nil || work != workspace || dbPath != selected {
		return answer("unmeasured", "marker_selector_mismatch")
	}
	relationship, err := read(filepath.Join(directory, "relationship.json"))
	if err != nil {
		return answer("unmeasured", err.Error())
	}
	rid, _ := relationship["relationshipId"].(string)
	if rid == "" {
		return answer("unmeasured", "registration_unresolved")
	}
	stops, err := os.ReadDir(filepath.Join(directory, "hook", request.Session, request.Turn))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return answer("unmeasured", "stop_unreadable")
	}
	type stop struct {
		number int
		record map[string]any
	}
	witnesses := []stop{}
	seen := map[int]bool{}
	for _, entry := range stops {
		name := entry.Name()
		if name == "hold.json" || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, e := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if e != nil || n < 0 || len(witnesses) >= 128 {
			return answer("unmeasured", "stop_history_invalid")
		}
		if seen[n] {
			return answer("unmeasured", "stop_sequence_ambiguous")
		}
		seen[n] = true
		record, e := read(filepath.Join(directory, "hook", request.Session, request.Turn, name))
		if e != nil {
			return answer("unmeasured", e.Error())
		}
		_, labelOK := record["observation"].(string)
		_, decisionOK := record["decisionState"].(string)
		at, _ := record["at"].(string)
		_, e = time.Parse(time.RFC3339Nano, at)
		if record["sessionId"] != request.Session || record["turnId"] != request.Turn || !labelOK || !decisionOK || e != nil {
			return answer("unmeasured", "stop_identity_or_shape")
		}
		witnesses = append(witnesses, stop{n, record})
	}
	sort.Slice(witnesses, func(i, j int) bool { return witnesses[i].number < witnesses[j].number })
	db, err := store.OpenReadOnly(ctx, selection.DBPath(), 5*time.Second)
	if err != nil {
		return answer("unmeasured", "store_unreadable: "+err.Error())
	}
	defer func() { failure = errors.Join(failure, db.Close()) }()
	// Keep all registry/settlement/event facts in one read-only SQLite snapshot.
	if _, err = db.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return answer("unmeasured", "store_unreadable: "+err.Error())
	}
	defer func() { _, err := db.ExecContext(ctx, "ROLLBACK"); failure = errors.Join(failure, err) }()
	var child, issue string
	var cwd, superseded, anchor sql.NullString
	var generation, opened int64
	err = db.QueryRowContext(ctx, `SELECT r.child_task_id,r.issue_key,r.child_cwd,r.execution_generation,r.superseded_by,g.execution_generation,g.dispatch_turn_id FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id WHERE r.relationship_id=? AND g.dispatch_request_id=?`, rid, dispatch).Scan(&child, &issue, &cwd, &generation, &superseded, &opened, &anchor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return answer("unmeasured", "registration_unresolved")
		}
		return answer("unmeasured", "store_unreadable: "+err.Error())
	}
	result["relationshipId"], result["executionGeneration"] = rid, opened
	actualWork, e := store.ResolvePath(cwd.String)
	if child != request.Session || bound["taskId"] != request.Session || issue != intent["issueKey"] || !cwd.Valid || e != nil || actualWork != workspace {
		return answer("unmeasured", "registry_identity_mismatch")
	}
	if generation != opened || superseded.Valid {
		return answer("unmeasured", "stale_generation")
	}
	var managedCount int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM managed_start_requests WHERE dispatch_request_id=?", dispatch).Scan(&managedCount); err != nil {
		return answer("unmeasured", "store_unreadable: "+err.Error())
	}
	if managedCount > 1 {
		return answer("unmeasured", "managed_request_ambiguous")
	}
	bootstrap := false
	if managedCount == 1 {
		var mc, mr, mw, mm, mi string
		var mg int64
		var standby sql.NullString
		err = db.QueryRowContext(ctx, "SELECT child_task_id,relationship_id,execution_generation,workspace,marker_root,issue_key,standby_turn_id FROM managed_start_requests WHERE dispatch_request_id=?", dispatch).Scan(&mc, &mr, &mg, &mw, &mm, &mi, &standby)
		if err != nil {
			return answer("unmeasured", "store_unreadable: "+err.Error())
		}
		mw, e1 = store.ResolvePath(mw)
		mm, e2 = store.ResolvePath(mm)
		if mc != child || mr != rid || mg != opened || mi != issue || e1 != nil || e2 != nil || mw != workspace || mm != root {
			return answer("unmeasured", "managed_request_identity_mismatch")
		}
		bootstrap = standby.Valid && standby.String == request.Turn
	}
	admitted := anchor.Valid && anchor.String == request.Turn
	if !admitted {
		var n int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM generation_turns WHERE relationship_id=? AND execution_generation=? AND turn_id=? AND evidence=?", rid, opened, request.Turn, "explicit_admission_bound:"+anchor.String).Scan(&n)
		if err != nil {
			return answer("unmeasured", "store_unreadable: "+err.Error())
		}
		admitted = anchor.Valid && anchor.String != "" && n > 0
	}
	if len(witnesses) == 0 {
		return answer("unmeasured", "stop_unobserved")
	}
	if bootstrap {
		return answer("unmeasured", "bootstrap")
	}
	if !admitted {
		return answer("unmeasured", "admission_unrecorded")
	}
	var states string
	err = db.QueryRowContext(ctx, "SELECT COALESCE(json_group_array(DISTINCT terminal_status),'[]') FROM assignment_settlements WHERE relationship_id=? AND thread_id=? AND turn_id=?", rid, request.Session, request.Turn).Scan(&states)
	if err != nil {
		return answer("unmeasured", "store_unreadable: "+err.Error())
	}
	terminal := []string{}
	if err = json.Unmarshal([]byte(states), &terminal); err != nil {
		return nil, err
	}
	if len(terminal) > 1 {
		return answer("unmeasured", "terminal_conflict")
	}
	status := "unobserved"
	if len(terminal) == 1 {
		status = terminal[0]
	}
	if status != "unobserved" && status != "completed" && status != "failed" && status != "interrupted" {
		return answer("unmeasured", "terminal_conflict")
	}
	disposition, err := read(filepath.Join(directory, "dispositions", request.Session, request.Turn+".json"))
	if err != nil {
		return answer("unmeasured", "disposition_unreadable_or_malformed")
	}
	label := "undeclared_turn_end"
	if disposition["sessionId"] == request.Session && disposition["turnId"] == request.Turn {
		switch disposition["outcome"] {
		case "in_progress":
			return answer("in_progress", "declared_in_progress")
		case "blocked_needs_input", "failed", "interrupted":
			return answer("reported", "declared_"+disposition["outcome"].(string))
		case "ready_for_review":
			// A declaration clears only when its current receipt still verifies.
			matched, readable, e := managedReceipt(ctx, db, rid, opened, request.Session, request.Turn, relationship)
			if e != nil {
				return answer("unmeasured", "store_unreadable: "+e.Error())
			}
			if !readable {
				return answer("unmeasured", "receipt_unreadable")
			}
			if matched {
				return answer("reported", "declared_ready_receipted")
			}
			label = "receipt_missing"
		}
	}
	if status == "failed" || status == "interrupted" {
		var n int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE relationship_id=? AND execution_generation=? AND turn_thread_id=? AND turn_id=? AND producer='daemon' AND stage='final' AND outcome=? AND turn_status=?", rid, opened, request.Session, request.Turn, status, status).Scan(&n)
		if err != nil {
			return answer("unmeasured", "store_unreadable: "+err.Error())
		}
		if n > 0 {
			return answer("reported", "daemon_execution_report")
		}
	}
	if status == "unobserved" {
		return answer("unmeasured", "host_terminal_unobserved")
	}
	last := witnesses[len(witnesses)-1].record
	witnessed := last["observation"] == "managed_unregistered" || last["observation"] == "receipt_missing" || last["observation"] == "undeclared_turn_end" || last["decisionState"] == "unresolved_handoff"
	if !witnessed || (label != "undeclared_turn_end" && label != "receipt_missing") {
		return answer("unmeasured", "no_confirmed_omission")
	}
	return answer("unreported", "terminal_without_report")
}

// managedReceipt uses the same declared-lineage and artifact checks as the
// reporting projection, returning only the two facts its classifier consumes.
func managedReceipt(ctx context.Context, db *store.ReadOnly, rid string, generation int64, session, turn string, marker map[string]any) (bool, bool, error) {
	if stamp, ok := marker["executionGeneration"].(float64); ok && int64(stamp) != generation {
		return false, true, nil
	}
	var status, rootsJSON string
	if err := db.QueryRowContext(ctx, "SELECT status,artifact_roots FROM relationships WHERE relationship_id=?", rid).Scan(&status, &rootsJSON); err != nil {
		return false, false, err
	}
	if status != "active" {
		return false, true, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT e.event_id,e.revision_hash,l.supersedes_hash,e.producer,e.stage,e.turn_thread_id,e.turn_id,e.receipt,e.manifest_ref
 FROM events e LEFT JOIN revision_lineage l ON l.event_id=e.event_id
 WHERE e.relationship_id=? AND e.execution_generation=? AND e.outcome='ready_for_review' AND e.suppressed_reason IS NULL ORDER BY e.event_id`, rid, generation)
	if err != nil {
		return false, false, err
	}
	type receipt struct {
		id, hash, producer, stage, session, turn, raw string
		previous, frozen                              sql.NullString
	}
	nodes := []receipt{}
	for rows.Next() {
		var r receipt
		if err = rows.Scan(&r.id, &r.hash, &r.previous, &r.producer, &r.stage, &r.session, &r.turn, &r.raw, &r.frozen); err != nil {
			break
		}
		nodes = append(nodes, r)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return false, false, err
	}
	if len(nodes) == 0 {
		return false, true, nil
	}
	byHash := map[string][]string{}
	for _, r := range nodes {
		byHash[r.hash] = append(byHash[r.hash], r.id)
	}
	edges := map[string]string{}
	used := map[string]bool{}
	for _, r := range nodes {
		if !r.previous.Valid || r.previous.String == "" {
			continue
		}
		targets := byHash[r.previous.String]
		if len(targets) != 1 {
			return false, true, nil
		}
		if used[targets[0]] {
			return false, true, nil
		}
		used[targets[0]] = true
		edges[r.id] = targets[0]
	}
	tip := -1
	for i, r := range nodes {
		if !used[r.id] {
			if tip >= 0 {
				return false, true, nil
			}
			tip = i
		}
	}
	if tip < 0 {
		return false, true, nil
	}
	head := nodes[tip]
	seen := map[string]bool{}
	for id := head.id; id != ""; id = edges[id] {
		if seen[id] {
			return false, true, nil
		}
		seen[id] = true
	}
	if len(seen) != len(nodes) || head.producer != "child" || (head.stage != "staged" && head.stage != "final") || head.session != session || head.turn != turn {
		return false, true, nil
	}
	var payload struct {
		Manifest []store.ManifestEntry `json:"manifest"`
		Revision string                `json:"revisionHash"`
	}
	if err = json.Unmarshal([]byte(head.raw), &payload); err != nil || len(payload.Manifest) == 0 {
		return false, true, nil
	}
	revision, err := store.ManifestRevision(payload.Manifest)
	if err != nil || revision != payload.Revision {
		return false, true, nil
	}
	var roots []string
	if err = json.Unmarshal([]byte(rootsJSON), &roots); err != nil {
		return false, true, nil
	}
	matches, readable := true, true
	for _, entry := range payload.Manifest {
		digest, size, _, e := store.HashArtifact(entry.Path, roots, false)
		if e != nil {
			matches = false
			readable = false
			continue
		}
		if digest != entry.SHA256 || (entry.Bytes != nil && *entry.Bytes != size) {
			matches = false
		}
	}
	if matches {
		return true, true, nil
	}
	if head.frozen.Valid && len(store.VerifyFrozen(head.frozen.String, payload.Manifest)) == 0 {
		return true, true, nil
	}
	return false, readable, nil
}

func managedMarkerObject(root, path string) (map[string]any, error) {
	for current := path; current != root; current = filepath.Dir(current) {
		if current == filepath.Dir(current) {
			return nil, fmt.Errorf("marker_symlink")
		}
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("marker_unreadable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("marker_symlink")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("marker_not_regular")
		}
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("marker_unreadable")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("marker_unreadable")
	}
	if len(raw) > 1024*1024 {
		return nil, fmt.Errorf("marker_record_limit")
	}
	var object map[string]any
	if err = json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("marker_unreadable")
	}
	return object, nil
}
