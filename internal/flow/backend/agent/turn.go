package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/sessionhost"
)

// MarkerPrefix starts the first line of a turn's input: `crw-flow-op: <call_id>/t<k>`.
const MarkerPrefix = "crw-flow-op: "

// Where a turn's operation marker was read from.
const (
	MarkerClientID  = "clientId"
	MarkerFirstLine = "firstLine"
)

// maxPullFailures bounds how many reads of a turn in a row may fail before the turn is given up as unreachable.
const maxPullFailures = 5

// UnreadableTurnError is a turn that exists on the host but whose full items could not be read (the list returned a
// summary view and thread/items/list did not give them). It is a state of its own: it is neither a turn without a message
// nor an absent turn, and the call stays with the reconcile pass.
type UnreadableTurnError struct {
	ThreadID, TurnID string
	Cause            error
}

func (e *UnreadableTurnError) Error() string {
	msg := fmt.Sprintf("turn %s of thread %s exists but its items cannot be read in full", e.TurnID, e.ThreadID)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *UnreadableTurnError) Unwrap() error { return e.Cause }

// ForeignTurnError is a turn read by its recorded id whose operation marker is another call's.
type ForeignTurnError struct{ Want, Got, Source string }

func (e *ForeignTurnError) Error() string {
	return fmt.Sprintf("the turn carries the operation %q (read from its %s), not %q", e.Got, e.Source, e.Want)
}

// MarkerOf reads the operation a turn belongs to from the turn's first userMessage item: the item's clientId first (it is
// TurnStartParams.clientUserMessageId, written by the host), then the first input line `crw-flow-op: <op>`.
func MarkerOf(items []json.RawMessage) (op, source string, ok bool) {
	for _, raw := range items {
		var item struct {
			Type     string  `json:"type"`
			ClientID *string `json:"clientId"`
			Content  []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "userMessage" {
			continue
		}
		if item.ClientID != nil && *item.ClientID != "" {
			return *item.ClientID, MarkerClientID, true
		}
		for _, part := range item.Content {
			if part.Type != "text" {
				continue
			}
			line, _, _ := strings.Cut(part.Text, "\n")
			if rest, found := strings.CutPrefix(strings.TrimRight(line, "\r"), MarkerPrefix); found {
				return rest, MarkerFirstLine, true
			}
			break
		}
		return "", "", false
	}
	return "", "", false
}

// turnParams builds the turn/start request: the thread's own pair repeated (a turn never changes it), the call's
// outputSchema, the operation marker as clientUserMessageId and as the input's first line, and a cwd only for an
// architect turn. A cwd on a turn moves the working directory of that turn and every later one, so an executor turn, whose
// cwd is fixed by thread/start or thread/fork, never carries one.
func turnParams(spec engine.TurnSpec) (map[string]any, error) {
	p := bridge.TurnStartParams{
		ThreadID: spec.ThreadID, Input: []bridge.UserInput{bridge.TextInput(spec.Input)},
		Model: spec.Pair.Model, Effort: spec.Pair.Effort,
	}
	if len(spec.Schema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(spec.Schema, &schema); err != nil {
			return nil, &engine.RPCError{Message: "the outputSchema is not a JSON object: " + err.Error(), CodexErrorInfo: "badRequest"}
		}
		p.OutputSchema = schema
	}
	if spec.Role == "architect" {
		p.CWD = spec.Cwd
	}
	params, err := p.Map()
	if err != nil {
		return nil, &engine.RPCError{Message: err.Error(), CodexErrorInfo: "badRequest"}
	}
	params["clientUserMessageId"] = spec.Op
	return params, nil
}

// RunTurn implements engine.Backend: it writes the flow binding of the turn (BoundConfig), starts the turn, follows the thread's notifications until the turn's terminal
// state, and reads the turn from the host by its recorded id when no notification comes.
func (b *Backend) RunTurn(ctx context.Context, spec engine.TurnSpec) (engine.TurnOutcome, error) {
	params, err := turnParams(spec)
	if err != nil {
		return engine.TurnOutcome{}, err
	}
	// F14: the line that tells the spawn hook whose thread this is is durable before the turn is sent; without it nothing is.
	if err := b.bindTurn(spec); err != nil {
		return engine.TurnOutcome{}, err
	}
	if err := b.hold(ctx, spec.ThreadID, spec.Tools); err != nil {
		return engine.TurnOutcome{}, err
	}
	feed := b.client.Bus().Subscribe(spec.ThreadID)
	defer feed.Close()
	raw, err := b.client.Call(ctx, "turn/start", params)
	if err != nil {
		return engine.TurnOutcome{}, requestError(err)
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Turn.ID == "" {
		return engine.TurnOutcome{}, &engine.TransportError{Err: errNoTurn}
	}
	turnID := started.Turn.ID
	if spec.Started != nil {
		spec.Started(turnID)
	}
	w := &turnWatch{b: b, spec: spec, turnID: turnID, feed: feed}
	return w.await(ctx)
}

// ErrTurnInProgress is what ReadTurn returns for a turn that has not ended.
var ErrTurnInProgress = errors.New("the turn is still in progress")

// ReadTurn reads one turn by its recorded id and returns it the way RunTurn would have ended it; nothing is waited for.
// It pages thread/turns/list with itemsView "full" up to that turn and reads a turn the list did not return in full with
// thread/items/list; a turn whose items cannot be read is an *UnreadableTurnError, never an empty turn. op is the
// operation the caller expects the turn to belong to (its clientId, else its first input line); "" skips the check.
func (b *Backend) ReadTurn(ctx context.Context, threadID, turnID, op string) (engine.TurnOutcome, error) {
	w := &turnWatch{b: b, spec: engine.TurnSpec{ThreadID: threadID, Op: op}, turnID: turnID}
	state, reading, err := w.read(ctx)
	if err != nil {
		return engine.TurnOutcome{}, requestError(err)
	}
	if state == nil {
		return engine.TurnOutcome{}, ErrTurnInProgress
	}
	return w.outcome(state, reading)
}

// message is an agent message of the turn seen on the bus.
// message is an agent message of the turn; the session host reads the same shape (sessionhost.Message).
type message = sessionhost.Message

type turnWatch struct {
	b      *Backend
	spec   engine.TurnSpec
	turnID string
	feed   *appserver.Feed

	messages []message
	usage    *usageSeen
}

type usageSeen struct {
	total, last engine.Tokens
}

// turnState is a turn as the bus or a read describes its end.
type turnState struct {
	status string
	err    json.RawMessage
}

func tokensOf(raw map[string]any) engine.Tokens {
	num := func(k string) int64 { f, _ := raw[k].(float64); return int64(f) }
	return engine.Tokens{Input: num("inputTokens"), CachedInput: num("cachedInputTokens"), Output: num("outputTokens"), ReasoningOutput: num("reasoningOutputTokens"), Total: num("totalTokens")}
}

// observe takes one notification of the thread; it returns the turn's terminal state when the notification is the end of
// this turn.
func (w *turnWatch) observe(n appserver.Notification) (*turnState, bool) {
	var params struct {
		TurnID string `json:"turnId"`
		Item   struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
			Text  string `json:"text"`
		} `json:"item"`
		Turn struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
		TokenUsage struct {
			Total map[string]any `json:"total"`
			Last  map[string]any `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(n.Params, &params) != nil {
		return nil, false
	}
	switch n.Method {
	case "item/completed":
		if params.TurnID == w.turnID && params.Item.Type == "agentMessage" {
			w.messages = append(w.messages, message{Phase: params.Item.Phase, Text: params.Item.Text})
		}
	case "thread/tokenUsage/updated":
		if params.TurnID == w.turnID {
			w.usage = &usageSeen{total: tokensOf(params.TokenUsage.Total), last: tokensOf(params.TokenUsage.Last)}
		}
	case "turn/completed":
		if params.Turn.ID == w.turnID {
			return &turnState{status: params.Turn.Status, err: params.Turn.Error}, true
		}
	}
	return nil, false
}

// await follows the turn to its end. The notification bus is the normal way; a tick, a feed that may have missed
// notifications, or nothing else to wait for reads the turn by its id. When ctx ends the host's turn is interrupted and
// the interrupted turn is waited for (the engine decides what the interruption means).
func (w *turnWatch) await(ctx context.Context) (engine.TurnOutcome, error) {
	b := w.b
	ticker := time.NewTicker(b.cfg.PollEvery)
	defer ticker.Stop()
	rpc := ctx
	done := ctx.Done()
	var deadline <-chan time.Time
	failures := 0
	// poll reads the turn: finished is whether it ended (err is then the outcome's own error). A read that fails is tried
	// again on the next tick; maxPullFailures in a row end the wait.
	poll := func() (out engine.TurnOutcome, finished bool, err error) {
		state, reading, readErr := w.read(rpc)
		if readErr != nil {
			failures++
			if failures >= maxPullFailures {
				return engine.TurnOutcome{}, true, &engine.TransportError{Err: fmt.Errorf("the turn %s cannot be read: %w", w.turnID, readErr)}
			}
			return engine.TurnOutcome{}, false, nil
		}
		failures = 0
		if state == nil {
			return engine.TurnOutcome{}, false, nil
		}
		out, err = w.outcome(state, reading)
		return out, true, err
	}
	for {
		select {
		case n, open := <-w.feed.Notifications():
			if !open {
				return engine.TurnOutcome{}, &engine.TransportError{Err: errors.New("the notification feed ended")}
			}
			if state, end := w.observe(n); end {
				return w.finish(rpc, state)
			}
		case <-w.feed.Lost():
			// The notifications of this turn may be gone, and a new connection follows nothing: read the turn and, when it is
			// still running, resume the thread on the connection that is now live.
			if out, finished, err := poll(); finished {
				return out, err
			}
			_ = b.hold(rpc, w.spec.ThreadID, w.spec.Tools)
		case <-ticker.C:
			if out, finished, err := poll(); finished {
				return out, err
			}
		case <-done:
			done = nil
			var cancel context.CancelFunc
			rpc, cancel = context.WithCancel(context.WithoutCancel(ctx))
			defer cancel()
			deadline = time.After(b.cfg.InterruptWait)
			if _, err := b.client.Call(rpc, "turn/interrupt", map[string]any{"threadId": w.spec.ThreadID, "turnId": w.turnID}); err != nil {
				return engine.TurnOutcome{}, &engine.TransportError{Err: fmt.Errorf("the turn could not be interrupted: %w", err)}
			}
		case <-deadline:
			return engine.TurnOutcome{}, &engine.TransportError{Err: fmt.Errorf("the interrupted turn %s did not end: %w", w.turnID, context.Cause(ctx))}
		}
	}
}

// read reads the turn by its recorded id. It returns a nil state while the turn is still running.
func (w *turnWatch) read(ctx context.Context) (*turnState, *appserverReading, error) {
	readings, err := w.b.client.Bus().ReconcileTurns(ctx, w.spec.ThreadID, []string{w.turnID})
	if err != nil {
		return nil, nil, err
	}
	r := readings[0]
	if !r.Found {
		return nil, nil, fmt.Errorf("the host lists no turn %s", w.turnID)
	}
	if r.Status == "inProgress" || r.Status == "" {
		return nil, nil, nil
	}
	return &turnState{status: r.Status, err: r.Error}, &r, nil
}

// finish builds the outcome of a turn the bus reported as ended.
func (w *turnWatch) finish(ctx context.Context, state *turnState) (engine.TurnOutcome, error) {
	if state.status != "completed" || w.busAnswered() {
		return w.outcome(state, nil)
	}
	// The final answer did not come over the bus (a commentary message is not it, and a stream that lost the end of the
	// turn may hold an earlier message only): read the turn's full items, which are the host's own record.
	readings, err := w.b.client.Bus().ReconcileTurns(ctx, w.spec.ThreadID, []string{w.turnID})
	if err != nil {
		return engine.TurnOutcome{}, requestError(err)
	}
	return w.outcome(state, &readings[0])
}

// busAnswered reports whether the bus delivered the turn's answer: a final_answer message, or - from a provider that sets
// no message phase (0.154 MessagePhase is nullable) - a last message without a phase when no commentary was seen. A turn
// with a commentary message and no final answer, or with no message at all, is read from the host.
func (w *turnWatch) busAnswered() bool {
	commentary := false
	for _, m := range w.messages {
		if m.Phase == "final_answer" {
			return true
		}
		commentary = commentary || m.Phase == "commentary"
	}
	return len(w.messages) > 0 && w.messages[len(w.messages)-1].Phase == "" && !commentary
}

// pickMessage is the last final answer among the messages, else the last message (sessionhost.PickMessage, which the
// session host shares).
func pickMessage(messages []message) string { return sessionhost.PickMessage(messages) }

// itemMessages are the agent messages among a turn's items.
func itemMessages(items []json.RawMessage) []message { return sessionhost.ItemMessages(items) }

// outcome turns a terminal turn into what the engine takes: the last agent message of a completed turn, or the failure
// of a failed or interrupted one. read is the host's reading of the turn when one was needed; its items must be
// readable in full for a completed turn whose final answer did not come over the bus.
func (w *turnWatch) outcome(state *turnState, read *appserverReading) (engine.TurnOutcome, error) {
	out := engine.TurnOutcome{TurnID: w.turnID, Tokens: w.b.turnTokens(w.spec.ThreadID, w.usage)}
	switch state.status {
	case "completed":
		// A completed turn used tokens; no usage notification means the usage is unknown, not zero.
		out.TokensUnmeasured = w.usage == nil
		messages := w.messages
		if read != nil {
			if !read.Readable {
				return engine.TurnOutcome{}, &engine.TransportError{Err: &UnreadableTurnError{ThreadID: w.spec.ThreadID, TurnID: w.turnID, Cause: read.ReadError}}
			}
			if op, source, ok := MarkerOf(read.Items); ok && w.spec.Op != "" && op != w.spec.Op {
				return engine.TurnOutcome{}, &engine.TransportError{Err: &ForeignTurnError{Want: w.spec.Op, Got: op, Source: source}}
			}
			// The host's items are the whole turn; what the bus delivered may be a part of it.
			if fromHost := itemMessages(read.Items); len(fromHost) > 0 {
				messages = fromHost
			}
		}
		out.Text = pickMessage(messages)
	case "failed", "interrupted":
		f := &engine.TurnFailure{Interrupted: state.status == "interrupted"}
		var e struct {
			Message        string `json:"message"`
			CodexErrorInfo any    `json:"codexErrorInfo"`
		}
		if len(state.err) > 0 && string(state.err) != "null" && json.Unmarshal(state.err, &e) == nil {
			f.Message, f.CodexErrorInfo = e.Message, infoName(e.CodexErrorInfo)
		}
		out.Failed = f
	default:
		return engine.TurnOutcome{}, &engine.TransportError{Err: fmt.Errorf("turn %s ended in the unknown state %q", w.turnID, state.status)}
	}
	return out, nil
}

// turnTokens is the tokens a turn used: the growth of the thread's running total over the turn. A thread the backend has
// not seen before counts the turn's own last usage.
func (b *Backend) turnTokens(thread string, seen *usageSeen) engine.Tokens {
	if seen == nil {
		return engine.Tokens{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	base, known := b.tokenTotal[thread]
	b.tokenTotal[thread] = seen.total
	if !known {
		return seen.last
	}
	return seen.total.Sub(base)
}

type appserverReading = appserver.TurnReading
