package sessionhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
)

// CollabCall is a collabAgentToolCall item of a turn: a multi-agent tool call (tool spawnAgent, sendInput, wait,
// closeAgent, ...) the thread made. For a spawn, ID is the tool use id the spawn hook records, ReceiverThreads the thread
// it created and Prompt the spawn message; Model and ReasoningEffort are what the call requested (nil for null).
type CollabCall struct {
	Type            string   `json:"type"`
	ID              string   `json:"id"`
	Tool            string   `json:"tool"`
	Status          string   `json:"status"`
	SenderThreadID  string   `json:"senderThreadId"`
	ReceiverThreads []string `json:"receiverThreadIds"`
	Model           *string  `json:"model"`
	ReasoningEffort *string  `json:"reasoningEffort"`
	Prompt          *string  `json:"prompt"`
}

// ToolSpawnAgent is the CollabAgentTool of a spawn.
const ToolSpawnAgent = "spawnAgent"

// DecodeCollab decodes one item; ok is false for an item that is no collabAgentToolCall.
func DecodeCollab(raw json.RawMessage) (CollabCall, bool) {
	var c CollabCall
	if json.Unmarshal(raw, &c) != nil || c.Type != "collabAgentToolCall" {
		return CollabCall{}, false
	}
	return c, true
}

// CollabCalls are the collabAgentToolCall items among a turn's items, in order.
func CollabCalls(items []json.RawMessage) []CollabCall {
	var out []CollabCall
	for _, raw := range items {
		if c, ok := DecodeCollab(raw); ok {
			out = append(out, c)
		}
	}
	return out
}

// How a spawn was tied to the op (Spawn.Via).
const (
	// ViaLedger: the hook's allowed decision for the tool use names the op.
	ViaLedger = "ledger"
	// ViaPrompt: the hook's allowed decision for the tool use names no op; the spawn message (the item's prompt) does.
	ViaPrompt = "prompt"
	// ViaUnrecorded: no allowed decision of the hook covers the tool use; the spawn message names the op.
	ViaUnrecorded = "unrecorded"
)

// The warnings of a search (each warning starts with its code).
const (
	WarnLedgerUnreadable = "ledger_unreadable"
	WarnNoLedgerLine     = "no_ledger_line"
	WarnRefusedDecision  = "refused_decision"
	WarnOpDiffers        = "ledger_op_differs"
	WarnSeveralSpawns    = "several_spawns"
	WarnSeveralReceivers = "several_receivers"
	WarnNoReceiver       = "no_receiver"
	WarnTurnUnreadable   = "owner_turn_unreadable"
	WarnWindowEnded      = "window_ended"
)

// Spawn is one spawn of the owner that names the op.
type Spawn struct {
	// ThreadID is the subagent the spawn created (receiverThreadIds).
	ThreadID string
	// ToolUseID is the collabAgentToolCall item's id; OwnerTurnID the owner turn that holds it.
	ToolUseID, OwnerTurnID string
	// Op is the operation the spawn is tied to, Via how (ViaLedger, ViaPrompt, ViaUnrecorded).
	Op, Via string
	// Decision is the hook's allowed decision for the tool use, nil for an unrecorded spawn.
	Decision *bound.Decision
}

// MarkerSearch is what FindByMarker found.
type MarkerSearch struct {
	Found bool
	// Spawn is the newest spawn of the op (ledger-tied spawns before the others).
	Spawn Spawn
	// Others are further spawns of the same op, newest first.
	Others []Spawn
	// Warnings say what the search could not tie as it should: they are recorded, never a refusal.
	Warnings []string
	// WindowEnded says the owner has turns older than the searched window (Host.OwnerTurns), which were not searched.
	WindowEnded bool
}

// FindByMarker finds the subagent the owner thread spawned for a published step (CRW-1316 design 5, step_published
// recovery): it joins the flow-bound decision ledger of the owner (decisions/<owner>.jsonl: tool_use_id and op of each
// spawn the hook decided) with the collabAgentToolCall items of the owner's recent turns (the item whose id is the tool use
// id names the created thread in receiverThreadIds), the same join ops.ImportNativeSpawns makes. A spawn whose decision
// records no op is tied by the op its message names (the item's prompt); a spawn no allowed decision covers is still found
// by its message and warned about (warn and record: a binding that went missing never hides a subagent that runs). A
// ledger that cannot be read is a warning too. Searched are the owner's Host.OwnerTurns most recent turns, read in full;
// when the owner has older turns, WindowEnded says so, and a search that finds nothing warns window_ended, so a spawn
// older than the window is not taken for one that never happened. An op that is no operation is an error, as is an owner
// the host cannot list.
func (h *Host) FindByMarker(ctx context.Context, ownerThread, op string) (MarkerSearch, error) {
	if _, ok := ParseOp(op); !ok {
		return MarkerSearch{}, fmt.Errorf("sessionhost: %q is not an operation <call>/t<k>-a<n>", op)
	}
	var out MarkerSearch
	allowed := map[string]bound.Decision{} // tool use id → the hook's last allowed decision
	refused := map[string]bool{}
	ledger, err := bound.ReadDecisions(h.boundDir, ownerThread)
	if err != nil {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s: the spawn ledger of %s: %v", WarnLedgerUnreadable, ownerThread, err))
	}
	for _, d := range ledger {
		switch {
		case d.ToolUseID == "":
		case d.Decision == bound.DecisionAllowed:
			allowed[d.ToolUseID] = d
		case d.Decision == bound.DecisionRefused:
			refused[d.ToolUseID] = true
		}
	}
	window := h.OwnerTurns
	if window <= 0 {
		window = DefaultOwnerTurns
	}
	turns, older, err := h.listTurns(ctx, ownerThread, window, true)
	if err != nil {
		return MarkerSearch{}, err
	}
	out.WindowEnded = older
	var tied, untied []Spawn
	seen := map[string]bool{}
	for _, turn := range turns { // newest first
		items := turn.Items
		if turn.ItemsView != "full" && turn.ItemsView != "" {
			if full, err := h.turnItems(ctx, ownerThread, turn.ID); err == nil {
				items = full
			} else {
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: turn %s of %s could not be read in full: %v", WarnTurnUnreadable, turn.ID, ownerThread, err))
			}
		}
		calls := CollabCalls(items)
		for i := len(calls) - 1; i >= 0; i-- { // newest first within the turn
			c := calls[i]
			if c.Tool != ToolSpawnAgent || c.ID == "" || seen[c.ID] || (c.SenderThreadID != "" && c.SenderThreadID != ownerThread) {
				continue
			}
			seen[c.ID] = true
			prompted := ""
			if c.Prompt != nil {
				if m, ok := ParseMarker(*c.Prompt); ok {
					prompted = m.Op()
				}
			}
			s := Spawn{ToolUseID: c.ID, OwnerTurnID: turn.ID}
			d, isAllowed := allowed[c.ID]
			switch {
			case isAllowed && d.Op != "":
				if d.Op != op {
					continue
				}
				s.Op, s.Via = d.Op, ViaLedger
				if prompted != "" && prompted != d.Op {
					out.Warnings = append(out.Warnings, fmt.Sprintf("%s: tool use %s: the ledger records %s, the spawn message names %s", WarnOpDiffers, c.ID, d.Op, prompted))
				}
			case prompted != op:
				continue
			case isAllowed:
				s.Op, s.Via = prompted, ViaPrompt
			default:
				s.Op, s.Via = prompted, ViaUnrecorded
				if refused[c.ID] {
					out.Warnings = append(out.Warnings, fmt.Sprintf("%s: tool use %s ran although the hook recorded it refused", WarnRefusedDecision, c.ID))
				} else {
					out.Warnings = append(out.Warnings, fmt.Sprintf("%s: tool use %s has no allowed decision in the spawn ledger of %s", WarnNoLedgerLine, c.ID, ownerThread))
				}
			}
			if isAllowed {
				dc := d
				s.Decision = &dc
			}
			switch len(c.ReceiverThreads) {
			case 0:
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: tool use %s created no thread (status %s)", WarnNoReceiver, c.ID, c.Status))
				continue
			case 1:
			default:
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: tool use %s names %d threads; the first is taken", WarnSeveralReceivers, c.ID, len(c.ReceiverThreads)))
			}
			s.ThreadID = c.ReceiverThreads[0]
			if s.Via == ViaUnrecorded {
				untied = append(untied, s)
			} else {
				tied = append(tied, s)
			}
		}
	}
	all := append(tied, untied...)
	if len(all) == 0 {
		if older {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: no spawn of %s in the %d most recent turns of %s; its older turns were not searched", WarnWindowEnded, op, window, ownerThread))
		}
		return out, nil
	}
	out.Found, out.Spawn, out.Others = true, all[0], all[1:]
	if len(out.Others) > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %d spawns name %s; the newest, %s (tool use %s), is taken", WarnSeveralSpawns, len(all), op, out.Spawn.ThreadID, out.Spawn.ToolUseID))
	}
	return out, nil
}
