package sessionhost

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The phases of an agent message (0.154 MessagePhase, nullable: a provider that sets none leaves it null).
const (
	PhaseFinalAnswer = "final_answer"
	PhaseCommentary  = "commentary"
)

// Message is an agent message of a turn: its phase ("" when null) and its text.
type Message struct {
	Phase, Text string
}

// ItemMessages are the agentMessage items among a turn's items, in order. An item that does not decode is skipped.
func ItemMessages(items []json.RawMessage) []Message {
	var out []Message
	for _, raw := range items {
		var item struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
			Text  string `json:"text"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "agentMessage" {
			out = append(out, Message{Phase: item.Phase, Text: item.Text})
		}
	}
	return out
}

// PickMessage is the App Server backend's choice of a turn's text (internal/flow/backend/agent): the last final answer
// among the messages, else the last message, whatever its phase.
func PickMessage(messages []Message) string {
	text, final := "", false
	for _, m := range messages {
		isFinal := m.Phase == PhaseFinalAnswer
		if isFinal || !final {
			text, final = m.Text, isFinal
		}
	}
	return text
}

// FinalAnswer is the answer of a session call's turn (CRW-1316 design 3.3): the last agent message of phase final_answer;
// from a provider that sets no phase, the last message whose phase is null, as the App Server backend takes a null-phase
// message for the answer when no commentary is in the way. A commentary message is never the answer: a turn with only
// commentary has none (ok false). phase is the chosen message's phase ("" for null).
func FinalAnswer(items []json.RawMessage) (text, phase string, ok bool) {
	messages := ItemMessages(items)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Phase == PhaseFinalAnswer {
			return messages[i].Text, PhaseFinalAnswer, true
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Phase == "" {
			return messages[i].Text, "", true
		}
	}
	return "", "", false
}

// MarkerPrefix starts the first line of a step's message and of an engine turn's input.
const MarkerPrefix = "crw-flow-op: "

// Marker is the operation a session step names: the call, the turn k and the attempt n of `<call>/t<k>-a<n>`. Attempt is
// 0 for the App Server backend's `<call>/t<k>`, which names no attempt.
type Marker struct {
	Call       string
	K, Attempt int
}

// Op is the operation's text: `<call>/t<k>-a<n>`, or `<call>/t<k>` without an attempt.
func (m Marker) Op() string {
	if m.Attempt == 0 {
		return fmt.Sprintf("%s/t%d", m.Call, m.K)
	}
	return fmt.Sprintf("%s/t%d-a%d", m.Call, m.K, m.Attempt)
}

// opPattern is an operation: a call id as the flow-bound files name one (internal/flow/bound ValidCallID), a turn number
// and an optional attempt number, each a positive number without leading zeros.
var opPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_-]{0,63})/t([1-9][0-9]{0,8})(?:-a([1-9][0-9]{0,8}))?$`)

// ParseOp reads an operation, `<call>/t<k>-a<n>` or `<call>/t<k>`.
func ParseOp(op string) (Marker, bool) {
	m := opPattern.FindStringSubmatch(op)
	if m == nil {
		return Marker{}, false
	}
	k, _ := strconv.Atoi(m[2])
	attempt := 0
	if m[3] != "" {
		attempt, _ = strconv.Atoi(m[3])
	}
	return Marker{Call: m[1], K: k, Attempt: attempt}, true
}

// ParseMarker reads the operation a message names on its first line, `crw-flow-op: <op>`; trailing white space on that
// line is ignored. A message whose first line is anything else names none.
func ParseMarker(text string) (Marker, bool) {
	line, _, _ := strings.Cut(text, "\n")
	rest, found := strings.CutPrefix(strings.TrimRight(line, " \t\r"), MarkerPrefix)
	if !found {
		return Marker{}, false
	}
	return ParseOp(rest)
}
