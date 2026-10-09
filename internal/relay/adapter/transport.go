package adapter

import (
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	bridgesettings "github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

func slicesSort(values []string) { sort.Strings(values) }

func plain(value any) any {
	switch x := value.(type) {
	case contract.OrderedObject:
		m := make(map[string]any, len(x))
		for _, f := range x {
			m[f.Key] = plain(f.Value)
		}
		return m
	case []any:
		v := make([]any, len(x))
		for i, e := range x {
			v[i] = plain(e)
		}
		return v
	default:
		return value
	}
}

// ErrTransportClosing answers a read, HostCall or GetOperation that arrives after Close began.
var ErrTransportClosing = errors.New("the relay transport is shutting down; the read was refused")

// lease is the admission one unit of work holds. It travels in the work's context, so a read the
// work makes while it lives (an in-flight send's guard calling HostCall) joins the work's
// admission instead of arriving as a late read. A context kept past the work, passed through
// context.WithoutCancel or handed to another goroutine carries an ended lease, which admits
// nothing; so does a lease of another transport. ended is guarded by transport.mu and is set
// before the work releases its pending count.
type lease struct {
	t     *transport
	ended bool
}

type leaseKey struct{}

type transport struct {
	adapter    *Adapter
	mu         sync.Mutex
	accepting  bool
	recipients map[string]bool
	pending    sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
}

func newTransport(a *Adapter) *transport {
	ctx, cancel := context.WithCancel(context.Background())
	return &transport{adapter: a, accepting: true, recipients: map[string]bool{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Close ends admission before draining, cancels unfinished work while the ledger is open,
// and only then closes the connection and ledger. Every admitted caller, a write or a read,
// receives an answer: a read that arrives once Close began is refused with ErrTransportClosing.
func (a *Adapter) Close() error {
	t := a.transport
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if !t.accepting {
		t.mu.Unlock()
		<-t.done
		return nil
	}
	t.accepting = false
	t.mu.Unlock()
	drained := make(chan struct{})
	go func() { t.pending.Wait(); close(drained) }()
	timer := time.NewTimer(a.drain)
	select {
	case <-drained:
	case <-timer.C:
		t.cancel()
		<-drained
	}
	timer.Stop()
	t.cancel()
	var rpcErr error
	if closer, ok := a.rpc.(interface{ Close() error }); ok {
		rpcErr = closer.Close()
	}
	ledgerErr := a.ledger.Close()
	close(t.done)
	return errors.Join(rpcErr, ledgerErr)
}

// admit counts one caller in pending and returns a context that Close cancels when the drain
// budget is spent, with the release that ends the count; call release exactly once. It refuses
// once Close began. A read may join (join true) when ctx carries a live lease of this transport:
// it is then nested in work that is itself counted, so Close is still waiting on that work and
// Add cannot race the Wait. A nested caller takes its own count and lease, so a read that
// outlives the work it started in is still collected by Close. A write never joins: a create
// that arrives after Close began is refused whatever context it carries.
func (t *transport) admit(ctx context.Context, join bool) (context.Context, func(), bool) {
	held := &lease{t: t}
	t.mu.Lock()
	nested := false
	if join {
		parent, _ := ctx.Value(leaseKey{}).(*lease)
		nested = parent != nil && parent.t == t && !parent.ended
	}
	if !t.accepting && !nested {
		t.mu.Unlock()
		return nil, nil, false
	}
	t.pending.Add(1)
	t.mu.Unlock()
	run, cancel := context.WithCancel(context.WithValue(ctx, leaseKey{}, held))
	stop := context.AfterFunc(t.ctx, cancel)
	return run, func() {
		stop()
		cancel()
		t.mu.Lock()
		held.ended = true
		t.mu.Unlock()
		t.pending.Done()
	}, true
}

// admitRead admits one read, refusing it with ErrTransportClosing once Close began. An adapter
// built without a ledger has no transport and nothing to close, so its reads run as called.
func (a *Adapter) admitRead(ctx context.Context) (context.Context, func(), error) {
	t := a.transport
	if t == nil {
		return ctx, func() {}, nil
	}
	run, release, ok := t.admit(ctx, true)
	if !ok {
		return nil, nil, ErrTransportClosing
	}
	return run, release, nil
}

func (a *Adapter) SendMessage(ctx context.Context, requestID, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	return a.Send(ctx, requestID, thread, message, settings, nil, 0)
}

type Guard func(context.Context) (map[string]any, error)

// Budgets preserves the historical shorter caller deadline for ordinary sends.
// A declared guard buys every transfer phase plus the same caller slack.
func (a *Adapter) Budgets(guardRequests int) (execution, caller time.Duration) {
	execution = a.timeout * 3 * time.Duration(3+guardRequests)
	caller = a.timeout + a.callerSlack
	if guardRequests > 0 {
		caller = execution + a.callerSlack
	}
	return
}

func (a *Adapter) Send(ctx context.Context, requestID, thread, message string, settings *delivery.TaskSettings, guard Guard, guardBudget any) (delivery.Obj, error) {
	if a.transport == nil {
		return nil, &HostUnavailable{"this adapter was built read-only, with no transport to send on"}
	}
	if settings == nil {
		return nil, &HostUnavailable{"a send requires the authorized task settings; refusing to resume with host defaults"}
	}
	guardRequests, validBudget := guardBudget.(int)
	if !validBudget || guardRequests < 0 || guardRequests > 10 {
		return nil, &HostUnavailable{"guard_rpc_requests must be an integer from 0 through 10; refusing before any send"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t := a.transport
	// Admission, replay and recipient ownership are one ordered critical section. Replaying a
	// retained request never waits for an unrelated in-flight send to the same recipient.
	t.mu.Lock()
	if !t.accepting {
		t.mu.Unlock()
		return nil, errors.New("the relay transport is shutting down; nothing was sent")
	}
	params := map[string]any{"threadId": thread, "message": message}
	retained, err := a.ledger.Lookup(ctx, requestID, "send_message_to_thread", params, nil)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	if retained != nil && pyjson.Text(retained["status"]) != "not_attempted" {
		r, err := a.receipt(ctx, requestID, true)
		t.mu.Unlock()
		return r, err
	}
	if t.recipients[thread] {
		t.mu.Unlock()
		return contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "status", Value: "failed"}, {Key: "error", Value: "this relay already has a turn in flight for the recipient; message withheld without being sent"}, {Key: "rpcError", Value: contract.OrderedObject{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "another send to this thread is still in flight in this process"}}}}, nil
	}
	t.recipients[thread] = true
	t.pending.Add(1)
	held := &lease{t: t}
	t.mu.Unlock()
	executionBudget, callerBudget := a.Budgets(guardRequests)
	type answer struct {
		receipt delivery.Obj
		err     error
	}
	answerCh := make(chan answer, 1)
	go func() {
		work, stopWork := context.WithCancel(context.WithValue(managed.PropagateArmedReplay(t.ctx, ctx), leaseKey{}, held))
		defer stopWork()
		// Stops a call the caller no longer waits for. It runs on a goroutine of its own, so it
		// decides nothing: an answer can land before it runs, and guardedSend asks the caller's
		// ctx.Err before it uses one.
		stopCaller := context.AfterFunc(ctx, stopWork)
		defer stopCaller()
		run, cancel := context.WithTimeout(work, executionBudget)
		defer cancel()
		receipt, err := a.guardedSend(run, requestID, thread, message, settings, guard, ctx.Err)
		t.mu.Lock()
		delete(t.recipients, thread)
		held.ended = true
		t.mu.Unlock()
		answerCh <- answer{receipt, err}
		t.pending.Done()
	}()
	timer := time.NewTimer(callerBudget)
	defer timer.Stop()
	select {
	case answer := <-answerCh:
		return answer.receipt, answer.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, &HostUnavailable{""}
	}
}

// guardedSend is the bridge's send sequence on the relay's ledger. callerGone reports the
// caller's cancellation, nil while the caller still waits.
func (a *Adapter) guardedSend(ctx context.Context, requestID, thread, message string, settings *delivery.TaskSettings, guard Guard, callerGone func() error) (delivery.Obj, error) {
	// Python's coroutine takes a cancellation at the await it is suspended in, before it reads
	// that await's answer or error, and sends nothing after it. awaited is that await: it runs
	// after each call returns and before its answer is used, and asks the caller directly
	// rather than trusting ctx, which the caller's cancellation reaches only when
	// context.AfterFunc's goroutine gets to run. claimed keeps what it saw.
	var claimed error
	awaited := func(err error) error {
		if claimed = callerGone(); claimed != nil {
			return claimed
		}
		return err
	}
	fresh, receipt, err := a.ledger.Begin(ctx, requestID, "send_message_to_thread", map[string]any{"threadId": thread, "message": message}, nil)
	if err != nil {
		return nil, err
	}
	if !fresh {
		return a.receipt(ctx, requestID, true)
	}
	receipt["threadId"] = thread
	var watch *appserver.TurnWatch
	defer func() {
		if watch != nil {
			turn, _ := receipt["turnId"].(string)
			watch.Finish(turn, false)
		}
	}()
	save := func() error { _, err := a.ledger.Save(context.WithoutCancel(ctx), receipt); return err }
	if err = save(); err != nil {
		return nil, err
	}
	refuse := func(method string, rpc contract.OrderedObject, retry bool) {
		receipt["status"] = "failed"
		if retry {
			receipt["status"] = "not_attempted"
			receipt["retrySafe"] = true
			receipt["attemptedEffects"] = []string{}
		}
		receipt["error"] = method + ": " + pyjson.Text(rpc.Get("message"))
		receipt["rpcError"] = rpc
	}
	action := func() error {
		state, err := a.hostCall(ctx, "thread/read", map[string]any{"threadId": thread})
		if err = awaited(err); err != nil {
			return err
		}
		th, err := object(state["thread"])
		if err != nil {
			return err
		}
		var statusObject map[string]any
		if value, exists := th["status"]; exists {
			var ok bool
			statusObject, ok = value.(map[string]any)
			if !ok {
				return attributeError(value, "get")
			}
		}
		status := statusObject["type"]
		receipt["statusBeforeResume"] = status
		if pyjson.Text(status) == "active" {
			refuse("thread/read", contract.OrderedObject{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active; message withheld. Wait for completion."}}, false)
			return nil
		}
		send, expectedMCP, err := a.withMCP(ctx, settings)
		if err = awaited(err); err != nil {
			// Resolving a profile only reads the host, so a refusal or a lost read leaves nothing sent and the
			// request retryable; a caller that has gone, or the run's own bound, is settled as before.
			var refused *execution.Refusal
			code, message := "mcp_profile_unresolved", errorText(err)
			switch {
			case claimed != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return err
			case errors.As(err, &refused):
				code, message = refused.Code, refused.Detail
			}
			refuse("config/read", contract.OrderedObject{{Key: "code", Value: code}, {Key: "message", Value: message}}, true)
			return nil
		}
		params := plain(send.ResumeParams(thread)).(map[string]any)
		if send.SettingsFreeResume {
			params = map[string]any{"threadId": thread, "excludeTurns": true}
			if expectedMCP != nil {
				params["config"] = expectedMCP.Overrides()
			}
		}
		// CRW-1140: the thread's PABCD state lives at the cwd the host reports for it now, not at the
		// recorded cwd, which a later settings record may have changed. A resume that would run the
		// thread elsewhere while that state is in flight is refused before anything is sent.
		target, _ := params["cwd"].(string)
		if conflict := stateroot.Guard(os.LookupEnv, pyjson.Text(th["cwd"]), target, thread); conflict != nil {
			refuse("thread/read", contract.OrderedObject{{Key: "code", Value: stateroot.Code}, {Key: "message", Value: conflict.Error() + "; message withheld"}}, false)
			return nil
		}
		// The limit is not in the record (the host never reports it back), so a resume built from the
		// record would drop it and the thread would return to the host's own window. It is resolved
		// from the policy by the pair the record states, on both routes: a settings-free resume sends
		// no pair, but it may still send the limit of the pair the record says the thread is on.
		limit := a.withAutoCompactLimit(send, params)
		if client, ok := a.rpc.(*appserver.Client); ok {
			watch, err = client.WatchTurn(ctx, thread)
			if err = awaited(err); err != nil {
				return err
			}
			ctx = watch.Context(ctx)
		}
		// The record is made when the request is about to go out, and stored before it does: a resume whose
		// answer is lost is outcome_unknown and may have installed the limit, so its receipt must say what it
		// carried, and a process that stops while it waits must leave that in the ledger. It is made after the
		// watch is admitted and the caller asked, so a send that fails before this point never claims a limit
		// was requested. A record that cannot be stored is withdrawn and nothing is sent: the receipt says
		// not_attempted and retry-safe, so the same request ID may send again. Once the answer is read, the
		// observation below replaces this one with the settings the host reported.
		if limit != nil {
			receipt["settings"] = bridgesettings.AutoCompactReceipt(*limit, nil)
			if err := save(); err != nil {
				delete(receipt, "settings")
				refuse("thread/resume", contract.OrderedObject{{Key: "code", Value: "receipt_unsaved"}, {Key: "message", Value: "the receipt could not be stored before the resume; nothing was sent: " + errorText(err)}}, true)
				return nil
			}
		}
		resumed, err := a.callValue(ctx, "thread/resume", params)
		// A resume the transport certainly withheld never reached the host, so the stored limit is
		// withdrawn; a resume that may have gone out (its answer lost) keeps it. The transport's own
		// error decides this before a cancellation of the caller replaces it, as the caller's
		// cancellation changes what the send returns, not what the transport sent.
		if limit != nil && err != nil && notSent(err) {
			delete(receipt, "settings")
		}
		if err = awaited(err); err != nil {
			return err
		}
		receipt["resumed"] = resumed
		if settings.SettingsFreeResume {
			receipt["settingsFreeResume"] = true
		}
		// What the host reported is recorded as soon as it is read, so a read-back of the servers that
		// fails after it keeps the settings the resume reported; the servers are added when it succeeds.
		if limit != nil {
			observed, _ := plain(resumed).(map[string]any)
			receipt["settings"] = bridgesettings.AutoCompactReceipt(*limit, observed)
		}
		response := resumed
		if expectedMCP != nil {
			receipt["mcpOverridesTransmitted"] = true
			response, err = a.observeMCP(ctx, thread, resumed, expectedMCP, receipt)
			if err = awaited(err); err != nil {
				return err
			}
		}
		// The host never reports this value back, so the receipt is the only place the send can say
		// what it carried: recorded as requested and unobservable, never as verified, in the same
		// notation the bridge uses. The actual snapshot is taken again after the servers were read back, so it
		// carries mcpServers too. A resume that carried no limit records no settings observation.
		if limit != nil {
			observed, _ := plain(response).(map[string]any)
			receipt["settings"] = bridgesettings.AutoCompactReceipt(*limit, observed)
		}
		if err = save(); err != nil {
			return err
		}
		rpc, findings, notes := verifyResume(*send, response, status)
		if len(findings) > 0 {
			receipt["settingsFindings"] = findings
			refuse("thread/resume", rpc, false)
			return nil
		}
		if len(notes) > 0 {
			receipt["settingsNotes"] = notes
			if err = save(); err != nil {
				return err
			}
		}
		if guard != nil {
			decision, err := guard(ctx)
			if err = awaited(err); err != nil {
				return err
			}
			if decision != nil {
				code, hasCode := decision["code"]
				msg, hasMessage := decision["message"]
				if !hasCode || !hasMessage {
					code = "managed_guard_invalid"
					msg = "the pre-start guard returned neither None nor a refusal"
				}
				refuse("turn/start", contract.OrderedObject{{Key: "code", Value: code}, {Key: "message", Value: msg}}, true)
				return nil
			}
		}
		// A cancellation that arrived during the checks since the last answer is taken here, so
		// nothing is dispatched on behalf of a caller that has gone.
		if err := awaited(nil); err != nil {
			return err
		}
		turn, err := a.callValue(ctx, "turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": message}}})
		if err = awaited(err); err != nil {
			return err
		}
		value, err := subscript(turn, "turn")
		if err != nil {
			return err
		}
		id, err := subscript(value, "id")
		if err != nil {
			return err
		}
		receipt["turnId"] = id
		receipt["status"] = "accepted"
		return nil
	}
	err = action()
	if err != nil {
		var rpc *appserver.RPCError
		var phase *appserver.PhaseTimeout
		switch {
		case claimed != nil:
			// Python's CancelledError handler: unknown, never non-delivery, and no error text.
			receipt["status"] = "outcome_unknown"
		case errors.As(err, &rpc):
			receipt["status"] = "failed"
			receipt["error"] = rpc.Error()
			if rpc.Object != nil {
				receipt["rpcError"] = rpc.Object
			} else {
				receipt["rpcError"] = map[string]any{"code": rpc.Code, "message": rpc.Message}
			}
		case errors.As(err, &phase) && phase.Phase == "establish":
			receipt["status"] = "failed"
			receipt["error"] = phaseText(phase)
			receipt["rpcError"] = contract.OrderedObject{{Key: "code", Value: "connection_unavailable"}, {Key: "message", Value: phaseText(phase)}}
		case errors.As(err, &phase):
			receipt["status"] = "outcome_unknown"
			receipt["error"] = "PhaseTimeout: " + phaseText(phase)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			receipt["status"] = "outcome_unknown"
		default:
			receipt["status"] = "outcome_unknown"
			receipt["error"] = errorText(err)
		}
	}
	_, saveErr := a.ledger.Save(context.WithoutCancel(ctx), ledger.Receipt(receipt))
	if saveErr != nil {
		return nil, saveErr
	}
	if claimed != nil {
		// The caller's own error, whichever of this answer and its context Send reads first.
		return nil, claimed
	}
	if errors.Is(err, context.Canceled) {
		return nil, errors.New("the relay transport was shut down while this send was in flight; outcome unknown, do not resend under a new request id")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, &delivery.HostError{Kind: "TimeoutError", Message: ""}
	}
	return a.receipt(context.WithoutCancel(ctx), requestID, false)
}
