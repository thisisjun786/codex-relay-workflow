package host

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// TranscriptGeneration is what the R-11 guards read from a transcript tail: the Codex rollout records after the last
// compaction the tail shows (CRW-1090). The oracle searched the raw tail for marker text, so a user who quoted
// `[crw: PLAN]`, an answer that echoed it, a tool output, or the compacted record's own histories (which keep the text the
// compaction removed from the model's context) all counted; a generation counts only the records a hook or Codex itself
// wrote, and only those the model still sees.
//
// The record shapes are the ones the isolated trial's Codex rollouts hold (S4-resilience, 10-10): a hook's
// additionalContext is a `response_item` message of role developer whose metadata names `hooks.additional_context`; a
// compaction is a `compacted` record followed by an `event_msg` item_completed of item type ContextCompaction; a user
// prompt is a `response_item` message of role user whose metadata names `user.text`, with an `event_msg` item_completed of
// item type UserMessage. A developer record counts as a hook's only when its metadata names `hooks.additional_context`: a
// record without metadata, with null metadata or with metadata of another kind is not attributable to a hook, so it is no
// marker and the state cursor decides (a missing marker costs one re-injection, a wrong one skips the directive). A user
// record without metadata is still read by its text, which only decides when the compaction's recovery window ends.
type TranscriptGeneration struct {
	compacted bool     // the tail shows a compaction
	userTurn  bool     // a user prompt was recorded after the last compaction the tail shows
	injected  []string // the texts of the hook developer records after the last compaction
}

// ReadTranscriptGeneration reads the whole JSONL records in the last maxBytes of the transcript at path, touching at most
// maxBytes bytes (readTranscriptBytes). Nothing readable is an empty generation: no marker, no pressure, no user turn (fail
// open, as ReadTranscriptTail).
func ReadTranscriptGeneration(path string, maxBytes int) TranscriptGeneration {
	w := readTranscriptBytes(path, maxBytes, nil)
	return parseTranscriptGeneration(w.data, w.whole)
}

// ParseTranscriptGeneration reads tail as JSONL records. whole says the tail's first line is a whole record (the tail
// starts the transcript or the line after a newline); when it is not, that line is the cut end of a longer record and is
// dropped, so only whole records count. A line that is not a JSON object is skipped.
func ParseTranscriptGeneration(tail string, whole bool) TranscriptGeneration {
	return parseTranscriptGeneration([]byte(tail), whole)
}

func parseTranscriptGeneration(tail []byte, whole bool) TranscriptGeneration {
	var g TranscriptGeneration
	for first := true; len(tail) > 0; first = false {
		line := tail
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			line, tail = tail[:i], tail[i+1:]
		} else {
			tail = nil
		}
		if first && !whole {
			continue
		}
		var record transcriptRecord
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &record) != nil {
			continue
		}
		switch {
		case record.isCompaction():
			g = TranscriptGeneration{compacted: true}
		case record.isUserPrompt():
			g.userTurn = true
		default:
			if texts, ok := record.hookContext(); ok {
				g.injected = append(g.injected, texts...)
			}
		}
	}
	return g
}

// HasStageMarkerForPhase is whether a hook injected the stage marker for phase in this generation, in either emitted
// form: the directive head `[crw: PLAN]` or the compaction-immune header `[crw — P: PLAN]` (the oracle's `[codexclaw...`
// markers after name-substitution rule R23). The marker has to open the injected text, as the leg emits it; another
// hook's context that quotes it (recall's untrusted data) is not the marker.
func (g TranscriptGeneration) HasStageMarkerForPhase(phase string) bool {
	label := stageMarkerLabel(phase)
	if label == "" {
		return false
	}
	return slices.ContainsFunc(g.injected, func(text string) bool {
		text = strings.TrimLeft(text, " \t\r\n")
		return strings.HasPrefix(text, "[crw: "+label+"]") || strings.HasPrefix(text, "[crw — "+phase+": "+label+"]")
	})
}

// ContextPressure is whether the tail shows a compaction that no user prompt has followed yet: the window in which the
// compacted context is being recovered. The next user prompt is its boundary, so a UserPromptSubmit hook, which runs
// before Codex records that prompt, is the boundary itself and not under pressure, and a Stop after a recorded prompt is
// past it.
func (g TranscriptGeneration) ContextPressure() bool {
	return g.compacted && !g.userTurn
}

// UserTurnRecorded is whether the tail shows a user prompt after the last compaction it shows (any prompt, when it shows
// none). Any compaction before the tail is older than every record in it, so such a prompt ends a recovery window whose
// compaction has scrolled out of the tail; a tail that shows neither a compaction nor a prompt cannot say where the
// window stands, and the Stop asks the boundary a hook recorded (hook/compaction_recovery.go).
func (g TranscriptGeneration) UserTurnRecorded() bool {
	return g.userTurn
}

// stageMarkerLabel is STAGE_LABELS for a work phase, or "" for one that has no stage marker.
func stageMarkerLabel(phase string) string {
	switch phase {
	case "I":
		return "INTERVIEW"
	case "P":
		return "PLAN"
	case "A":
		return "AUDIT"
	case "B":
		return "BUILD"
	case "C":
		return "CHECK"
	case "D":
		return "DONE"
	}
	return ""
}

// transcriptRecord is the part of a rollout line the guards read.
type transcriptRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Item    struct {
			Type string `json:"type"`
		} `json:"item"`
		Metadata *struct {
			Kinds []string `json:"content_item_kinds"`
		} `json:"internal_chat_message_metadata_passthrough"`
	} `json:"payload"`
}

// isCompaction is a compacted record, or the event Codex writes for one (item_completed of a ContextCompaction item, or
// the context_compacted event of the rollouts that predate items).
func (r transcriptRecord) isCompaction() bool {
	return r.Type == "compacted" || r.Type == "event_msg" && (r.Payload.Type == "context_compacted" || r.Payload.Item.Type == "ContextCompaction")
}

// isUserPrompt is a prompt the user submitted: the user.text message, its UserMessage item, or the user_message event.
// The environment context Codex records as a user message after a compaction is not one.
func (r transcriptRecord) isUserPrompt() bool {
	switch r.Type {
	case "event_msg":
		return r.Payload.Type == "user_message" || r.Payload.Type == "item_completed" && r.Payload.Item.Type == "UserMessage"
	case "response_item":
		if r.Payload.Type != "message" || r.Payload.Role != "user" {
			return false
		}
		if r.Payload.Metadata != nil {
			return slices.Contains(r.Payload.Metadata.Kinds, "user.text")
		}
		texts := r.texts()
		return len(texts) > 0 && !strings.HasPrefix(strings.TrimSpace(texts[0]), "<")
	}
	return false
}

// hookContext is the text of a hook's additionalContext record: a developer message whose metadata names
// hooks.additional_context. Nothing else attributes a developer record to a hook, so a record without metadata is not one.
func (r transcriptRecord) hookContext() ([]string, bool) {
	if r.Type != "response_item" || r.Payload.Type != "message" || r.Payload.Role != "developer" {
		return nil, false
	}
	if r.Payload.Metadata == nil || !slices.Contains(r.Payload.Metadata.Kinds, "hooks.additional_context") {
		return nil, false
	}
	return r.texts(), true
}

// texts is the text of each content item of a message.
func (r transcriptRecord) texts() []string {
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(r.Payload.Content, &items) != nil {
		return nil
	}
	texts := make([]string, 0, len(items))
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	return texts
}
