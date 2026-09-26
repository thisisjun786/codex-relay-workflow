package managed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const bootstrap = "This is a managed-start standby turn, not an implementation assignment. Do not use tools, change files, initialize a workflow or create a goal. End this turn now. The registered assignment will arrive in a separate turn."

// Start coordinates one durable managed request. The adapter must preserve bridge
// operation receipts; an uncertain receipt is never retried under a new identity.
type Start struct {
	Store                             *store.Store
	Adapter                           Adapter
	Now                               func() string
	Socket, MarkerRoot, StateSelector string
	// Readiness returns the same worker policy refusal code as rolepolicy.worker_readiness,
	// or empty when the role pair is ready; it is called again before each host effect.
	Readiness func(context.Context, map[string]any) (string, error)
}

type managedClock struct{ now func() string }

func (c managedClock) ISO() string  { return c.now() }
func (c managedClock) Now() float64 { return float64(time.Now().Unix()) }
func (m *Start) now() string {
	if m.Now != nil {
		return m.Now()
	}
	return registry.SystemISO()
}
func (m *Start) ready(ctx context.Context, req map[string]any) (string, error) {
	if m.Readiness == nil {
		return "caller_policy_unconfigured", nil
	}
	return m.Readiness(ctx, req)
}
func obj(value any) map[string]any { result, _ := value.(map[string]any); return result }
func str(value any) string         { result, _ := value.(string); return result }
func stringsOf(values any) []string {
	out := []string{}
	for _, v := range values.([]any) {
		out = append(out, v.(string))
	}
	return out
}
func nullableSQL(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
func (m *Start) Run(ctx context.Context, raw []byte) (contract.OrderedObject, error) {
	req, err := ParseRequest(raw)
	if err != nil {
		return nil, err
	}
	if m.Adapter == nil {
		return nil, fmt.Errorf("managed start requires an observed bridge ledger identity")
	}
	ledger, err := m.Adapter.LedgerIdentityRecord(ctx)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"realPath", "device", "inode"} {
		if ledger == nil || ledger[key] == nil {
			return nil, fmt.Errorf("managed start requires an observed bridge ledger identity")
		}
	}
	if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, err
	}
	identity, err := RequestIdentity(ctx, req, m.Store, m.Socket, m.MarkerRoot, m.StateSelector, ledger)
	if err != nil {
		return nil, err
	}
	assignment := delivery.AssignmentID(identity.DispatchRequestID)
	reservation := Reservation{Store: m.Store, Now: m.now}
	row, err := m.Store.ManagedStartRequest(ctx, identity.RequestID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && row.RequestFingerprint != identity.Fingerprint {
		return nil, refusal("relationship_conflict", "managed request id belongs to different input or selectors")
	}
	physical, err := m.Store.Locate(ctx)
	if err != nil {
		return nil, err
	}
	result := func(row store.ManagedStartRequestsRow) startResult {
		r := NewStartResult(identity, row, assignment, filepath.Dir(m.Store.Path))
		if row.RequestID == "" {
			r.Revision = nil
			r.ReservationState = nil
		}
		r.Ledger = contract.OrderedObject{}
		for _, key := range []string{"path", "realPath", "device", "inode"} {
			if value, ok := ledger[key]; ok {
				r.Ledger = append(r.Ledger.(contract.OrderedObject), contract.Field{Key: key, Value: value})
			}
		}
		return r
	}
	answer := func(row store.ManagedStartRequestsRow, state, stage, reason string) (contract.OrderedObject, error) {
		return result(row).Observe(ctx, m.Store, m.now(), state, stage, reason)
	}
	readiness, err := m.ready(ctx, req)
	if err != nil {
		return nil, err
	}
	if readiness != "" {
		return answer(row, "refused", "preflight", readiness)
	}
	release, err := Lock(m.Store.Path, identity.RequestID)
	if err != nil {
		return nil, err
	}
	defer release()
	row, err = reservation.Reserve(ctx, identity)
	if err != nil {
		return nil, err
	}
	if row.State == "attached" {
		recipients := stringsOf(req["allowedRecipients"])
		task := row.ChildTaskID.String
		present := false
		for _, v := range recipients {
			if v == task {
				present = true
			}
		}
		if !present {
			recipients = append(recipients, task)
		}
		problem, e := m.registeredProblem(ctx, identity, row, req, recipients, false)
		if e != nil {
			return nil, e
		}
		if problem != "" {
			return answer(row, "refused", "replay", problem)
		}
	}
	declared, err := delivery.DeclareIntent(identity.MarkerRoot, delivery.IntentDeclaration{Workspace: identity.Workspace, DispatchRequestID: identity.DispatchRequestID, IssueKey: identity.IssueKey, DeclaredAt: m.now(), CriteriaSource: req["criteriaSource"], BaselineRevision: req["baselineRevision"], AuthorizedSettings: deliveryValue(obj(obj(req["child"])["settings"])), DBPath: m.Store.Path})
	if err != nil {
		return nil, err
	}
	if field(declared, "outcome") == delivery.Conflict {
		return answer(row, "refused", "intent", "intent_conflict")
	}
	if row.State == "reserved" {
		row, err = reservation.Arm(ctx, identity.RequestID, identity.Fingerprint, row.Revision)
		if err != nil {
			return nil, err
		}
	}
	if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, err
	}
	receipt, err := m.Adapter.GetOperation(ctx, identity.CreateRequestID)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt["status"] == "not_attempted" {
		readiness, err = m.ready(ctx, req)
		if err != nil {
			return nil, err
		}
		if readiness != "" {
			return answer(row, "refused", "creation", readiness)
		}
		child := obj(req["child"])
		settings := obj(child["settings"])
		if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
			return nil, err
		}
		sandbox := obj(settings["sandbox"])
		kind := map[string]string{"workspaceWrite": "workspace-write", "readOnly": "read-only", "dangerFullAccess": "danger-full-access"}[str(sandbox["type"])]
		receipt, err = m.Adapter.CreateThread(ctx, CreateThreadRequest{RequestID: identity.CreateRequestID, CWD: str(settings["cwd"]), Prompt: bootstrap, Title: str(child["title"]), Sandbox: kind, Model: str(settings["model"]), ReasoningEffort: str(settings["reasoningEffort"]), RuntimeWorkspaceRoots: stringsOf(settings["runtimeWorkspaceRoots"]), ExpectedSandboxPolicy: sandbox, Role: "child"})
		if err != nil {
			return nil, err
		}
	}
	if str(receipt["status"]) == "failed" {
		receipt, err = m.recoverStandby(ctx, identity, req, ledger, physical, receipt)
		if err != nil {
			return nil, err
		}
	}
	if str(receipt["status"]) != "accepted" {
		outcome := "unknown"
		if receipt != nil && receipt["status"] == "failed" {
			outcome = "failed"
		}
		var attemptedTask any
		if receipt != nil {
			attemptedTask = receipt["threadId"]
		}
		_, err = delivery.RecordAttempt(identity.MarkerRoot, identity.Workspace, assignment, outcome, m.now(), attemptedTask)
		if err != nil {
			return nil, err
		}
		incomplete := result(row)
		var retained any
		if receipt != nil {
			retained = receipt["threadId"]
		}
		recovery := contract.OrderedObject{}
		for _, key := range []string{"recoveryReason", "recoveryRequestId", "recoveryStatus"} {
			if receipt != nil {
				if value, ok := receipt[key]; ok {
					recovery = append(recovery, contract.Field{Key: key, Value: value})
				}
			}
		}
		incomplete.Observed = contract.OrderedObject{{Key: "retainedChildTaskId", Value: retained}, {Key: "standbyRecovery", Value: recovery}}
		return incomplete.Observe(ctx, m.Store, m.now(), "incomplete", "creation", "creation_"+outcome)
	}
	task, standby := str(receipt["threadId"]), str(receipt["turnId"])
	if !delivery.ValidSegment(task) || !delivery.ValidSegment(standby) {
		return answer(row, "incomplete", "creation", "creation_identity_unobserved")
	}
	childSettings := obj(obj(req["child"])["settings"])
	if !creationMatches(childSettings, obj(receipt["creation"])) {
		return answer(row, "refused", "creation", "creation_settings_unverified")
	}
	row, err = reservation.Receipt(ctx, identity.RequestID, identity.Fingerprint, receipt)
	if err != nil {
		return nil, err
	}
	bound, err := delivery.BindIdentity(identity.MarkerRoot, identity.Workspace, assignment, task, task, m.now())
	if err != nil {
		return nil, err
	}
	if field(bound, "outcome") == delivery.Conflict {
		return answer(row, "refused", "binding", "marker_identity_conflict")
	}
	parent := obj(req["parent"])
	recipients := stringsOf(req["allowedRecipients"])
	found := false
	for _, who := range recipients {
		if who == task {
			found = true
		}
	}
	if !found {
		recipients = append(recipients, task)
	}
	reg := &registry.Registry{Store: m.Store, Now: m.now}
	record, err := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: str(parent["taskId"]), HostID: str(parent["hostId"]), Cwd: nullableSQL(str(obj(parent["settings"])["cwd"]))}, Child: registry.Endpoint{TaskID: task, HostID: str(obj(req["child"])["hostId"]), Cwd: nullableSQL(identity.Workspace), CXCSession: nullableSQL(task)}, IssueKey: identity.IssueKey, ArtifactRoots: stringsOf(req["artifactRoots"]), AllowedRecipients: recipients, ScopeRef: nullableSQL(str(req["scopeRef"])), DispatchRequestID: identity.DispatchRequestID, DispatchTurnID: nullableSQL(standby), ProjectKey: str(req["projectKey"]), ManagedRequestID: identity.RequestID})
	if err != nil {
		return nil, err
	}
	row, err = m.Store.ManagedStartRequest(ctx, identity.RequestID)
	if err != nil {
		return nil, err
	}
	criteria := &delivery.Criteria{Store: m.Store, Clock: managedClock{now: m.now}}
	entries := make([]any, 0)
	for _, item := range req["criteria"].([]any) {
		v := obj(item)
		entries = append(entries, delivery.Obj{{Key: "id", Value: v["id"]}, {Key: "title", Value: v["title"]}, {Key: "required", Value: v["required"]}})
	}
	if _, err = criteria.EnsureRegistered(ctx, record.ID, entries, req["criteriaSource"]); err != nil {
		return nil, err
	}
	for _, entry := range []struct {
		task, role string
		settings   map[string]any
	}{{str(parent["taskId"]), "parent", obj(parent["settings"])}, {task, "child", childSettings}} {
		settings := map[string]any{}
		for key, v := range entry.settings {
			settings[key] = v
		}
		settings["citedRole"] = entry.role
		if _, err = EnsureSettings(ctx, m.Store, entry.task, settings, "managed_start", m.now()); err != nil {
			return nil, err
		}
	}
	if _, err = delivery.RegisterRelationship(ctx, identity.MarkerRoot, identity.Workspace, assignment, record.ID, identity.DispatchRequestID, m.now(), m.Store.Path); err != nil {
		return nil, err
	}
	problem, err := m.registeredProblem(ctx, identity, row, req, recipients, true)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return answer(row, "refused", "readback", problem)
	}
	if err = m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, err
	}
	sent, err := m.Adapter.GetOperation(ctx, identity.DispatchRequestID)
	if err != nil {
		return nil, err
	}
	if sent == nil || sent["status"] == "not_attempted" {
		turn, err := m.Adapter.ReadTurn(ctx, task, standby)
		if err != nil {
			return nil, err
		}
		if turn == nil || turn.Status != "completed" {
			return answer(row, "incomplete", "standby", "standby_incomplete")
		}
		if check, ok := m.Adapter.(interface {
			Lifecycle(context.Context, string, string) (bool, string, error)
		}); ok {
			may, reason, e := check.Lifecycle(ctx, task, identity.Workspace)
			if e != nil {
				return nil, e
			}
			if !may {
				return answer(row, "incomplete", "business", reason)
			}
		}
		readiness, err = m.ready(ctx, req)
		if err != nil {
			return nil, err
		}
		if readiness != "" {
			return answer(row, "refused", "business", readiness)
		}
		if err = m.Adapter.RequireLedger(ctx, ledger); err != nil {
			return nil, err
		}
		sent, err = m.Adapter.SendMessage(ctx, SendRequest{RequestID: identity.DispatchRequestID, ThreadID: task, Message: m.packet(identity, row, req, assignment), Settings: childSettings, GuardRPCRequests: 10, BeforeStart: func(guardCtx context.Context) (map[string]any, error) {
			refuse := func(code string) (map[string]any, error) {
				return map[string]any{"code": code, "message": "Managed business start withheld: " + code}, nil
			}
			if err := m.Adapter.RequireLedger(guardCtx, ledger); err != nil {
				return refuse("managed_store_changed")
			}
			if code, err := m.ready(guardCtx, req); err != nil {
				return nil, err
			} else if code != "" {
				return refuse(code)
			}
			info, err := os.Stat(m.Store.Path)
			if err != nil {
				return refuse("managed_store_changed")
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(stat.Dev) != physical.Device || stat.Ino != physical.Inode {
				return refuse("managed_store_changed")
			}
			read, err := store.OpenReadOnly(guardCtx, m.Store.Path, 5*time.Second)
			if err != nil {
				return refuse("managed_store_changed")
			}
			defer read.Close()
			var status, issue, parentTask, parentHost, childTask, childHost, scope, rootsJSON, recipientsJSON string
			var generation int64
			err = read.QueryRowContext(guardCtx, "SELECT status,issue_key,parent_task_id,parent_host_id,child_task_id,child_host_id,scope_ref,artifact_roots,allowed_recipients,execution_generation FROM relationships WHERE relationship_id=?", row.RelationshipID.String).Scan(&status, &issue, &parentTask, &parentHost, &childTask, &childHost, &scope, &rootsJSON, &recipientsJSON, &generation)
			if err != nil || status != "active" {
				return refuse("relationship_not_active")
			}
			if generation != row.ExecutionGeneration.Int64 {
				return refuse("stale_generation")
			}
			var roots, allowed []string
			if json.Unmarshal([]byte(rootsJSON), &roots) != nil || json.Unmarshal([]byte(recipientsJSON), &allowed) != nil {
				return refuse("managed_scope_changed")
			}
			if issue != identity.IssueKey || parentTask != str(parent["taskId"]) || parentHost != str(parent["hostId"]) || childTask != task || childHost != str(obj(req["child"])["hostId"]) || scope != str(req["scopeRef"]) || !jsonSame(roots, stringsOf(req["artifactRoots"])) || !jsonSame(allowed, recipients) {
				return refuse("managed_scope_changed")
			}
			var dispatchTurn, dispatchID string
			err = read.QueryRowContext(guardCtx, "SELECT dispatch_turn_id,dispatch_request_id FROM generations WHERE relationship_id=? AND execution_generation=?", row.RelationshipID.String, row.ExecutionGeneration.Int64).Scan(&dispatchTurn, &dispatchID)
			if err != nil || dispatchTurn != standby || dispatchID != identity.DispatchRequestID {
				return refuse("managed_identity_changed")
			}
			var mode string
			if read.QueryRowContext(guardCtx, "SELECT mode FROM verification_mode WHERE relationship_id=?", row.RelationshipID.String).Scan(&mode) != nil || mode != "managed" {
				return refuse("managed_criteria_changed")
			}
			for _, role := range []string{"parent", "child"} {
				who := task
				if role == "parent" {
					who = str(parent["taskId"])
				}
				var current string
				if read.QueryRowContext(guardCtx, "SELECT settings FROM authorized_settings WHERE task_id=?", who).Scan(&current) != nil {
					return refuse("managed_settings_changed")
				}
				var recorded any
				if json.Unmarshal([]byte(current), &recorded) != nil {
					return refuse("managed_settings_changed")
				}
				expected := map[string]any{}
				for k, v := range obj(obj(req[role])["settings"]) {
					expected[k] = v
				}
				expected["citedRole"] = role
				if !jsonSame(recorded, expected) {
					return refuse("managed_settings_changed")
				}
			}
			criteriaRows, e := read.QueryContext(guardCtx, "SELECT criterion_id,title,required,source_ref,set_digest FROM canonical_criteria WHERE relationship_id=?", row.RelationshipID.String)
			if e != nil {
				return refuse("managed_criteria_changed")
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
					return refuse("managed_criteria_changed")
				}
				found = append(found, entry)
			}
			e = criteriaRows.Err()
			criteriaRows.Close()
			if e != nil {
				return refuse("managed_criteria_changed")
			}
			entries := make([]any, 0)
			for _, item := range req["criteria"].([]any) {
				v := obj(item)
				entries = append(entries, delivery.Obj{{Key: "id", Value: v["id"]}, {Key: "title", Value: v["title"]}, {Key: "required", Value: v["required"]}})
			}
			normal, e := delivery.NormaliseCriteria(entries)
			if e != nil || len(normal) != len(found) {
				return refuse("managed_criteria_changed")
			}
			digest := delivery.SetDigest(normal)
			for _, item := range found {
				match := false
				for _, expected := range normal {
					if item.id == expected.ID && item.title == expected.Title && ((item.required == 1) == expected.Required) {
						match = true
					}
				}
				if !match || item.source != str(req["criteriaSource"]) || item.digest != digest {
					return refuse("managed_criteria_changed")
				}
			}
			code, e := hostReady(guardCtx, m.Adapter, task)
			if e != nil {
				return nil, e
			}
			if code != "" {
				return map[string]any{"code": code, "message": "Managed turn withheld: " + code}, nil
			}
			return nil, nil
		}})
		if err != nil {
			return nil, err
		}
	}
	if sent == nil || sent["status"] != "accepted" {
		status := "unknown"
		if sent != nil {
			status = str(sent["status"])
		}
		return answer(row, "incomplete", "business", "business_"+status)
	}
	turn := str(sent["turnId"])
	if !delivery.ValidSegment(turn) || str(sent["threadId"]) != task {
		return answer(row, "incomplete", "business", "business_identity_unobserved")
	}
	problem, err = m.registeredProblem(ctx, identity, row, req, recipients, true)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return answer(row, "incomplete", "business_accepted", problem)
	}
	if err = reg.AdmitExplicitly(ctx, record.ID, row.ExecutionGeneration.Int64, turn, str(parent["taskId"]), "managed business dispatch confirmed by its retained bridge receipt"); err != nil {
		return nil, err
	}
	resultRow := result(row)
	resultRow.BusinessTurnID = turn
	return resultRow.Observe(ctx, m.Store, m.now(), "admitted", "business_accepted", "")
}
func deliveryValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		ordered := delivery.Obj{}
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			ordered = append(ordered, delivery.F{Key: key, Value: deliveryValue(x[key])})
		}
		return ordered
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = deliveryValue(value)
		}
		return out
	default:
		return v
	}
}
func field(o delivery.Obj, key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}
func creationMatches(expected, created map[string]any) bool {
	if created == nil {
		return false
	}
	settings := registry.TaskSettings{Data: deliveryValue(expected).(contract.OrderedObject)}
	observed := deliveryValue(created).(contract.OrderedObject)
	return len(settings.Mismatches(observed, true, true, false)) == 0
}
func (m *Start) recoverStandby(ctx context.Context, id Identity, req map[string]any, ledger map[string]any, physical store.Location, receipt map[string]any) (map[string]any, error) {
	task := str(receipt["threadId"])
	effects, ok := receipt["attemptedEffects"].([]any)
	attemptedStart, attemptedTurn := false, false
	for _, effect := range effects {
		if effect == "thread/start" {
			attemptedStart = true
		}
		if effect == "turn/start" {
			attemptedTurn = true
		}
	}
	settings := obj(obj(req["child"])["settings"])
	if !delivery.ValidSegment(task) || receipt["turnId"] != nil || !ok || !attemptedStart || attemptedTurn || !creationMatches(settings, obj(receipt["creation"])) {
		return receipt, nil
	}
	digest := sha256.Sum256([]byte(id.RequestID))
	recoveryID := fmt.Sprintf("managed-standby-%x", digest)
	if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, err
	}
	recovered, err := m.Adapter.GetOperation(ctx, recoveryID)
	if err != nil {
		return nil, err
	}
	if recovered == nil || recovered["status"] == "not_attempted" {
		if check, ok := m.Adapter.(interface {
			Lifecycle(context.Context, string, string) (bool, string, error)
		}); ok {
			may, reason, e := check.Lifecycle(ctx, task, id.Workspace)
			if e != nil {
				return nil, e
			}
			if !may {
				receipt["recoveryReason"] = reason
				return receipt, nil
			}
		}
		readiness, err := m.ready(ctx, req)
		if err != nil {
			return nil, err
		}
		if readiness != "" {
			receipt["recoveryReason"] = readiness
			return receipt, nil
		}
		if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
			return nil, err
		}
		recovered, err = m.Adapter.SendMessage(ctx, SendRequest{RequestID: recoveryID, ThreadID: task, Message: bootstrap, Settings: settings, GuardRPCRequests: 10, BeforeStart: func(guardCtx context.Context) (map[string]any, error) {
			if err := m.Adapter.RequireLedger(guardCtx, ledger); err != nil {
				return map[string]any{"code": "managed_store_changed", "message": "Standby recovery store changed"}, nil
			}
			if code, e := m.ready(guardCtx, req); e != nil {
				return nil, e
			} else if code != "" {
				return map[string]any{"code": code, "message": "Standby recovery withheld"}, nil
			}
			info, e := os.Stat(m.Store.Path)
			if e != nil {
				return map[string]any{"code": "managed_store_changed", "message": "Standby recovery store changed"}, nil
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || uint64(stat.Dev) != physical.Device || stat.Ino != physical.Inode {
				return map[string]any{"code": "managed_store_changed", "message": "Standby recovery store changed"}, nil
			}
			ro, e := store.OpenReadOnly(guardCtx, m.Store.Path, 5*time.Second)
			if e != nil {
				return map[string]any{"code": "managed_store_changed", "message": "Standby recovery store changed"}, nil
			}
			defer ro.Close()
			var fp, state string
			e = ro.QueryRowContext(guardCtx, "SELECT request_fingerprint,state FROM managed_start_requests WHERE request_id=?", id.RequestID).Scan(&fp, &state)
			if e != nil || fp != id.Fingerprint || state != "create_armed" {
				return map[string]any{"code": "managed_reservation_changed", "message": "Standby recovery reservation changed"}, nil
			}
			return nil, nil
		}})
		if err != nil {
			return nil, err
		}
	}
	receipt["recoveryRequestId"] = recoveryID
	if recovered == nil || recovered["status"] != "accepted" || recovered["threadId"] != task || !delivery.ValidSegment(recovered["turnId"]) {
		receipt["recoveryStatus"] = "unknown"
		if recovered != nil {
			receipt["recoveryStatus"] = recovered["status"]
		}
		return receipt, nil
	}
	combined := make(map[string]any, len(receipt)+4)
	for k, v := range receipt {
		combined[k] = v
	}
	combined["creationStatus"] = receipt["status"]
	combined["status"] = "accepted"
	combined["turnId"] = recovered["turnId"]
	combined["standbyRecovery"] = recovered
	return combined, nil
}
func (m *Start) registeredProblem(ctx context.Context, id Identity, row store.ManagedStartRequestsRow, req map[string]any, recipients []string, complete bool) (string, error) {
	if row.State != "attached" {
		return "reservation_not_attached", nil
	}
	reg := &registry.Registry{Store: m.Store, Now: m.now}
	record, err := reg.Get(ctx, row.RelationshipID.String)
	if err != nil {
		return "", err
	}
	if record.Status != "active" {
		return "relationship_not_active", nil
	}
	if record.Parent.TaskID != str(obj(req["parent"])["taskId"]) || record.Parent.HostID != str(obj(req["parent"])["hostId"]) || record.Child.TaskID != row.ChildTaskID.String || record.Child.HostID != str(obj(req["child"])["hostId"]) || !jsonSame(record.Roots, stringsOf(req["artifactRoots"])) || record.ScopeRef.String != str(req["scopeRef"]) || !jsonSame(record.Recipients, recipients) {
		return "managed_scope_changed", nil
	}
	if record.Generation != row.ExecutionGeneration.Int64 {
		return "stale_generation", nil
	}
	for _, g := range record.Generations {
		if g.Number == record.Generation && (g.DispatchTurnID.String != row.StandbyTurnID.String || g.DispatchRequestID != id.DispatchRequestID) {
			return "managed_identity_changed", nil
		}
	}
	if !complete {
		return "", nil
	}
	criteria := &delivery.Criteria{Store: m.Store, Clock: delivery.SystemClock{}}
	registered, err := criteria.Get(ctx, row.RelationshipID.String)
	if err != nil {
		return "", err
	}
	mode, err := criteria.Mode(ctx, row.RelationshipID.String)
	if err != nil {
		return "", err
	}
	entries := make([]any, 0)
	for _, item := range req["criteria"].([]any) {
		v := obj(item)
		entries = append(entries, delivery.Obj{{Key: "id", Value: v["id"]}, {Key: "title", Value: v["title"]}, {Key: "required", Value: v["required"]}})
	}
	normal, err := delivery.NormaliseCriteria(entries)
	if err != nil {
		return "", err
	}
	if registered == nil || mode != delivery.Managed || field(registered, "setDigest") != delivery.SetDigest(normal) || field(registered, "sourceRef") != req["criteriaSource"] {
		return "managed_criteria_changed", nil
	}
	var storedCriteria []struct {
		ID, Title      string
		Required       int64
		Source, Digest string
	}
	rows, e := m.Store.Querier(ctx).QueryContext(ctx, "SELECT criterion_id,title,required,source_ref,set_digest FROM canonical_criteria WHERE relationship_id=?", row.RelationshipID.String)
	if e != nil {
		return "", e
	}
	for rows.Next() {
		var item struct {
			ID, Title      string
			Required       int64
			Source, Digest string
		}
		if e = rows.Scan(&item.ID, &item.Title, &item.Required, &item.Source, &item.Digest); e != nil {
			rows.Close()
			return "", e
		}
		storedCriteria = append(storedCriteria, item)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", e
	}
	if len(storedCriteria) != len(normal) {
		return "managed_criteria_changed", nil
	}
	for _, item := range storedCriteria {
		found := false
		for _, expected := range normal {
			if item.ID == expected.ID && item.Title == expected.Title && ((item.Required == 1) == expected.Required) {
				found = true
			}
		}
		if !found || item.Source != str(req["criteriaSource"]) || item.Digest != delivery.SetDigest(normal) {
			return "managed_criteria_changed", nil
		}
	}
	for _, role := range []string{"parent", "child"} {
		task := row.ChildTaskID.String
		if role == "parent" {
			task = str(obj(req["parent"])["taskId"])
		}
		var stored string
		e := m.Store.Querier(ctx).QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id=?", task).Scan(&stored)
		if errors.Is(e, sql.ErrNoRows) {
			return "managed_settings_changed", nil
		}
		if e != nil {
			return "", e
		}
		var current any
		if e := json.Unmarshal([]byte(stored), &current); e != nil {
			return "", e
		}
		expected := map[string]any{}
		for k, v := range obj(obj(req[role])["settings"]) {
			expected[k] = v
		}
		expected["citedRole"] = role
		if !jsonSame(current, expected) {
			return "managed_settings_changed", nil
		}
	}
	directory, err := delivery.AssignmentDir(id.MarkerRoot, id.Workspace, delivery.AssignmentID(id.DispatchRequestID))
	if err != nil {
		return "", err
	}
	marker, unreadable := delivery.ReadAssignment(directory)
	if len(unreadable) > 0 || delivery.Malformed(marker) != "" {
		return "managed_marker_unreadable", nil
	}
	bound, _ := field(marker, "bound").(delivery.Obj)
	relationship, _ := field(marker, "relationship").(delivery.Obj)
	if field(bound, "taskId") != row.ChildTaskID.String || field(bound, "sessionId") != row.ChildTaskID.String || field(relationship, "relationshipId") != row.RelationshipID.String {
		return "managed_marker_changed", nil
	}
	return "", nil
}
func (m *Start) packet(id Identity, row store.ManagedStartRequestsRow, req map[string]any, assignment string) string {
	control := map[string]any{"taskId": row.ChildTaskID.String, "standbyTurnId": row.StandbyTurnID.String, "dispatchRequestId": id.DispatchRequestID, "assignmentId": assignment, "relationshipId": row.RelationshipID.String, "executionGeneration": row.ExecutionGeneration.Int64, "state": filepath.Dir(m.Store.Path), "socket": m.Socket, "workspace": id.Workspace, "markerRoot": id.MarkerRoot}
	encoded, _ := compactPythonJSON(control)
	return "Managed assignment routing record:\n" + string(encoded) + "\nFirst publish your own intent-claim using this task, assignment and dispatch request. Publish your own per-turn disposition; continuation claims must name standbyTurnId. Do not fabricate completion, ACK or verification. Report through the registered relay. The following is the authorized business assignment:\n\n" + str(req["prompt"])
}
