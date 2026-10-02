package managed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
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

// Run is one managed start. The request is read and its identity fixed (begin), then each step below runs in
// turn: a step either settles the request, with the answer it journals or with an error, or hands over to
// the next, and a start that no step settles admits its business turn. The steps are in run_steps.go; the
// guard that runs just before the business send is in business_guard.go.
//
// Stage by stage: preflight, reserve and declareIntent make the request exist; decideScope is the project
// scope decision, which arms it; createChild, retryStandby, incompleteCreation and verifyCreation are the
// creation and the retry of its standby turn; bindMarker through readback register the child; businessTurn,
// checkBusiness and confirmRegistration are the standby check and the business turn; admit ends it.
func (m *Start) Run(ctx context.Context, raw []byte) (contract.OrderedObject, error) {
	run, err := m.begin(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer run.finish()
	for _, step := range []func(context.Context) (contract.OrderedObject, error){
		run.preflight,
		run.reserve,
		run.declareIntent,
		run.decideScope,
		run.createChild,
		run.retryStandby,
		run.incompleteCreation,
		run.verifyCreation,
		run.bindMarker,
		run.register,
		run.registerCriteria,
		run.authorizeSettings,
		run.registerRelationship,
		run.readback,
		run.businessTurn,
		run.checkBusiness,
		run.confirmRegistration,
	} {
		if answer, err := step(ctx); answer != nil || err != nil {
			return answer, err
		}
	}
	return run.admit(ctx)
}

// create is the one place a managed start asks the host for its child. Every refusal that can be decided
// without the child is decided here, in the order it was always decided, and the last of them directly
// before the host is asked: readiness, the project scope, the ledger, then the project scope again with
// the request's own fields (requestRefusal). A request that names a project holds the project's lock
// (LockProject) from before the first of these, and the caller keeps it until the child is registered,
// so a writer of the project's parent binding, which takes the lock exclusively, can change the binding
// neither between the last ask and the creation nor between the creation and the registration (where
// registration would refuse a child whose thread already exists).
//
// It returns the host's receipt, or the readiness refusal code that stands in for it, and the function
// that lets the lock go; the function is never nil, may be called more than once, and must be called on
// every way out of the caller.
func (m *Start) create(ctx context.Context, id Identity, req, ledger map[string]any) (map[string]any, string, func(), error) {
	release := func() {}
	if project := str(req["projectKey"]); project != "" {
		held, err := LockProject(ctx, m.Store.Path, project)
		if err != nil {
			return nil, "", release, err
		}
		var once sync.Once
		release = func() { once.Do(func() { _ = held() }) }
	}
	readiness, err := m.ready(ctx, req)
	if err != nil {
		return nil, "", release, err
	}
	if readiness != "" {
		return nil, readiness, release, nil
	}
	// Asked right before the effect, as readiness is: the binding may have moved since the first ask.
	if err := m.scopeRefusal(ctx, id, req); err != nil {
		return nil, "", release, err
	}
	if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, "", release, err
	}
	// The ledger check is an adapter call and takes time: the last ask is the one after it, and nothing but
	// the host call follows it.
	if err := m.scopeRefusal(ctx, id, req); err != nil {
		return nil, "", release, err
	}
	if err := m.requestRefusal(ctx, req); err != nil {
		return nil, "", release, err
	}
	child := obj(req["child"])
	settings := obj(child["settings"])
	sandbox := obj(settings["sandbox"])
	kind := map[string]string{"workspaceWrite": "workspace-write", "readOnly": "read-only", "dangerFullAccess": "danger-full-access"}[str(sandbox["type"])]
	receipt, err := m.Adapter.CreateThread(ctx, CreateThreadRequest{RequestID: id.CreateRequestID, CWD: str(settings["cwd"]), Prompt: bootstrap, Title: str(child["title"]), Sandbox: kind, Model: str(settings["model"]), ReasoningEffort: str(settings["reasoningEffort"]), RuntimeWorkspaceRoots: stringsOf(settings["runtimeWorkspaceRoots"]), ExpectedSandboxPolicy: sandbox, Role: "child"})
	return receipt, "", release, err
}

// requestRefusal is what registration and the settings record refuse once the child exists and the request
// alone decides: a field holding the field separator, then the parent's recorded settings that differ from
// the request's. Each is the refusal registration or EnsureSettings gives, with the same reason, detail
// and error type, asked while the child does not exist. The settings read is the store's answer now;
// EnsureSettings after creation stays the write that decides.
func (m *Start) requestRefusal(ctx context.Context, req map[string]any) error {
	if err := separatorRefusal(req); err != nil {
		return err
	}
	parent := obj(req["parent"])
	return SettingsConflict(ctx, m.Store, str(parent["taskId"]), settingsWithRole(obj(parent["settings"]), "parent"))
}

// scopeRefusal is the project-scope decision Register makes once the child exists, asked while it does
// not. A request that names a project no parent is bound to, or one another task is the parent of, is
// refused with Register's own refusal before anything is created. A request that names no project has
// nothing to decide.
func (m *Start) scopeRefusal(ctx context.Context, id Identity, req map[string]any) error {
	reg := &registry.Registry{Store: m.Store, Now: m.now}
	return reg.PrecheckScope(ctx, str(obj(req["parent"])["taskId"]), id.IssueKey, str(req["projectKey"]))
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
		if check, ok := m.Adapter.(lifecycleChecker); ok {
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
	normal, err := delivery.NormaliseCriteria(criteriaEntries(req))
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
		if !jsonSame(current, settingsWithRole(obj(obj(req[role])["settings"]), role)) {
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
	control := map[string]any{"taskId": row.ChildTaskID.String, "standbyTurnId": row.StandbyTurnID.String, "dispatchRequestId": id.DispatchRequestID, "assignmentId": assignment, "relationshipId": row.RelationshipID.String, "executionGeneration": row.ExecutionGeneration.Int64, "state": store.PathlibParent(m.Store.Path), "socket": m.Socket, "workspace": id.Workspace, "markerRoot": id.MarkerRoot}
	encoded, _ := compactPythonJSON(control)
	// The row's standby turn is the anchor of the row's generation: the send is refused unless
	// generations.dispatch_turn_id of row.ExecutionGeneration is this very turn (the guard in Run), and
	// registration records generation 1, so the text managed-start sends today is generation 1's.
	return routingText(string(encoded), row.ExecutionGeneration.Int64, row.StandbyTurnID.String, str(req["prompt"]))
}

// routingText is the message that carries a child its routing record and its assignment. Its bytes are
// part of what the bridge ledger fingerprints for the send under the dispatch request id, so a retry of a
// request first sent before a change of this text would end in a ledger conflict: generation 1's text is
// frozen (continuationClaim) and the later generations' wording is the only part that varies.
func routingText(record string, generation int64, anchor, prompt string) string {
	return "Managed assignment routing record:\n" + record + "\nFirst publish your own intent-claim using this task, assignment and dispatch request. Publish your own per-turn disposition; " + continuationClaim(generation, anchor) + ". Do not fabricate completion, ACK or verification. Report through the registered relay. The following is the authorized business assignment:\n\n" + prompt
}

// continuationClaim is the instruction naming the turn a continuation claim must name: the anchor of the
// generation the text is for. standbyTurnId in the routing record is the anchor of generation 1 only; a
// later generation is anchored to the turn its revision request arrived in, so naming standbyTurnId there
// is refused as a claim against the wrong anchor.
func continuationClaim(generation int64, anchor string) string {
	if generation > 1 {
		return fmt.Sprintf("continuation claims must name this generation's anchor turn %s (the dispatch turn bound for executionGeneration %d; standbyTurnId anchors generation 1 only)", anchor, generation)
	}
	return "continuation claims must name standbyTurnId"
}
