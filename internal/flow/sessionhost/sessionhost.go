// Package sessionhost reads the host for the session calls of a flow run (CRW-1316, lane S2b CRW-1332): the role
// subagents a project orchestrator spawns (multi-agent V1 spawn_agent) are threads of the App Server, and the run reads
// them there to confirm what was spawned and to harvest the answer.
//
// Every read goes through the App Server client (internal/bridge/appserver): thread/read without turns (ReadAgent),
// thread/turns/list newest first with summary or full items (Turns), turn/interrupt (Interrupt). The final answer of a turn
// is chosen by FinalAnswer, the operation a step names by ParseMarker, the tokens a subagent used by Tokens (its rollout's
// last token_count, read only), and the subagent of a published step is found from the owner's side by FindByMarker (the
// spawn hook's ledger joined with the owner turn's collabAgentToolCall items).
//
// The types are plain: the engine (internal/flow/engine) defines the interface it consumes and this package imports
// neither the engine nor the backends.
package sessionhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// ErrNotFound is a thread the host does not have (thread/read, thread/turns/list or turn/interrupt answered that it was not
// found). Errors wrap it; test with errors.Is.
var ErrNotFound = errors.New("sessionhost: the host has no such thread")

// Caller is the App Server client as this package uses it (*appserver.Client).
type Caller interface {
	Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error)
}

// Host reads subagent threads through an App Server client.
type Host struct {
	client   Caller
	boundDir string
	// OwnerTurns is how many of the owner's most recent turns FindByMarker searches (0: DefaultOwnerTurns).
	OwnerTurns int
}

// DefaultOwnerTurns is the window of owner turns FindByMarker searches.
const DefaultOwnerTurns = 20

// New is a Host reading through client; boundDir is the flow-bound directory whose decision ledger FindByMarker reads
// (bound.Dir() in production, a temporary directory in tests).
func New(client Caller, boundDir string) *Host { return &Host{client: client, boundDir: boundDir} }

// The statuses of a thread (ThreadStatus.type).
const (
	StatusIdle        = "idle"
	StatusActive      = "active"
	StatusNotLoaded   = "notLoaded"
	StatusSystemError = "systemError"
)

// Agent is a subagent thread as thread/read (includeTurns false) gives it.
type Agent struct {
	ThreadID string
	// ParentThreadID is Thread.parentThreadId: the thread that spawned it ("" for a thread that is no subagent).
	ParentThreadID string
	// Model and Effort are the pair the thread is configured with (Thread.model, Thread.reasoningEffort); "" is null.
	Model, Effort string
	// Cwd is the working directory the host reports (the parent turn's for a subagent).
	Cwd string
	// AgentRole is Thread.agentRole, the agent_type it was spawned with ("" is null).
	AgentRole string
	// Status is ThreadStatus.type: idle, active, notLoaded or systemError.
	Status string
	// RolloutPath is Thread.path, the rollout file ("" is null).
	RolloutPath string
	// ThreadSource is Thread.threadSource ("subagent"); Nickname Thread.agentNickname; CLIVersion Thread.cliVersion.
	ThreadSource, Nickname, CLIVersion string
}

// threadWire is the part of a Thread this package reads.
type threadWire struct {
	ID              string  `json:"id"`
	ParentThreadID  *string `json:"parentThreadId"`
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	Cwd             string  `json:"cwd"`
	AgentRole       *string `json:"agentRole"`
	AgentNickname   *string `json:"agentNickname"`
	ThreadSource    *string `json:"threadSource"`
	Path            *string `json:"path"`
	CLIVersion      string  `json:"cliVersion"`
	Status          struct {
		Type string `json:"type"`
	} `json:"status"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ReadAgent reads a thread with thread/read, includeTurns false.
func (h *Host) ReadAgent(ctx context.Context, threadID string) (Agent, error) {
	raw, err := h.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return Agent{}, err
	}
	var answer struct {
		Thread threadWire `json:"thread"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Thread.ID == "" {
		return Agent{}, fmt.Errorf("sessionhost: thread/read of %s: the answer names no thread", threadID)
	}
	w := answer.Thread
	return Agent{
		ThreadID: w.ID, ParentThreadID: deref(w.ParentThreadID), Model: deref(w.Model), Effort: deref(w.ReasoningEffort), Cwd: w.Cwd,
		AgentRole: deref(w.AgentRole), Status: w.Status.Type, RolloutPath: deref(w.Path), ThreadSource: deref(w.ThreadSource),
		Nickname: deref(w.AgentNickname), CLIVersion: w.CLIVersion,
	}, nil
}

// Interrupt asks the host to interrupt the turn of a thread (turn/interrupt). The host answers at once; the turn ends
// interrupted later, which a read of the turn shows.
func (h *Host) Interrupt(ctx context.Context, threadID, turnID string) error {
	_, err := h.call(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	return err
}

// call is one request; an answer that says the thread is not there wraps ErrNotFound.
func (h *Host) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	raw, err := h.client.Call(ctx, method, params)
	if err != nil {
		var rpc *appserver.RPCError
		if errors.As(err, &rpc) && notFound(rpc) {
			return nil, fmt.Errorf("%w: %s: %v", ErrNotFound, method, err)
		}
		return nil, fmt.Errorf("sessionhost: %w", err)
	}
	return raw, nil
}

// notFound reports whether an error answer says the thread does not exist. The fake answers "thread not found: <id>"; a
// host whose thread has no rollout says "no rollout found". A transport failure is never "not found".
func notFound(e *appserver.RPCError) bool {
	message := strings.ToLower(e.Message)
	if m, ok := e.Object["message"].(string); ok {
		message += " " + strings.ToLower(m)
	}
	return strings.Contains(message, "not found") || strings.Contains(message, "no rollout")
}

// TurnError is a failed turn's error (Turn.error): its message and its CodexErrorInfo variant name ("" when null).
type TurnError struct {
	Message, CodexErrorInfo string
}

// Turn is one turn of a thread as thread/turns/list gives it, with what a run reads from its items.
type Turn struct {
	ID string
	// Status is completed, interrupted, failed or inProgress.
	Status string
	// ItemsView is how much of the items the host gave: full, summary or notLoaded.
	ItemsView string
	// FirstUserText is the text of the turn's first userMessage; Marker the operation its first line names
	// (`crw-flow-op: <op>`, or the message's clientId when that is an operation), "" for none.
	FirstUserText, Marker string
	// FinalText and FinalPhase are the turn's answer (FinalAnswer): Answered says there is one.
	FinalText, FinalPhase string
	Answered              bool
	// ErrorInfo is the error of a failed turn, nil when the host gives none.
	ErrorInfo *TurnError
	// UpdatedAt is when the turn completed, else when it started (zero when the host gives neither).
	UpdatedAt time.Time
	// ReadError is why a full read of the turn could not be completed (the list gave no full items and thread/items/list
	// failed); the other fields are what the list gave.
	ReadError error
}

// turnWire is a Turn as the host gives it.
type turnWire struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	ItemsView   string            `json:"itemsView"`
	Items       []json.RawMessage `json:"items"`
	Error       json.RawMessage   `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
}

// maxPages bounds the pages of one listing; pageSize is the page size asked for when no limit is given.
const (
	maxPages = 500
	pageSize = 100
)

// Turns lists the turns of a thread newest first (thread/turns/list, sortDirection desc). limit caps how many (0 or less:
// every turn, page by page). full asks for every item (itemsView full); otherwise the host's summary view (the first user
// message and the last agent message). A full read that the host answers without the items is completed with
// thread/items/list.
func (h *Host) Turns(ctx context.Context, threadID string, limit int, full bool) ([]Turn, error) {
	wires, _, err := h.listTurns(ctx, threadID, limit, full)
	if err != nil {
		return nil, err
	}
	out := make([]Turn, len(wires))
	for i, w := range wires {
		var readErr error
		if full && w.ItemsView != "full" && w.ItemsView != "" {
			if items, err := h.turnItems(ctx, threadID, w.ID); err != nil {
				readErr = err
			} else {
				w.Items, w.ItemsView = items, "full"
			}
		}
		out[i] = w.turn()
		out[i].ReadError = readErr
	}
	return out, nil
}

// listTurns lists up to limit turns newest first (every turn when limit is 0 or less); more says the thread has turns older
// than the ones returned (only a limited listing can stop before the thread's first turn).
func (h *Host) listTurns(ctx context.Context, threadID string, limit int, full bool) (turns []turnWire, more bool, err error) {
	view := "summary"
	if full {
		view = "full"
	}
	var out []turnWire
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, false, fmt.Errorf("sessionhost: thread/turns/list of %s: more than %d pages", threadID, maxPages)
		}
		params := map[string]any{"threadId": threadID, "itemsView": view, "sortDirection": "desc", "limit": pageSize}
		if limit > 0 {
			params["limit"] = limit - len(out)
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := h.call(ctx, "thread/turns/list", params)
		if err != nil {
			return nil, false, err
		}
		var answer struct {
			Data       []turnWire `json:"data"`
			NextCursor *string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, false, fmt.Errorf("sessionhost: thread/turns/list of %s: %w", threadID, err)
		}
		out = append(out, answer.Data...)
		last := answer.NextCursor == nil || *answer.NextCursor == "" || len(answer.Data) == 0
		if limit > 0 && len(out) >= limit {
			return out[:limit], len(out) > limit || !last, nil
		}
		if last {
			return out, false, nil
		}
		cursor = *answer.NextCursor
	}
}

// turnItems reads every item of one turn with thread/items/list.
func (h *Host) turnItems(ctx context.Context, threadID, turnID string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("sessionhost: thread/items/list of turn %s: more than %d pages", turnID, maxPages)
		}
		params := map[string]any{"threadId": threadID, "turnId": turnID, "limit": pageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := h.call(ctx, "thread/items/list", params)
		if err != nil {
			return nil, err
		}
		var answer struct {
			Data []struct {
				Item json.RawMessage `json:"item"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return nil, fmt.Errorf("sessionhost: thread/items/list of turn %s: %w", turnID, err)
		}
		for _, d := range answer.Data {
			out = append(out, d.Item)
		}
		if answer.NextCursor == nil || *answer.NextCursor == "" || len(answer.Data) == 0 {
			return out, nil
		}
		cursor = *answer.NextCursor
	}
}

func (w turnWire) turn() Turn {
	t := Turn{ID: w.ID, Status: w.Status, ItemsView: w.ItemsView}
	t.FirstUserText, t.Marker = firstUser(w.Items)
	t.FinalText, t.FinalPhase, t.Answered = FinalAnswer(w.Items)
	if len(w.Error) > 0 && string(w.Error) != "null" {
		var e struct {
			Message        string `json:"message"`
			CodexErrorInfo any    `json:"codexErrorInfo"`
		}
		if json.Unmarshal(w.Error, &e) == nil {
			t.ErrorInfo = &TurnError{Message: e.Message, CodexErrorInfo: infoName(e.CodexErrorInfo)}
		}
	}
	switch {
	case w.CompletedAt != nil:
		t.UpdatedAt = time.Unix(*w.CompletedAt, 0).UTC()
	case w.StartedAt != nil:
		t.UpdatedAt = time.Unix(*w.StartedAt, 0).UTC()
	}
	return t
}

// firstUser is the text of the first userMessage item and the operation it names: its clientId when that is an operation
// (the App Server backend sends the op as clientUserMessageId), else the marker on its first line.
func firstUser(items []json.RawMessage) (text, marker string) {
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
		var b strings.Builder
		for _, part := range item.Content {
			if part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		text = b.String()
		if item.ClientID != nil {
			if m, ok := ParseOp(*item.ClientID); ok {
				return text, m.Op()
			}
		}
		if m, ok := ParseMarker(text); ok {
			return text, m.Op()
		}
		return text, ""
	}
	return "", ""
}

// infoName is the variant name of a CodexErrorInfo: a plain name, or the one key of an object variant.
func infoName(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if len(x) == 1 {
			for name := range x {
				return name
			}
		}
	}
	return ""
}
