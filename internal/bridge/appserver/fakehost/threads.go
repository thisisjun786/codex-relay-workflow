package fakehost

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// This file is the fake's model of the thread methods a flow backend uses: thread/start, thread/read, thread/list,
// thread/fork, thread/resume, thread/turns/list, thread/items/list, turn/start and turn/interrupt. Unlike the scripted
// answers of the rest of the package it keeps state (threads, their turns and items) and pushes the notifications a
// turn produces to the connections that follow its thread. Every Thread it returns is built to satisfy the schema copy
// in testdata/schema-0.154 (the tests of the flow backend validate it against that copy).

// CLIVersion is the codex-cli version whose protocol the thread model follows.
const CLIVersion = "0.154.0"

// EphemeralIncludeTurnsRefusal is the error member codex-cli 0.154.0 answers thread/read with includeTurns on an
// ephemeral thread. The text is the one the 0.154.0 binary carries (upstream thread_processor.rs, "ephemeral threads do
// not support includeTurns"); the code is the App Server's invalid-request code. The raw answer is the fixture
// testdata/fixtures/thread-read-ephemeral-include-turns.json, which the backend's tests compare with this value.
var EphemeralIncludeTurnsRefusal = map[string]any{"code": -32600, "message": "ephemeral threads do not support includeTurns"}

// EphemeralTurnsListRefusal is the error member codex-cli 0.154.0 answers thread/turns/list with on an ephemeral thread
// (fixture testdata/fixtures/thread-turns-list-ephemeral.json).
var EphemeralTurnsListRefusal = map[string]any{"code": -32600, "message": "ephemeral threads do not support thread/turns/list"}

// IncludeTurnsDeprecated is the error member the fake answers thread/read with includeTurns true on a persistent
// thread: 0.154 deprecates full-history hydration for paginated threads and tells callers to page thread/turns/list and
// thread/items/list. The fake refuses it so that a backend that falls back on it fails its tests.
var IncludeTurnsDeprecated = map[string]any{"code": -32600, "message": "Full-history hydration is deprecated for paginated threads; omit `includeTurns` or set it to `false`, then page with `thread/turns/list` and `thread/items/list`."}

// ErrorWithInfo is an error member whose data carries a codexErrorInfo, the way a turn/start or thread/start
// refusal names its CodexErrorInfo variant. info is a variant name or a one-member object such as
// {"activeTurnNotSteerable": {"turnKind": "review"}}.
func ErrorWithInfo(code int, message string, info any) map[string]any {
	return map[string]any{"code": code, "message": message, "data": map[string]any{"codexErrorInfo": info}}
}

// Failure is how a planned turn fails.
type Failure struct {
	Message string
	// CodexErrorInfo is the variant (a name or a one-member object) of Turn.error.codexErrorInfo; nil leaves it null.
	CodexErrorInfo any
	// NoError leaves Turn.error null on a failed turn.
	NoError bool
}

// TurnPlan decides how one turn/start goes. The zero value answers "ok" at once.
type TurnPlan struct {
	// StartError answers turn/start itself with this error member and starts no turn.
	StartError map[string]any
	// Message is the turn's final agent message.
	Message string
	// Hang keeps the turn in progress until turn/interrupt (or the connection ends).
	Hang bool
	// Delay holds the turn back before it ends.
	Delay time.Duration
	// Gate holds the turn in progress until it is closed (or the turn is interrupted); a test closes it when the
	// state it needs - another turn in flight - has been reached.
	Gate <-chan struct{}
	// Fail ends the turn failed.
	Fail *Failure
	// Interrupted ends the turn interrupted without any turn/interrupt request (an external interrupt).
	Interrupted bool
	// IgnoreInterrupt keeps a Hang turn in progress when turn/interrupt arrives: the host acknowledges the request and
	// never ends the turn.
	IgnoreInterrupt bool
	// Silent sends no notification for this turn: the thread keeps the result and only a read finds it.
	Silent bool
	// SummaryOnly makes thread/turns/list return the turn with itemsView "summary" and no items.
	SummaryOnly bool
	// ItemsListError answers thread/items/list for this turn with this error member.
	ItemsListError map[string]any
	// Tokens is the turn's token usage; the zero value is a small fixed usage.
	Tokens int64
}

// TurnRequest is the turn/start request a plan is asked about.
type TurnRequest struct {
	ThreadID string
	// N is the turn's ordinal on the thread, from 1.
	N      int
	Params map[string]any
	// Text is the input's text, ClientID the clientUserMessageId ("" when absent).
	Text     string
	ClientID string
}

// ThreadView is a copy of a thread the model holds.
type ThreadView struct {
	ID                    string
	Cwd                   string
	Model, Effort         string
	Sandbox               string
	Ephemeral             bool
	DeveloperInstructions string
	Config                map[string]any
	StartParams           map[string]any
	ForkedFromID          string
	Turns                 []TurnView
}

// TurnView is a copy of a turn the model holds.
type TurnView struct {
	ID       string
	Status   string
	Text     string
	ClientID string
	Params   map[string]any
}

type turnState struct {
	id        string
	status    string
	errObj    any
	items     []map[string]any
	plan      TurnPlan
	params    map[string]any
	text      string
	clientID  string
	startedAt int64
	endedAt   int64
	interrupt chan struct{}
}

type threadState struct {
	id        string
	cwd       string
	model     string
	effort    string
	sandbox   string
	ephemeral bool
	dev       string
	config    map[string]any
	start     map[string]any
	forkedOf  string
	created   int64
	turns     []*turnState
	active    *turnState
	used      int64
}

// Threads is the thread model of one fake Server.
type Threads struct {
	server *Server
	mu     sync.Mutex
	nextID int
	seq    int64
	all    []*threadState
	byID   map[string]*threadState
	subs   map[string][]*Conn

	onTurn       func(TurnRequest) TurnPlan
	onStart      func(params map[string]any) map[string]any
	readback     func(ThreadView) (model, effort *string)
	dropAll      bool
	interrupted  []string
	unsubscribed []string
}

// Threads installs (once) and returns the thread model of the server.
func (s *Server) Threads() *Threads {
	s.mu.Lock()
	if s.threads != nil {
		t := s.threads
		s.mu.Unlock()
		return t
	}
	t := &Threads{server: s, byID: map[string]*threadState{}, subs: map[string][]*Conn{}, seq: 1790000000}
	s.threads = t
	s.mu.Unlock()
	s.HandleConn("thread/start", t.handleStart)
	s.HandleConn("thread/resume", t.handleResume)
	s.HandleConn("thread/fork", t.handleFork)
	s.HandleConn("thread/unsubscribe", t.handleUnsubscribe)
	s.HandleConn("thread/read", t.handleRead)
	s.HandleConn("thread/list", t.handleList)
	s.HandleConn("thread/turns/list", t.handleTurnsList)
	s.HandleConn("thread/items/list", t.handleItemsList)
	s.HandleConn("turn/start", t.handleTurnStart)
	s.HandleConn("turn/interrupt", t.handleInterrupt)
	return t
}

// OnTurn decides every turn/start from now on.
func (t *Threads) OnTurn(plan func(TurnRequest) TurnPlan) {
	t.mu.Lock()
	t.onTurn = plan
	t.mu.Unlock()
}

// OnStart lets a test refuse a thread/start: a non-nil error member is the answer.
func (t *Threads) OnStart(refuse func(params map[string]any) map[string]any) {
	t.mu.Lock()
	t.onStart = refuse
	t.mu.Unlock()
}

// Readback decides the model and reasoning effort thread/read (and the start answer) report for a thread; a nil pointer
// is reported as null. Without it the thread reports what thread/start asked for.
func (t *Threads) Readback(fn func(ThreadView) (model, effort *string)) {
	t.mu.Lock()
	t.readback = fn
	t.mu.Unlock()
}

// DropNotifications stops every notification of every thread: the host keeps its state and answers reads, and a client
// that waits for turn/completed never hears it.
func (t *Threads) DropNotifications(drop bool) {
	t.mu.Lock()
	t.dropAll = drop
	t.mu.Unlock()
}

// Thread returns a copy of one thread.
func (t *Threads) Thread(id string) (ThreadView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[id]
	if !ok {
		return ThreadView{}, false
	}
	return th.view(), true
}

// All returns a copy of every thread in creation order.
func (t *Threads) All() []ThreadView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ThreadView, 0, len(t.all))
	for _, th := range t.all {
		out = append(out, th.view())
	}
	return out
}

// Interrupts returns the thread ids turn/interrupt was received for, in order.
func (t *Threads) Interrupts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.interrupted)
}

func (th *threadState) view() ThreadView {
	v := ThreadView{ID: th.id, Cwd: th.cwd, Model: th.model, Effort: th.effort, Sandbox: th.sandbox, Ephemeral: th.ephemeral,
		DeveloperInstructions: th.dev, Config: th.config, StartParams: th.start, ForkedFromID: th.forkedOf}
	for _, tu := range th.turns {
		v.Turns = append(v.Turns, TurnView{ID: tu.id, Status: tu.status, Text: tu.text, ClientID: tu.clientID, Params: tu.params})
	}
	return v
}

func (t *Threads) tick() int64 {
	t.seq++
	return t.seq
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func decode(raw json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func errorReply(object map[string]any) Reply { return Reply{ErrorObject: object} }

func (t *Threads) subscribe(thread string, c *Conn) {
	for _, existing := range t.subs[thread] {
		if existing == c {
			return
		}
	}
	t.subs[thread] = append(t.subs[thread], c)
}

// notify sends n to the connections that follow thread, unless notifications are dropped.
func (t *Threads) notify(thread string, silent bool, n Notification) {
	t.mu.Lock()
	drop := t.dropAll || silent
	conns := slices.Clone(t.subs[thread])
	t.mu.Unlock()
	if drop {
		return
	}
	for _, c := range conns {
		c.Notify(n)
	}
}

func sandboxPolicy(mode string) map[string]any {
	switch mode {
	case "read-only":
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	}
	return map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{}, "excludeSlashTmp": false, "excludeTmpdirEnvVar": false}
}

func configString(config map[string]any, key string) string {
	s, _ := config[key].(string)
	return s
}

// create makes a thread from thread/start or thread/fork parameters. The caller holds the lock.
func (t *Threads) create(c *Conn, params map[string]any) *threadState {
	t.nextID++
	config, _ := params["config"].(map[string]any)
	th := &threadState{
		id: fmt.Sprintf("thr-fake-%03d", t.nextID), cwd: str(params, "cwd"), model: str(params, "model"),
		effort: configString(config, "model_reasoning_effort"), sandbox: str(params, "sandbox"),
		dev: str(params, "developerInstructions"), config: config, start: params, created: t.tick(),
	}
	th.ephemeral, _ = params["ephemeral"].(bool)
	if th.model == "" {
		th.model = "gpt-fake-default"
	}
	if th.cwd == "" {
		th.cwd = "/"
	}
	t.all = append(t.all, th)
	t.byID[th.id] = th
	t.subscribe(th.id, c)
	return th
}

func (t *Threads) readbackOf(th *threadState) (model, effort any) {
	m, e := &th.model, &th.effort
	if *e == "" {
		e = nil
	}
	if t.readback != nil {
		m, e = t.readback(th.view())
	}
	if m != nil {
		model = *m
	}
	if e != nil {
		effort = *e
	}
	return model, effort
}

func (t *Threads) threadObject(th *threadState, withTurns bool) map[string]any {
	model, effort := t.readbackOf(th)
	turns := []any{}
	preview := ""
	for i, tu := range th.turns {
		if i == 0 {
			preview = tu.text
		}
		if withTurns {
			turns = append(turns, tu.object(true))
		}
	}
	var forked any
	if th.forkedOf != "" {
		forked = th.forkedOf
	}
	return map[string]any{
		"id": th.id, "cliVersion": CLIVersion, "createdAt": th.created, "updatedAt": th.created + int64(len(th.turns)),
		"cwd": th.cwd, "ephemeral": th.ephemeral, "modelProvider": "openai", "model": model, "reasoningEffort": effort,
		"preview": preview, "projectId": nil, "sessionId": "session-" + th.id, "source": "appServer",
		"status": map[string]any{"type": "idle"}, "turns": turns, "forkedFromId": forked, "parentThreadId": nil,
		"name": nil, "path": nil, "threadSource": nil,
	}
}

func (tu *turnState) object(withItems bool) map[string]any {
	var started, ended any
	if tu.startedAt != 0 {
		started = tu.startedAt
	}
	if tu.endedAt != 0 {
		ended = tu.endedAt
	}
	items := []any{}
	view := "notLoaded"
	if withItems {
		view = "full"
		for _, it := range tu.items {
			items = append(items, it)
		}
	}
	return map[string]any{"id": tu.id, "status": tu.status, "error": tu.errObj, "items": items, "itemsView": view, "startedAt": started, "completedAt": ended, "durationMs": nil}
}

func (t *Threads) startResponse(th *threadState, withTurns bool) map[string]any {
	model, effort := t.readbackOf(th)
	return map[string]any{
		"approvalPolicy": "never", "approvalsReviewer": "user", "cwd": th.cwd, "model": model, "modelProvider": "openai",
		"reasoningEffort": effort, "sandbox": sandboxPolicy(th.sandbox), "instructionSources": []any{}, "serviceTier": nil,
		"thread": t.threadObject(th, withTurns),
	}
}

func (t *Threads) handleStart(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onStart != nil {
		if refusal := t.onStart(params); refusal != nil {
			return errorReply(refusal)
		}
	}
	th := t.create(c, params)
	return Reply{Result: t.startResponse(th, false)}
}

func (t *Threads) handleResume(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	t.subscribe(th.id, c)
	exclude, _ := params["excludeTurns"].(bool)
	return Reply{Result: t.startResponse(th, !exclude)}
}

// handleUnsubscribe stops the notifications of a thread on the connection that asked: the host follows a thread only on
// the connections subscribed to it (thread/start, thread/resume and thread/fork subscribe).
func (t *Threads) handleUnsubscribe(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	id := str(params, "threadId")
	if _, ok := t.byID[id]; !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + id})
	}
	t.subs[id] = slices.DeleteFunc(t.subs[id], func(other *Conn) bool { return other == c })
	t.unsubscribed = append(t.unsubscribed, id)
	return Reply{Result: map[string]any{"status": "unsubscribed"}}
}

// Unsubscribes returns the thread ids thread/unsubscribe was received for, in order.
func (t *Threads) Unsubscribes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.unsubscribed)
}

func (t *Threads) handleFork(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	source, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	if t.onStart != nil {
		if refusal := t.onStart(params); refusal != nil {
			return errorReply(refusal)
		}
	}
	keep := len(source.turns)
	if last := str(params, "lastTurnId"); last != "" {
		keep = -1
		for i, tu := range source.turns {
			if tu.id == last {
				keep = i + 1
			}
		}
		if keep < 0 {
			return errorReply(map[string]any{"code": -32600, "message": "turn not found: " + last})
		}
	}
	// A fork inherits what the source was started with unless the request overrides it.
	merged := map[string]any{"model": source.model, "sandbox": source.sandbox, "config": source.config, "developerInstructions": source.dev}
	for k, v := range params {
		merged[k] = v
	}
	th := t.create(c, merged)
	th.forkedOf = source.id
	for _, tu := range source.turns[:keep] {
		cp := *tu
		cp.interrupt = nil
		th.turns = append(th.turns, &cp)
	}
	exclude, _ := params["excludeTurns"].(bool)
	return Reply{Result: t.startResponse(th, !exclude)}
}

func (t *Threads) handleRead(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	if include, _ := params["includeTurns"].(bool); include {
		if th.ephemeral {
			return errorReply(EphemeralIncludeTurnsRefusal)
		}
		return errorReply(IncludeTurnsDeprecated)
	}
	return Reply{Result: map[string]any{"thread": t.threadObject(th, false)}}
}

func page(total int, params map[string]any) (from, to int, next any) {
	limit := 50
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if c := str(params, "cursor"); c != "" {
		from, _ = strconv.Atoi(c)
	}
	from = min(max(from, 0), total)
	to = min(from+limit, total)
	if to < total {
		next = strconv.Itoa(to)
	}
	return from, to, next
}

func (t *Threads) handleList(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	var matched []*threadState
	for _, th := range t.all {
		if cwd := str(params, "cwd"); cwd != "" && th.cwd != cwd {
			continue
		}
		matched = append(matched, th)
	}
	from, to, next := page(len(matched), params)
	data := []any{}
	for _, th := range matched[from:to] {
		data = append(data, t.threadObject(th, false))
	}
	return Reply{Result: map[string]any{"data": data, "nextCursor": next, "backwardsCursor": nil}}
}

func (t *Threads) handleTurnsList(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	if th.ephemeral {
		return errorReply(EphemeralTurnsListRefusal)
	}
	turns := slices.Clone(th.turns)
	if str(params, "sortDirection") != "asc" {
		slices.Reverse(turns)
	}
	from, to, next := page(len(turns), params)
	data := []any{}
	for _, tu := range turns[from:to] {
		full := str(params, "itemsView") == "full" && !tu.plan.SummaryOnly
		o := tu.object(full)
		if tu.plan.SummaryOnly {
			o["itemsView"] = "summary"
		}
		data = append(data, o)
	}
	return Reply{Result: map[string]any{"data": data, "nextCursor": next, "backwardsCursor": nil}}
}

func (t *Threads) handleItemsList(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	for _, tu := range th.turns {
		if tu.id != str(params, "turnId") {
			continue
		}
		if tu.plan.ItemsListError != nil {
			return errorReply(tu.plan.ItemsListError)
		}
		from, to, next := page(len(tu.items), params)
		data := []any{}
		for _, it := range tu.items[from:to] {
			data = append(data, map[string]any{"item": it, "turnId": tu.id})
		}
		return Reply{Result: map[string]any{"data": data, "nextCursor": next, "backwardsCursor": nil}}
	}
	return errorReply(map[string]any{"code": -32600, "message": "turn not found: " + str(params, "turnId")})
}

func inputText(params map[string]any) string {
	var text string
	input, _ := params["input"].([]any)
	for _, element := range input {
		if m, ok := element.(map[string]any); ok && str(m, "type") == "text" {
			text += str(m, "text")
		}
	}
	return text
}

func (t *Threads) handleTurnStart(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		t.mu.Unlock()
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	req := TurnRequest{ThreadID: th.id, N: len(th.turns) + 1, Params: params, Text: inputText(params), ClientID: str(params, "clientUserMessageId")}
	plan := TurnPlan{Message: "ok"}
	if t.onTurn != nil {
		plan = t.onTurn(req)
	}
	if plan.StartError != nil {
		t.mu.Unlock()
		return errorReply(plan.StartError)
	}
	if th.active != nil {
		t.mu.Unlock()
		return errorReply(ErrorWithInfo(-32600, "an active turn cannot accept another turn/start", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "regular"}}))
	}
	tu := &turnState{
		id: fmt.Sprintf("turn-%s-%d", th.id, req.N), status: "inProgress", plan: plan, params: params, text: req.Text,
		clientID: req.ClientID, startedAt: t.tick(), interrupt: make(chan struct{}),
	}
	user := map[string]any{"type": "userMessage", "id": "u-" + tu.id, "content": []any{map[string]any{"type": "text", "text": req.Text, "text_elements": []any{}}}}
	if req.ClientID != "" {
		user["clientId"] = req.ClientID
	} else {
		user["clientId"] = nil
	}
	tu.items = append(tu.items, user)
	th.turns = append(th.turns, tu)
	th.active = tu
	object := tu.object(false)
	t.mu.Unlock()
	return Reply{Result: map[string]any{"turn": object}, After: func(conn *Conn) { go t.run(conn, th, tu) }}
}

func (t *Threads) handleInterrupt(c *Conn, raw json.RawMessage) Reply {
	params := decode(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	th, ok := t.byID[str(params, "threadId")]
	if !ok {
		return errorReply(map[string]any{"code": -32600, "message": "thread not found: " + str(params, "threadId")})
	}
	t.interrupted = append(t.interrupted, th.id)
	if tu := th.active; tu != nil && (str(params, "turnId") == "" || str(params, "turnId") == tu.id) {
		select {
		case <-tu.interrupt:
		default:
			close(tu.interrupt)
		}
	}
	return Reply{Result: map[string]any{}}
}

// run plays a turn's timeline after turn/start was answered.
func (t *Threads) run(conn *Conn, th *threadState, tu *turnState) {
	plan := tu.plan
	t.notify(th.id, plan.Silent, Notification{Method: "turn/started", Params: map[string]any{"threadId": th.id, "turn": tu.object(false)}})
	interrupted := plan.Interrupted
	if plan.Hang {
		if plan.IgnoreInterrupt {
			<-conn.Done()
			return
		}
		select {
		case <-tu.interrupt:
			interrupted = true
		case <-conn.Done():
			return
		}
	} else if plan.Gate != nil {
		select {
		case <-plan.Gate:
		case <-tu.interrupt:
			interrupted = true
		case <-conn.Done():
			return
		}
	} else if plan.Delay > 0 {
		timer := time.NewTimer(plan.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-tu.interrupt:
			interrupted = true
		case <-conn.Done():
			return
		}
	}
	t.mu.Lock()
	tu.endedAt = t.tick()
	var failure *Failure
	switch {
	case interrupted:
		tu.status = "interrupted"
	case plan.Fail != nil:
		tu.status, failure = "failed", plan.Fail
		if !failure.NoError {
			tu.errObj = map[string]any{"message": failure.Message, "codexErrorInfo": failure.CodexErrorInfo, "additionalDetails": nil}
		}
	default:
		tu.status = "completed"
		item := map[string]any{"type": "agentMessage", "id": "m-" + tu.id, "text": plan.Message, "phase": "final_answer"}
		tu.items = append(tu.items, item)
	}
	th.active = nil
	final := tu.object(false)
	tokens := plan.Tokens
	if tokens == 0 {
		tokens = 10
	}
	var message map[string]any
	if tu.status == "completed" {
		message = tu.items[len(tu.items)-1]
	}
	th.used += tokens
	total := th.used
	t.mu.Unlock()

	if message != nil {
		t.notify(th.id, plan.Silent, Notification{Method: "item/completed", Params: map[string]any{"item": message, "threadId": th.id, "turnId": tu.id, "completedAtMs": 1790000041000}})
	}
	breakdown := func(n int64) map[string]any {
		return map[string]any{"cachedInputTokens": 0, "inputTokens": n / 2, "outputTokens": n - n/2, "reasoningOutputTokens": 0, "totalTokens": n}
	}
	t.notify(th.id, plan.Silent, Notification{Method: "thread/tokenUsage/updated", Params: map[string]any{"threadId": th.id, "turnId": tu.id, "tokenUsage": map[string]any{"last": breakdown(tokens), "total": breakdown(total), "modelContextWindow": 272000}}})
	if failure != nil && !failure.NoError {
		t.notify(th.id, plan.Silent, Notification{Method: "error", Params: map[string]any{"threadId": th.id, "turnId": tu.id, "willRetry": false, "error": map[string]any{"message": failure.Message, "codexErrorInfo": failure.CodexErrorInfo, "additionalDetails": nil}}})
	}
	t.notify(th.id, plan.Silent, Notification{Method: "turn/completed", Params: map[string]any{"threadId": th.id, "turn": final}})
}
