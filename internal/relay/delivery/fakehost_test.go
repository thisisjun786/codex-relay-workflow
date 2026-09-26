package delivery

import (
	"fmt"
	"strings"
)

// fakeThread and fakeHost are fakehost.FakeHostAdapter: a deterministic host shaped like the
// real one where the delivery logic depends on it. The receipt is written BEFORE the calls run,
// and a settled request id is answered from its receipt, never sent again.
type fakeThread struct {
	id             string
	status         string
	approvalPolicy string
	archived       *bool
	goalStatus     any
	canAcceptInput bool
	turns          []TurnInfo
	items          [][3]string
}

type fakeSend struct {
	requestID, thread, message, outcome string
}

type fakeHost struct {
	clock        *FakeClock
	threads      map[string]*fakeThread
	ledger       map[string]Obj
	script       []string
	sends        []fakeSend
	readFailures map[string]bool
	counter      int
	calls        []string
	onGoalRead   func(thread string)
	onArchived   func(thread string) (*bool, error)
}

func newFakeHost(clock *FakeClock) *fakeHost {
	return &fakeHost{clock: clock, threads: map[string]*fakeThread{}, ledger: map[string]Obj{}, readFailures: map[string]bool{}}
}

func (h *fakeHost) addThread(id string) *fakeThread {
	f := false
	t := &fakeThread{id: id, status: "idle", approvalPolicy: "never", archived: &f, canAcceptInput: true}
	h.threads[id] = t
	return t
}

func (h *fakeHost) startTurn(thread, turnID, status, text string) TurnInfo {
	t := h.threads[thread]
	h.counter++
	if turnID == "" {
		turnID = fmt.Sprintf("turn-%s-%d", thread, h.counter)
	}
	at := h.clock.Now()
	turn := TurnInfo{TurnID: turnID, Status: status, StartedAt: &at}
	t.turns = append(t.turns, turn)
	if text != "" {
		t.items = append(t.items, [3]string{turnID, text, "userMessage"})
	}
	return turn
}

func (h *fakeHost) finishTurn(thread, turnID, status string) {
	for i, turn := range h.threads[thread].turns {
		if turn.TurnID == turnID {
			h.threads[thread].turns[i].Status = status
		}
	}
}

func (h *fakeHost) guard(name string) error {
	h.calls = append(h.calls, name)
	if h.readFailures[name] {
		return &HostError{Kind: "ConnectionError", Message: name + " unavailable"}
	}
	return nil
}

func (h *fakeHost) ReadThread(thread string) (ThreadFacts, error) {
	if err := h.guard("read_thread"); err != nil {
		return ThreadFacts{}, err
	}
	t := h.threads[thread]
	accepts := t.canAcceptInput
	return ThreadFacts{RuntimeStatus: t.status, CanAcceptInput: &accepts}, nil
}

func (h *fakeHost) IsArchived(thread string, _ any) (*bool, error) {
	if err := h.guard("is_archived"); err != nil {
		return nil, err
	}
	if h.onArchived != nil {
		return h.onArchived(thread)
	}
	return h.threads[thread].archived, nil
}

func (h *fakeHost) ReadGoalStatus(thread string) (any, error) {
	if h.onGoalRead != nil {
		h.onGoalRead(thread)
	}
	if err := h.guard("read_goal_status"); err != nil {
		return nil, err
	}
	return h.threads[thread].goalStatus, nil
}

func (h *fakeHost) ListTurnIDs(thread string, limit int) ([]string, error) {
	if err := h.guard("list_turn_ids"); err != nil {
		return nil, err
	}
	turns := h.threads[thread].turns
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	var ids []string
	for _, t := range turns {
		ids = append(ids, t.TurnID)
	}
	return ids, nil
}

func (h *fakeHost) ReadTurn(thread, turn string) (*TurnInfo, error) {
	if err := h.guard("read_turn"); err != nil {
		return nil, err
	}
	for _, t := range h.threads[thread].turns {
		if t.TurnID == turn {
			copy := t
			return &copy, nil
		}
	}
	return nil, nil
}

func (h *fakeHost) GetOperation(requestID string) (Obj, error) {
	if err := h.guard("get_operation"); err != nil {
		return nil, err
	}
	return h.ledger[requestID], nil
}

func (h *fakeHost) FindToken(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
	if err := h.guard("find_token"); err != nil {
		return TokenScan{}, err
	}
	items := h.threads[thread].items
	scanned := 0
	for i := len(items) - 1; i >= 0 && scanned < limit; i-- {
		scanned++
		item := items[i]
		if messageOnly && item[2] != "userMessage" {
			continue
		}
		if strings.Contains(item[1], token) {
			return TokenScan{Found: true, TurnID: item[0], Exhausted: scanned >= len(items), Scanned: scanned}, nil
		}
	}
	return TokenScan{Found: false, Exhausted: limit >= len(items), Scanned: scanned}, nil
}

func (h *fakeHost) SendMessage(requestID, thread, message string, settings *TaskSettings) (Obj, error) {
	if cached, ok := h.ledger[requestID]; ok && str(cached, "status") != Unfinished {
		return append(append(Obj(nil), cached...), F{Key: "replayed", Value: true}), nil
	}
	outcome := "accepted"
	if len(h.script) > 0 {
		outcome, h.script = h.script[0], h.script[1:]
	}
	receipt := Obj{{Key: "requestId", Value: requestID}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: Unfinished}, {Key: "threadId", Value: thread}, {Key: "retrySafe", Value: false}}
	h.ledger[requestID] = receipt
	h.sends = append(h.sends, fakeSend{requestID, thread, message, outcome})
	t := h.threads[thread]
	resumed := Obj{{Key: "approvalPolicy", Value: t.approvalPolicy}}
	rpc := func(code, msg string) Obj { return Obj{{Key: "code", Value: code}, {Key: "message", Value: msg}} }
	switch outcome {
	case "in_progress":
		return append(Obj(nil), receipt...), nil
	case "busy":
		receipt = set(receipt, "status", FailedStatus)
		receipt = set(receipt, "error", "thread/read: Thread is active; message withheld. Wait for completion.")
		receipt = set(receipt, "rpcError", rpc("thread_busy", "Thread is active"))
	case "read_fail":
		receipt = set(receipt, "status", FailedStatus)
		receipt = set(receipt, "error", "thread/read: transport refused")
		receipt = set(receipt, "rpcError", rpc("internal", "transport refused"))
	case "approval_policy":
		receipt = set(receipt, "status", FailedStatus)
		receipt = set(receipt, "resumed", resumed)
		receipt = set(receipt, "error", "thread/resume: Interactive approvals unsupported; message withheld.")
		receipt = set(receipt, "rpcError", rpc("unsupported_approval_policy", "unsupported"))
	case "turn_start_fail":
		receipt = set(receipt, "status", FailedStatus)
		receipt = set(receipt, "resumed", resumed)
		receipt = set(receipt, "error", "turn/start: refused")
		receipt = set(receipt, "rpcError", rpc("internal", "refused"))
	case "transport_unknown":
		receipt = set(receipt, "status", OutcomeUnknown)
		receipt = set(receipt, "error", "TransportError: turn/start: response unavailable; do not resend")
	case "steer_existing":
		var existing any
		if n := len(t.turns); n > 0 {
			existing = t.turns[n-1].TurnID
		}
		receipt = set(receipt, "status", Accepted)
		receipt = set(receipt, "resumed", resumed)
		receipt = set(receipt, "turnId", existing)
		t.items = append(t.items, [3]string{pyStr(existing), message, "userMessage"})
	default:
		turn := h.startTurn(thread, "", "inProgress", message)
		receipt = set(receipt, "status", Accepted)
		receipt = set(receipt, "resumed", resumed)
		receipt = set(receipt, "turnId", turn.TurnID)
	}
	h.ledger[requestID] = receipt
	return append(Obj(nil), receipt...), nil
}

func (h *fakeHost) newestItems(thread string) []Item {
	items := h.threads[thread].items
	out := make([]Item, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		out = append(out, Item{items[i][0], items[i][1], items[i][2]})
	}
	return out
}

func (h *fakeHost) FindDispatchedTurn(thread, turnID string, sentAt float64) (TurnPresence, error) {
	if err := h.guard("find_dispatched_turn"); err != nil {
		return TurnPresence{}, err
	}
	turns := h.threads[thread].turns
	newest := make([]TurnInfo, 0, len(turns))
	for i := len(turns) - 1; i >= 0; i-- {
		newest = append(newest, turns[i])
	}
	return FindInListing([]ListingPage{{newest, false}}, turnID, sentAt)
}

func (h *fakeHost) FindTokenSince(thread, token string, older []string, limit int) (TokenScan, error) {
	if err := h.guard("find_token_since"); err != nil {
		return TokenScan{}, err
	}
	items := h.newestItems(thread)
	bound := min(limit, len(items))
	return FindTokenIn([]ItemPage{{items[:bound], bound < len(items)}}, token, older), nil
}

func (h *fakeHost) FindTokenInTurn(thread, token, turnID string, limit int) (TokenScan, error) {
	if err := h.guard("find_token_in_turn"); err != nil {
		return TokenScan{}, err
	}
	var own []Item
	for _, item := range h.threads[thread].items {
		if item[0] == turnID {
			own = append(own, Item{item[0], item[1], item[2]})
		}
	}
	bound := min(limit, len(own))
	return FindTokenInTurnItems([]ItemPage{{own[:bound], limit < len(own)}}, token, turnID), nil
}
