package appserver

import "encoding/json"

// Option configures a Client at construction.
type Option func(*Client)

// AnsweredByResponder is the Answered value of a request the opt-in responder answered. The values a
// client without a responder records ("refused", "left_for_thread_approver") are unchanged.
const AnsweredByResponder = "denied_by_responder"

// ServerRequest is one server-to-client request, as the responder sees it.
type ServerRequest struct {
	Method   string
	ThreadID string
	TurnID   string
	Params   json.RawMessage
}

// ServerAnswer is the response to a server-to-client request: a result or a JSON-RPC error, not both.
type ServerAnswer struct {
	Result map[string]any
	Error  *ServerError
}

// ServerError is the error member of a response to a server-to-client request.
type ServerError struct {
	Code    int
	Message string
}

type serverResponder struct {
	owned  func(threadID string) bool
	answer func(ServerRequest) (ServerAnswer, bool)
}

// WithServerRequestResponder installs the flow backend's opt-in answerer of server-to-client requests.
//
// owned says whether a thread id is one the backend started and journalled; it is asked once per
// request, so ownership that ends takes effect at the next request. answer returns the response for
// a request about an owned thread, or false to leave that request to the default handling. Both run
// on the connection's reader: neither may block.
//
// The responder only refuses. An answer is sent, and recorded as AnsweredByResponder, only when it
// refuses the request (Refuses): a JSON-RPC error, or the method's own decline form. Any other
// answer, a grant included, is not sent and the request takes the default handling, so the record
// of a denial is never kept for a request that was granted.
//
// Only a request that carries a threadId the backend owns reaches answer. A request without a
// threadId, or about any other thread of the same connection, is recorded and handled exactly as it
// is by a client without a responder: an approval-class request stays with the thread's own approver
// and every other request is refused. A client built without this option is that default client.
func WithServerRequestResponder(owned func(threadID string) bool, answer func(ServerRequest) (ServerAnswer, bool)) Option {
	return func(c *Client) {
		if owned == nil || answer == nil {
			c.responder = nil
			return
		}
		c.responder = &serverResponder{owned: owned, answer: answer}
	}
}

// flowDenial is what a refused request tells the model: a flow run has nobody to approve.
const flowDenial = "This thread runs under a CRW flow backend, which has no user to approve or answer. The request was refused and nothing was run."

// DenyAnswer is the flow backend's answer: a refusal, in the form the protocol gives each method, for
// the approval-class requests (the seven methods the default path leaves to a thread's approver).
// Any other method is not answered (false), so it takes the default handling.
func DenyAnswer(request ServerRequest) (ServerAnswer, bool) {
	switch request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return ServerAnswer{Result: map[string]any{"decision": "decline"}}, true
	case "mcpServer/elicitation/request":
		return ServerAnswer{Result: map[string]any{"action": "decline"}}, true
	case "item/permissions/requestApproval":
		// An empty grant is the refusal: nothing is permitted for the turn.
		return ServerAnswer{Result: map[string]any{"permissions": map[string]any{}, "scope": "turn"}}, true
	case "execCommandApproval", "applyPatchApproval":
		return ServerAnswer{Result: map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": flowDenial}}}}, true
	case "item/tool/requestUserInput":
		// The protocol has no decline form for a question; a JSON-RPC error is the unambiguous refusal.
		return ServerAnswer{Error: &ServerError{Code: -32601, Message: flowDenial}}, true
	}
	return ServerAnswer{}, false
}

// Refuses reports whether answer refuses request, which is the only kind of answer the responder
// sends. A JSON-RPC error refuses (it grants nothing). A result refuses when it is the method's
// decline form in the generated codex schema: decline or cancel for a command or file-change
// approval and for an elicitation, no permission granted for a permissions request, denied or
// abort for a legacy approval. A result for any other method (a user's answer, a tool's output) is
// not a refusal.
func Refuses(request ServerRequest, answer ServerAnswer) bool {
	if answer.Error != nil {
		return true
	}
	declines := func(member string) bool {
		v, _ := answer.Result[member].(string)
		return v == "decline" || v == "cancel"
	}
	switch request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return declines("decision")
	case "mcpServer/elicitation/request":
		return declines("action")
	case "item/permissions/requestApproval":
		granted, ok := answer.Result["permissions"].(map[string]any)
		if !ok {
			return false
		}
		for _, grant := range granted {
			if grant != nil {
				return false
			}
		}
		return true
	case "execCommandApproval", "applyPatchApproval":
		switch decision := answer.Result["decision"].(type) {
		case string:
			return decision == "abort"
		case map[string]any:
			_, denied := decision["denied"]
			return denied && len(decision) == 1
		}
	}
	return false
}
