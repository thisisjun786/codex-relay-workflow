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
