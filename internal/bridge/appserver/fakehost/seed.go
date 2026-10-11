package fakehost

import (
	"fmt"
	"time"
)

// SeedTurn is a turn placed on a seeded thread.
type SeedTurn struct {
	// ClientID is the clientId of the turn's userMessage ("" leaves it null).
	ClientID string
	// Text is the input text; it is also the first line a marker is read from.
	Text string
	// Status is the turn's status (default completed).
	Status string
	// Plan gives the turn the read behaviour of a TurnPlan (SummaryOnly, NotLoaded, ItemsListError).
	Plan TurnPlan
}

// SeedThread describes a thread that exists on the host without any request of the test's client having made it: a
// thread another caller made, or one whose creation answer a lost connection never delivered and that a test places
// late.
type SeedThread struct {
	Cwd string
	// Source is the Thread.source (default appServer).
	Source string
	// ParentThreadID is Thread.parentThreadId ("" is null).
	ParentThreadID string
	// ForkedFromID is Thread.forkedFromId ("" is null).
	ForkedFromID string
	// CreatedAt is Thread.createdAt (zero: now on the model's clock).
	CreatedAt time.Time
	Model     string
	Effort    string
	Turns     []SeedTurn
	// CopyTurnsOf copies the turns of this thread through ThroughTurn (inclusive; "" copies all) in front of Turns, the way a
	// fork carries its prefix.
	CopyTurnsOf string
	ThroughTurn string
}

// Seed places a thread on the host and returns its id.
func (t *Threads) Seed(spec SeedThread) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	th := &threadState{
		id: fmt.Sprintf("thr-fake-%03d", t.nextID), cwd: spec.Cwd, model: spec.Model, effort: spec.Effort, sandbox: "workspace-write",
		source: spec.Source, parent: spec.ParentThreadID, forkedOf: spec.ForkedFromID, config: map[string]any{},
	}
	if th.source == "" {
		th.source = "appServer"
	}
	if th.model == "" {
		th.model = "gpt-fake-default"
	}
	if spec.CreatedAt.IsZero() {
		th.created = t.createdAt()
	} else {
		th.created = spec.CreatedAt.Unix()
	}
	if spec.CopyTurnsOf != "" {
		if source, ok := t.byID[spec.CopyTurnsOf]; ok {
			for _, tu := range source.turns {
				cp := *tu
				cp.interrupt = nil
				th.turns = append(th.turns, &cp)
				if spec.ThroughTurn != "" && tu.id == spec.ThroughTurn {
					break
				}
			}
		}
	}
	for _, st := range spec.Turns {
		status := st.Status
		if status == "" {
			status = "completed"
		}
		tu := &turnState{
			id: fmt.Sprintf("turn-%s-%d", th.id, len(th.turns)+1), status: status, plan: st.Plan, text: st.Text, clientID: st.ClientID,
			startedAt: t.tick(), endedAt: t.tick(), interrupt: make(chan struct{}),
		}
		user := map[string]any{"type": "userMessage", "id": "u-" + tu.id, "content": []any{map[string]any{"type": "text", "text": st.Text, "text_elements": []any{}}}}
		if st.ClientID != "" {
			user["clientId"] = st.ClientID
		} else {
			user["clientId"] = nil
		}
		tu.items = append(tu.items, user)
		if status == "completed" {
			tu.items = append(tu.items, map[string]any{"type": "agentMessage", "id": "m-" + tu.id, "text": "ok", "phase": "final_answer"})
		}
		th.turns = append(th.turns, tu)
	}
	t.all = append(t.all, th)
	t.byID[th.id] = th
	return th.id
}

// SwapTurns exchanges two turns of a thread (a history in another order).
func (t *Threads) SwapTurns(thread string, i, j int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	th := t.byID[thread]
	th.turns[i], th.turns[j] = th.turns[j], th.turns[i]
}

// ReadAs gives one turn of a thread the read behaviour of plan (SummaryOnly, NotLoaded, ItemsListError): how
// thread/turns/list and thread/items/list report it from now on.
func (t *Threads) ReadAs(thread string, index int, plan TurnPlan) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID[thread].turns[index].plan = plan
}
