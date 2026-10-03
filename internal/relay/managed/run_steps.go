package managed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// startRun is the state of one Start.Run: what the request and the host's ledger say, the reservation row
// as it stands now, and what each step learned for the next. Every step is a method that reads and writes
// it; Run lists the steps in the order they run.
//
// row is always the latest the store answered with (the lookup, then each of Reserve, Arm, Receipt and the
// refresh after registration): the answers and the final guard read it when they run, not as it was when
// the step began.
type startRun struct {
	m           *Start
	req, ledger map[string]any
	identity    Identity
	assignment  string
	physical    store.Location
	reservation Reservation
	row         store.ManagedStartRequestsRow

	// unlock lets go of the request's lock; nil until the lock is taken.
	unlock func() error
	// projectLock lets go of the project's lock, held by createChild from before the creation until
	// register has registered the child; never nil, may be called more than once.
	projectLock func()

	receipt       map[string]any // the host's creation receipt, with what the standby recovery added to it
	task, standby string         // the child's task id and its standby turn
	parent        map[string]any // the request's parent as it was when the child was registered
	childSettings map[string]any
	recipients    []string
	reg           *registry.Registry
	record        registry.Relationship
	sent          map[string]any // the host's receipt for the business send
	turn          string         // the business turn the host accepted

	attempt    int             // the creation attempt now current: 0 is the request's own operation (reconcile.go)
	adopted    bool            // the receipt is an unknown creation continued on the thread the App Server showed
	reconciled *reconciliation // what reconcileCreation concluded, when it ran
}

// A step settles the request or hands over to the next one. It settles it with the answer Observe gives
// (which can come with an error, when the receipt could not be journaled) or with an error alone; it hands
// over by returning nil and nil.

// begin reads the request and fixes its identity before anything is reserved: the request, the ledger the
// host runs on, the request's fingerprint and the row an earlier try left, which must be this request's.
func (m *Start) begin(ctx context.Context, raw []byte) (*startRun, error) {
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
	run := &startRun{m: m, req: req, ledger: ledger, identity: identity, assignment: delivery.AssignmentID(identity.DispatchRequestID), reservation: Reservation{Store: m.Store, Now: m.now}, projectLock: func() {}}
	row, err := m.Store.ManagedStartRequest(ctx, identity.RequestID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && row.RequestFingerprint != identity.Fingerprint {
		return nil, refusal("relationship_conflict", "managed request id belongs to different input or selectors")
	}
	run.row = row
	run.physical, err = m.Store.Locate(ctx)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// finish lets go of the project's lock and then of the request's, on every way out of Run.
func (r *startRun) finish() {
	r.projectLock()
	if r.unlock != nil {
		_ = r.unlock()
	}
}

// result is the receipt the request has so far, with the ledger the host runs on.
func (r *startRun) result() startResult {
	res := NewStartResult(r.identity, r.row, r.assignment, store.PathlibParent(r.m.Store.Path))
	if r.reconciled != nil {
		res.Reconciliation = r.reconciled.object()
	}
	if r.row.RequestID == "" {
		res.Revision = nil
		res.ReservationState = nil
	}
	res.Ledger = contract.OrderedObject{}
	for _, key := range []string{"path", "realPath", "device", "inode"} {
		if value, ok := r.ledger[key]; ok {
			res.Ledger = append(res.Ledger.(contract.OrderedObject), contract.Field{Key: key, Value: value})
		}
	}
	return res
}

// answer journals and returns the receipt for a request that stops here.
func (r *startRun) answer(ctx context.Context, state, stage, reason string) (contract.OrderedObject, error) {
	return r.result().Observe(ctx, r.m.Store, r.m.now(), state, stage, reason)
}

// preflight refuses a request whose worker policy is not ready, before anything is reserved.
func (r *startRun) preflight(ctx context.Context) (contract.OrderedObject, error) {
	readiness, err := r.m.ready(ctx, r.req)
	if err != nil {
		return nil, err
	}
	if readiness != "" {
		return r.answer(ctx, "refused", "preflight", readiness)
	}
	return nil, nil
}

// reserve takes the request's lock and its reservation. A request that was attached to its child by an
// earlier try is checked against what is registered now before it goes on.
func (r *startRun) reserve(ctx context.Context) (contract.OrderedObject, error) {
	unlock, err := Lock(r.m.Store.Path, r.identity.RequestID)
	if err != nil {
		return nil, err
	}
	r.unlock = unlock
	r.row, err = r.reservation.Reserve(ctx, r.identity)
	if err != nil {
		return nil, err
	}
	if r.row.State == "attached" {
		problem, err := r.m.registeredProblem(ctx, r.identity, r.row, r.req, recipientsWith(r.req, r.row.ChildTaskID.String), false)
		if err != nil {
			return nil, err
		}
		if problem != "" {
			return r.answer(ctx, "refused", "replay", problem)
		}
	}
	return nil, nil
}

// declareIntent publishes the intent that exists before the child does; the same dispatch with other
// terms is refused.
func (r *startRun) declareIntent(ctx context.Context) (contract.OrderedObject, error) {
	declared, err := delivery.DeclareIntent(ctx, r.identity.MarkerRoot, delivery.IntentDeclaration{Workspace: r.identity.Workspace, DispatchRequestID: r.identity.DispatchRequestID, IssueKey: r.identity.IssueKey, DeclaredAt: r.m.now(), CriteriaSource: r.req["criteriaSource"], BaselineRevision: r.req["baselineRevision"], AuthorizedSettings: deliveryValue(pyjson.Map(pyjson.Map(r.req["child"])["settings"])), DBPath: r.m.Store.Path})
	if err != nil {
		return nil, err
	}
	if declared.Get("outcome") == delivery.Conflict {
		return r.answer(ctx, "refused", "intent", "intent_conflict")
	}
	return nil, nil
}

// decideScope is the project-scope decision for a request that is only reserved, and then arms it.
func (r *startRun) decideScope(ctx context.Context) (contract.OrderedObject, error) {
	if r.row.State != "reserved" {
		return nil, nil
	}
	// A reserved request has armed nothing, so no host effect can exist yet: one whose project scope
	// will be refused stops here, and managed-release can still release its reservation.
	if err := r.m.scopeRefusal(ctx, r.identity, r.req); err != nil {
		return nil, err
	}
	var err error
	r.row, err = r.reservation.Arm(ctx, r.identity.RequestID, r.identity.Fingerprint, r.row.Revision)
	return nil, err
}

// createChild reads the creation receipt and, when the host holds none, asks it to create the child. A start
// that creates holds the project's lock (create) until its child is registered; the lock is let go by
// register on the way through and by finish on every other way out.
func (r *startRun) createChild(ctx context.Context) (contract.OrderedObject, error) {
	if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
		return nil, err
	}
	receipt, err := r.currentCreation(ctx)
	if err != nil {
		return nil, err
	}
	r.receipt = receipt
	if receipt == nil || receipt["status"] == "not_attempted" {
		var notReady string
		r.receipt, notReady, r.projectLock, err = r.m.create(ctx, r.attemptIdentity(), r.req, r.ledger)
		if err != nil {
			return nil, err
		}
		if notReady != "" {
			return r.answer(ctx, "refused", "creation", notReady)
		}
	}
	return nil, nil
}

// retryStandby is the retry of a creation that failed after its thread started, or that was unknown and reconcileCreation continued on the
// thread it found: the standby turn is sent to that thread, and the receipt says what came of it.
func (r *startRun) retryStandby(ctx context.Context) (contract.OrderedObject, error) {
	if pyjson.Text(r.receipt["status"]) != "failed" && !r.adopted {
		return nil, nil
	}
	// The title is set once the standby is accepted, and not again once the receipt is recorded: a replay of an admitted start sets nothing.
	receipt, err := r.m.recoverStandby(ctx, r.identity, r.req, r.ledger, r.physical, r.receipt, standbyRecovery{attempt: r.attempt, adopted: r.adopted, rename: r.row.ReceiptStatus.String != "accepted"})
	if err != nil {
		return nil, err
	}
	r.receipt = receipt
	return nil, nil
}

// incompleteCreation answers a start whose creation did not end accepted: the attempt is recorded and the
// thread, if one was left, is retained for the retry.
func (r *startRun) incompleteCreation(ctx context.Context) (contract.OrderedObject, error) {
	receipt := r.receipt
	if pyjson.Text(receipt["status"]) == "accepted" {
		return nil, nil
	}
	outcome := "unknown"
	if receipt != nil && receipt["status"] == "failed" {
		outcome = "failed"
	}
	var attemptedTask any
	if receipt != nil {
		attemptedTask = receipt["threadId"]
	}
	// The attempt marker records the retained thread for the retry, and is written when the creation ended
	// unknown or failed, which includes a creation the caller cancelled: it does not end with that context.
	if _, err := delivery.RecordAttempt(context.WithoutCancel(ctx), r.identity.MarkerRoot, r.identity.Workspace, r.assignment, outcome, r.m.now(), attemptedTask); err != nil {
		return nil, err
	}
	incomplete := r.result()
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
	return incomplete.Observe(ctx, r.m.Store, r.m.now(), "incomplete", "creation", "creation_"+outcome)
}

// verifyCreation takes the child's identity from the accepted receipt, checks that the host created it
// with the settings asked for, and records the receipt on the reservation.
func (r *startRun) verifyCreation(ctx context.Context) (contract.OrderedObject, error) {
	task, standby := pyjson.Text(r.receipt["threadId"]), pyjson.Text(r.receipt["turnId"])
	if !delivery.ValidSegment(task) || !delivery.ValidSegment(standby) {
		return r.answer(ctx, "incomplete", "creation", "creation_identity_unobserved")
	}
	r.task, r.standby = task, standby
	r.childSettings = pyjson.Map(pyjson.Map(r.req["child"])["settings"])
	if !creationMatches(r.childSettings, pyjson.Map(r.receipt["creation"])) {
		return r.answer(ctx, "refused", "creation", "creation_settings_unverified")
	}
	var err error
	r.row, err = r.reservation.Receipt(ctx, r.identity.RequestID, r.identity.Fingerprint, r.receipt)
	return nil, err
}

// bindMarker binds the assignment's marker to the child; a marker bound to another task refuses it.
func (r *startRun) bindMarker(ctx context.Context) (contract.OrderedObject, error) {
	bound, err := delivery.BindIdentity(ctx, r.identity.MarkerRoot, r.identity.Workspace, r.assignment, r.task, r.task, r.m.now())
	if err != nil {
		return nil, err
	}
	if bound.Get("outcome") == delivery.Conflict {
		return r.answer(ctx, "refused", "binding", "marker_identity_conflict")
	}
	return nil, nil
}

// register registers the child under the parent. The project's lock is let go right after Register, before
// its error is looked at, so it is never held past the registration however that ended.
func (r *startRun) register(ctx context.Context) (contract.OrderedObject, error) {
	r.parent = pyjson.Map(r.req["parent"])
	r.recipients = recipientsWith(r.req, r.task)
	r.reg = &registry.Registry{Store: r.m.Store, Now: r.m.now}
	record, err := r.reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: pyjson.Text(r.parent["taskId"]), HostID: pyjson.Text(r.parent["hostId"]), Cwd: nullableSQL(pyjson.Text(pyjson.Map(r.parent["settings"])["cwd"]))}, Child: registry.Endpoint{TaskID: r.task, HostID: pyjson.Text(pyjson.Map(r.req["child"])["hostId"]), Cwd: nullableSQL(r.identity.Workspace), CXCSession: nullableSQL(r.task)}, IssueKey: r.identity.IssueKey, ArtifactRoots: stringsOf(r.req["artifactRoots"]), AllowedRecipients: r.recipients, ScopeRef: nullableSQL(pyjson.Text(r.req["scopeRef"])), DispatchRequestID: r.identity.DispatchRequestID, DispatchTurnID: nullableSQL(r.standby), ProjectKey: pyjson.Text(r.req["projectKey"]), ManagedRequestID: r.identity.RequestID})
	r.projectLock()
	if err != nil {
		return nil, err
	}
	r.record = record
	r.row, err = r.m.Store.ManagedStartRequest(ctx, r.identity.RequestID)
	return nil, err
}

// registerCriteria records the criteria the child will be judged by.
func (r *startRun) registerCriteria(ctx context.Context) (contract.OrderedObject, error) {
	criteria := &delivery.Criteria{Store: r.m.Store, Clock: managedClock{now: r.m.now}}
	_, err := criteria.EnsureRegistered(ctx, r.record.ID, criteriaEntries(r.req), r.req["criteriaSource"])
	return nil, err
}

// authorizeSettings records the settings the parent and the child were started with.
func (r *startRun) authorizeSettings(ctx context.Context) (contract.OrderedObject, error) {
	for _, entry := range []struct {
		task, role string
		settings   map[string]any
	}{{pyjson.Text(r.parent["taskId"]), "parent", pyjson.Map(r.parent["settings"])}, {r.task, "child", r.childSettings}} {
		if _, err := EnsureSettings(ctx, r.m.Store, entry.task, settingsWithRole(entry.settings, entry.role), "managed_start", r.m.now()); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// registerRelationship publishes the registration in the assignment's marker.
func (r *startRun) registerRelationship(ctx context.Context) (contract.OrderedObject, error) {
	_, err := delivery.RegisterRelationship(ctx, r.identity.MarkerRoot, r.identity.Workspace, r.assignment, r.record.ID, r.identity.DispatchRequestID, r.m.now(), r.m.Store.Path)
	return nil, err
}

// readback reads the registration back and refuses a start whose relationship is not what the request
// asked for.
func (r *startRun) readback(ctx context.Context) (contract.OrderedObject, error) {
	problem, err := r.m.registeredProblem(ctx, r.identity, r.row, r.req, r.recipients, true)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return r.answer(ctx, "refused", "readback", problem)
	}
	return nil, nil
}

// businessTurn sends the child its assignment, once. The host's receipt for the send is read first: a
// request whose business send was attempted before is not sent again, whatever came of it.
func (r *startRun) businessTurn(ctx context.Context) (contract.OrderedObject, error) {
	if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
		return nil, err
	}
	sent, err := r.m.Adapter.GetOperation(ctx, r.identity.DispatchRequestID)
	if err != nil {
		return nil, err
	}
	r.sent = sent
	if sent != nil && sent["status"] != "not_attempted" {
		return nil, nil
	}
	if answer, err := r.awaitStandby(ctx); answer != nil || err != nil {
		return answer, err
	}
	return nil, r.sendBusiness(ctx)
}

// lifecycleChecker is what an adapter that knows the child's lifecycle answers: whether it may be sent to,
// and if not, why.
type lifecycleChecker interface {
	Lifecycle(context.Context, string, string) (bool, string, error)
}

// awaitStandby waits for a known end of the inert standby. An interrupted or failed
// standby keeps its original anchor; its business send also needs a ready host.
func (r *startRun) awaitStandby(ctx context.Context) (contract.OrderedObject, error) {
	turn, err := r.m.Adapter.ReadTurn(ctx, r.task, r.standby)
	if err != nil {
		return nil, err
	}
	if turn == nil || (turn.Status != "completed" && turn.Status != "interrupted" && turn.Status != "failed") {
		return r.answer(ctx, "incomplete", "standby", "standby_incomplete")
	}
	if check, ok := r.m.Adapter.(lifecycleChecker); ok {
		may, reason, err := check.Lifecycle(ctx, r.task, r.identity.Workspace)
		if err != nil {
			return nil, err
		}
		if !may {
			return r.answer(ctx, "incomplete", "business", reason)
		}
	}
	readiness, err := r.m.ready(ctx, r.req)
	if err != nil {
		return nil, err
	}
	if readiness != "" {
		return r.answer(ctx, "refused", "business", readiness)
	}
	if turn.Status != "completed" {
		// Observe before consuming the business operation id. The final guard still
		// rechecks host readiness; neither observation is an atomic turn reservation.
		code, err := hostReady(ctx, r.m.Adapter, r.task)
		if err != nil {
			return nil, err
		}
		if code != "" {
			return r.answer(ctx, "incomplete", "business", code)
		}
	}
	return nil, nil
}

// sendBusiness asks the host to start the business turn. The host calls businessGuard just before it does.
func (r *startRun) sendBusiness(ctx context.Context) error {
	if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
		return err
	}
	var err error
	r.sent, err = r.m.Adapter.SendMessage(ctx, SendRequest{RequestID: r.identity.DispatchRequestID, ThreadID: r.task, Message: r.m.packet(r.identity, r.row, r.req, r.assignment), Settings: r.childSettings, GuardRPCRequests: 10, BeforeStart: r.businessGuard})
	return err
}

// checkBusiness answers a start whose business send did not end accepted for this child.
func (r *startRun) checkBusiness(ctx context.Context) (contract.OrderedObject, error) {
	if r.sent == nil || r.sent["status"] != "accepted" {
		status := "unknown"
		if r.sent != nil {
			if value, present := r.sent["status"]; present {
				status = pyvalue.Str(value)
			}
		}
		return r.answer(ctx, "incomplete", "business", "business_"+status)
	}
	r.turn = pyjson.Text(r.sent["turnId"])
	if !delivery.ValidSegment(r.turn) || pyjson.Text(r.sent["threadId"]) != r.task {
		return r.answer(ctx, "incomplete", "business", "business_identity_unobserved")
	}
	return nil, nil
}

// confirmRegistration reads the registration back once more now that the business turn is accepted.
func (r *startRun) confirmRegistration(ctx context.Context) (contract.OrderedObject, error) {
	problem, err := r.m.registeredProblem(ctx, r.identity, r.row, r.req, r.recipients, true)
	if err != nil {
		return nil, err
	}
	if problem != "" {
		return r.answer(ctx, "incomplete", "business_accepted", problem)
	}
	return nil, nil
}

// admit records the business turn as admitted for the child and answers with it.
func (r *startRun) admit(ctx context.Context) (contract.OrderedObject, error) {
	if err := r.reg.AdmitExplicitly(ctx, r.record.ID, r.row.ExecutionGeneration.Int64, r.turn, pyjson.Text(r.parent["taskId"]), "managed business dispatch confirmed by its retained bridge receipt"); err != nil {
		return nil, err
	}
	admitted := r.result()
	admitted.BusinessTurnID = r.turn
	return admitted.Observe(ctx, r.m.Store, r.m.now(), "admitted", "business_accepted", "")
}

// criteriaEntries is the request's criteria as the delivery package takes them.
func criteriaEntries(req map[string]any) []any {
	entries := make([]any, 0)
	for _, item := range req["criteria"].([]any) {
		v := pyjson.Map(item)
		entries = append(entries, delivery.Obj{{Key: "id", Value: v["id"]}, {Key: "title", Value: v["title"]}, {Key: "required", Value: v["required"]}})
	}
	return entries
}

// recipientsWith is the request's allowed recipients with the child added when it is not among them.
func recipientsWith(req map[string]any, task string) []string {
	recipients := stringsOf(req["allowedRecipients"])
	for _, who := range recipients {
		if who == task {
			return recipients
		}
	}
	return append(recipients, task)
}

// settingsWithRole is a copy of settings with the role they are recorded under; settings is not changed.
func settingsWithRole(settings map[string]any, role string) map[string]any {
	out := make(map[string]any, len(settings)+1)
	for key, value := range settings {
		out[key] = value
	}
	out["citedRole"] = role
	return out
}
