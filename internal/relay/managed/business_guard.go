package managed

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// businessGuard is the last decision before the host starts the business turn, made by the host just ahead
// of it with a context of its own (never the Run's): the store, the ledger and the worker policy are what
// the start saw, and the relationship registered for the child still is what the request asked for. It
// answers nil to let the send through, or the refusal that withholds it. The checks run in a fixed order, so
// a start that changed in two ways is refused for the first; the host's own answer about the thread comes
// last and has its own wording.
func (r *startRun) businessGuard(ctx context.Context) (map[string]any, error) {
	refuse := func(code string) (map[string]any, error) {
		return map[string]any{"code": code, "message": "Managed business start withheld: " + code}, nil
	}
	if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
		return refuse("managed_store_changed")
	}
	if code, err := r.m.ready(ctx, r.req); err != nil {
		return nil, err
	} else if code != "" {
		return refuse(code)
	}
	read, ok := r.openUnchangedStore(ctx)
	if !ok {
		return refuse("managed_store_changed")
	}
	defer read.Close()
	for _, check := range []func(context.Context, *store.ReadOnly) string{r.guardRelationship, r.guardGeneration, r.guardSettings, r.guardCriteria} {
		if code := check(ctx, read); code != "" {
			return refuse(code)
		}
	}
	code, err := hostReady(ctx, r.m.Adapter, r.task)
	if err != nil {
		return nil, err
	}
	if code != "" {
		return map[string]any{"code": code, "message": "Managed turn withheld: " + code}, nil
	}
	return nil, nil
}

// openUnchangedStore opens the store read-only, provided the file at its path is the one the start began
// with.
func (r *startRun) openUnchangedStore(ctx context.Context) (*store.ReadOnly, bool) {
	info, err := os.Stat(r.m.Store.Path)
	if err != nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != r.physical.Device || stat.Ino != r.physical.Inode {
		return nil, false
	}
	read, err := store.OpenReadOnly(ctx, r.m.Store.Path, 5*time.Second)
	if err != nil {
		return nil, false
	}
	return read, true
}

// guardRelationship is the refusal code for a relationship that is not active, is in another generation or
// no longer binds the parent, the child, the roots, the recipients and the scope the request named; empty
// when it is as registered.
func (r *startRun) guardRelationship(ctx context.Context, read *store.ReadOnly) string {
	var status, issue, parentTask, parentHost, childTask, childHost, scope, rootsJSON, recipientsJSON string
	var generation int64
	err := read.QueryRowContext(ctx, "SELECT status,issue_key,parent_task_id,parent_host_id,child_task_id,child_host_id,scope_ref,artifact_roots,allowed_recipients,execution_generation FROM relationships WHERE relationship_id=?", r.row.RelationshipID.String).Scan(&status, &issue, &parentTask, &parentHost, &childTask, &childHost, &scope, &rootsJSON, &recipientsJSON, &generation)
	if err != nil || status != "active" {
		return "relationship_not_active"
	}
	if generation != r.row.ExecutionGeneration.Int64 {
		return "stale_generation"
	}
	var roots, allowed []string
	if json.Unmarshal([]byte(rootsJSON), &roots) != nil || json.Unmarshal([]byte(recipientsJSON), &allowed) != nil {
		return "managed_scope_changed"
	}
	parent := obj(r.req["parent"])
	if issue != r.identity.IssueKey || parentTask != str(parent["taskId"]) || parentHost != str(parent["hostId"]) || childTask != r.task || childHost != str(obj(r.req["child"])["hostId"]) || scope != str(r.req["scopeRef"]) || !jsonSame(roots, stringsOf(r.req["artifactRoots"])) || !jsonSame(allowed, r.recipients) {
		return "managed_scope_changed"
	}
	return ""
}

// guardGeneration is the refusal code for a generation whose dispatch is no longer this start's standby
// turn and request, or whose verification mode is no longer managed; empty when they are.
func (r *startRun) guardGeneration(ctx context.Context, read *store.ReadOnly) string {
	var dispatchTurn, dispatchID string
	err := read.QueryRowContext(ctx, "SELECT dispatch_turn_id,dispatch_request_id FROM generations WHERE relationship_id=? AND execution_generation=?", r.row.RelationshipID.String, r.row.ExecutionGeneration.Int64).Scan(&dispatchTurn, &dispatchID)
	if err != nil || dispatchTurn != r.standby || dispatchID != r.identity.DispatchRequestID {
		return "managed_identity_changed"
	}
	var mode string
	if read.QueryRowContext(ctx, "SELECT mode FROM verification_mode WHERE relationship_id=?", r.row.RelationshipID.String).Scan(&mode) != nil || mode != "managed" {
		return "managed_criteria_changed"
	}
	return ""
}

// guardSettings is the refusal code for a parent or child whose recorded settings are not the ones the
// request carried; empty when both are.
func (r *startRun) guardSettings(ctx context.Context, read *store.ReadOnly) string {
	for _, role := range []string{"parent", "child"} {
		who := r.task
		if role == "parent" {
			who = str(obj(r.req["parent"])["taskId"])
		}
		var current string
		if read.QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id=?", who).Scan(&current) != nil {
			return "managed_settings_changed"
		}
		var recorded any
		if json.Unmarshal([]byte(current), &recorded) != nil {
			return "managed_settings_changed"
		}
		if !jsonSame(recorded, settingsWithRole(obj(obj(r.req[role])["settings"]), role)) {
			return "managed_settings_changed"
		}
	}
	return ""
}

// guardCriteria is the refusal code for criteria that are no longer the request's set, from its source, with
// its digest; empty when they are.
func (r *startRun) guardCriteria(ctx context.Context, read *store.ReadOnly) string {
	criteriaRows, e := read.QueryContext(ctx, "SELECT criterion_id,title,required,source_ref,set_digest FROM canonical_criteria WHERE relationship_id=?", r.row.RelationshipID.String)
	if e != nil {
		return "managed_criteria_changed"
	}
	type criterion struct {
		id, title, source, digest string
		required                  int64
	}
	var found []criterion
	for criteriaRows.Next() {
		var entry criterion
		if e = criteriaRows.Scan(&entry.id, &entry.title, &entry.required, &entry.source, &entry.digest); e != nil {
			criteriaRows.Close()
			return "managed_criteria_changed"
		}
		found = append(found, entry)
	}
	e = criteriaRows.Err()
	criteriaRows.Close()
	if e != nil {
		return "managed_criteria_changed"
	}
	normal, e := delivery.NormaliseCriteria(criteriaEntries(r.req))
	if e != nil || len(normal) != len(found) {
		return "managed_criteria_changed"
	}
	digest := delivery.SetDigest(normal)
	for _, item := range found {
		match := false
		for _, expected := range normal {
			if item.id == expected.ID && item.title == expected.Title && ((item.required == 1) == expected.Required) {
				match = true
			}
		}
		if !match || item.source != str(r.req["criteriaSource"]) || item.digest != digest {
			return "managed_criteria_changed"
		}
	}
	return ""
}
