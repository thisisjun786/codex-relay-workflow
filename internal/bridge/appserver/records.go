package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const MaxFrameBytes = 16 * 1024 * 1024
const requestsKept = 64

var approvalMethods = map[string]bool{
	"execCommandApproval": true, "applyPatchApproval": true,
	"item/commandExecution/requestApproval": true, "item/fileChange/requestApproval": true,
	"item/permissions/requestApproval": true, "mcpServer/elicitation/request": true,
	"item/tool/requestUserInput": true,
}

// legacyApprovalMethods are the two approvals the codex schema raises with a conversationId in place of a threadId.
var legacyApprovalMethods = map[string]bool{"execCommandApproval": true, "applyPatchApproval": true}

type PhaseBounds struct{ Establish, Transmit, Ack time.Duration }

var DefaultBounds = PhaseBounds{20 * time.Second, 20 * time.Second, 20 * time.Second}

// ackBoundKey carries a per-call ack bound on the context. A call whose context holds one uses it
// instead of the client's PhaseBounds.Ack; the establish and transmit bounds are unchanged. The
// managed create path is the one caller: under App Server load its thread/start, thread/name/set
// and turn/start answers have taken longer than the default 20 s, and a lost answer leaves a
// creation receipt that says outcome_unknown while the host did the work.
type ackBoundKey struct{}

// WithAckBound returns a context whose App Server calls acknowledge within d. A non-positive d is
// ignored, so a caller cannot accidentally disable the bound.
func WithAckBound(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ackBoundKey{}, d)
}

// ackBound is the ack bound this call uses: the context's when it holds a positive one, else the
// client's stage bound for this method when a test set one (BoundAckOn), else the client's configured
// bound. A caller that already holds an earlier deadline keeps it, so a raised
// bound never reports itself as the wait that expired.
func (c *Client) ackBound(ctx context.Context, method string) time.Duration {
	d, ok := ctx.Value(ackBoundKey{}).(time.Duration)
	if !ok || d <= 0 {
		c.mu.Lock()
		stage, staged := c.stageAck[method]
		c.mu.Unlock()
		if staged {
			return stage
		}
		return c.bounds.Ack
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < d {
			return remaining
		}
	}
	return d
}

type PhaseTimeout struct {
	Method, Phase string
	Bound         time.Duration
}

func (e *PhaseTimeout) Error() string {
	return fmt.Sprintf("%s: %s phase exceeded %s; response unavailable; do not resend", e.Method, e.Phase, e.Bound)
}

type ResponseTooLarge struct {
	FrameBytes, Limit int
	// Methods is what was in flight when the connection went down: context, never attribution.
	Methods []string
}

func (e *ResponseTooLarge) Error() string {
	return fmt.Sprintf("App Server sent a response frame of at least %d bytes, past this client's %d byte limit, and the connection closed with 1009. The frame was refused before its id was read, so it cannot be attributed to a request. Ask again with a narrower query.", e.FrameBytes, e.Limit)
}

type RPCError struct {
	Method  string
	Code    int
	Message string
	// Object is the host's error member verbatim, whose code may be a string (rpc.py RpcError.error).
	Object map[string]any
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s: %s", e.Method, e.Message) }

type TransportError struct {
	Reason string
	// withheld marks an error the client returned before the request's frame was written: nothing
	// reached the host.
	withheld bool
}

func (e *TransportError) Error() string { return e.Reason }

// Withheld reports whether err is a request the client refused before writing its frame, so the
// host certainly never received it.
func Withheld(err error) bool {
	var transport *TransportError
	return errors.As(err, &transport) && transport.withheld
}

type RequestRecord struct {
	Index    uint64
	Method   string
	Approval bool
	ThreadID string
	TurnID   string
	Answered string
	// At is when the request arrived, in seconds since the epoch (rpc.py time.time()).
	At float64
}

// MarshalJSON writes the record as rpc.py records it: an absent threadId or turnId is null.
func (r RequestRecord) MarshalJSON() ([]byte, error) {
	optional := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return json.Marshal(map[string]any{"index": r.Index, "method": r.Method, "approval": r.Approval, "threadId": optional(r.ThreadID), "turnId": optional(r.TurnID), "answered": r.Answered, "at": r.At})
}

type RequestReport struct {
	ThisThread                 []RequestRecord `json:"thisThread"`
	Unattributed               []RequestRecord `json:"unattributed"`
	OtherThreads               int             `json:"otherThreads"`
	ApprovalsLeftForThisThread int             `json:"approvalsLeftForThisThread"`
	RefusedForThisThread       int             `json:"refusedForThisThread"`
	NotRetained                uint64          `json:"notRetained"`
	Note                       string          `json:"note"`
	// DeniedByResponderForThisThread counts requests an opt-in responder answered. A client without a
	// responder never sets it, and the member is then absent from the report.
	DeniedByResponderForThisThread int `json:"deniedByResponderForThisThread,omitempty"`
}

const requestsNote = "Requests recorded between the two marks this receipt spans. A turn outlives that window, so a request the turn raises later is handled the same way and is simply not on this receipt. An approval-class request is never answered here: the host sends it to every client subscribed to the thread and applies the first answer, so it stays with the thread's own approver. Nothing here was granted or denied."

// responderRequestsNote is the note of a report that holds a request the opt-in responder denied. It
// says what the responder did, so it does not say that nothing was denied.
const responderRequestsNote = "Requests recorded between the two marks this receipt spans. A turn outlives that window, so a request the turn raises later is handled the same way and is simply not on this receipt. An approval-class request about a thread the flow backend started was denied by the backend's responder (answered \"denied_by_responder\", counted in deniedByResponderForThisThread): the run has no user to approve, and nothing was granted. An approval-class request about any other thread is not answered here: the host sends it to every client subscribed to the thread and applies the first answer, so it stays with the thread's own approver."

type response struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type outcome struct {
	response response
	err      error
}
type pending struct {
	conn   *websocket.Conn
	done   chan outcome
	method string
	watch  *TurnWatch
}

// Client owns a single connection and correlates concurrent calls by numeric or string IDs.
type Client struct {
	socket        string
	bounds        PhaseBounds
	stageAck      map[string]time.Duration
	maxFrame      int
	info          map[string]any
	connectGate   chan struct{}
	writeFrame    func(*websocket.Conn, context.Context, websocket.MessageType, []byte) error
	beforeWrite   func(string) error
	closeFrame    func(*websocket.Conn) error
	mu            sync.Mutex
	conn          *websocket.Conn
	networkConn   net.Conn
	connSeq       uint64
	connID        uint64
	counter       uint64
	pending       map[string]pending
	requests      []RequestRecord
	refusals      []RequestRecord
	total         uint64
	notifications chan Notification
	subscriptions *subscriptionManager
	bus           *Bus
	// responder is the opt-in answerer of server requests about threads its owner started; nil for a
	// client that was not given one, which then handles every request as it always has.
	responder *serverResponder
}
type Notification struct {
	Method string
	Params json.RawMessage
}

// LimitFrames sets the largest response frame this client buffers (rpc.py max_frame_bytes).
// It applies to connections established afterwards.
func (c *Client) LimitFrames(bytes int) *Client { c.maxFrame = bytes; return c }

// FailBeforeWrite injects a one-shot failure before a method's frame reaches the socket.
// Intended for fake-host integration tests; it must be installed before concurrent calls.
func (c *Client) FailBeforeWrite(method string) {
	c.mu.Lock()
	c.beforeWrite = func(actual string) error {
		if actual == method {
			c.beforeWrite = nil
			return &TransportError{Reason: method + ": simulated failure before write", withheld: true}
		}
		return nil
	}
	c.mu.Unlock()
}

// RetireBeforeWrite injects, once, the loss of the current connection as a reader sees it, at the
// moment a method's request is past its first checks and before its frame is written: the watches
// on that socket retire exactly as when the reader loses it. Intended for fake-host integration
// tests of the window between the retire check and the write; it must be installed before
// concurrent calls.
func (c *Client) RetireBeforeWrite(method string) {
	c.mu.Lock()
	c.beforeWrite = func(actual string) error {
		if actual == method {
			c.beforeWrite = nil
			c.subscriptions.lost(c.conn)
		}
		return nil
	}
	c.mu.Unlock()
}

// BoundAck sets the acknowledgement deadline for subsequently sent requests.
func (c *Client) BoundAck(duration time.Duration) { c.bounds.Ack = duration }

// BoundAckOn sets the acknowledgement deadline of one method's requests, leaving every other method on
// the client's own bound. It is the test seam for an interruption aimed at one stage of a multi-call
// operation, which BoundAck (every request) cannot aim; production never sets it. A non-positive
// duration clears the stage bound.
func (c *Client) BoundAckOn(method string, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if duration <= 0 {
		delete(c.stageAck, method)
		return
	}
	if c.stageAck == nil {
		c.stageAck = map[string]time.Duration{}
	}
	c.stageAck[method] = duration
}

// Info is the initialize result of the current connection (rpc.py AppServer.info).
func (c *Client) Info() map[string]any { c.mu.Lock(); defer c.mu.Unlock(); return c.info }

// ConnectionID identifies the connection the client holds now: every connection it makes has an id of its own, ids are
// never reused, and 0 means the client holds none. A request sent on a connection whose id is no longer the current one
// (or 0) was carried by a connection that is closed.
func (c *Client) ConnectionID() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return 0
	}
	return c.connID
}

// ConnectionSnapshot borrows the actual transport of the current WebSocket.
// Observers must not read, write or close it. The generation must still match
// after an operation before its result can be attributed to this connection.
func (c *Client) ConnectionSnapshot() (net.Conn, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.networkConn == nil {
		return nil, 0
	}
	return c.networkConn, c.connID
}

// SocketPath is the unix socket this client dials.
func (c *Client) SocketPath() string { return c.socket }

type sendHookKey struct{}

// WithSendHook returns a context whose calls report each request frame just before it is written
// (effects.py mark_sent): the only point that knows a frame was about to go out.
func WithSendHook(ctx context.Context, hook func(method string)) context.Context {
	return context.WithValue(ctx, sendHookKey{}, hook)
}

type outcomeHookKey struct{}

// WithOutcomeHook installs a test seam after a request selects a reader outcome but before it
// interprets it. This forces cancellation to race a response or disconnect without relying on
// which ready select case the scheduler chooses.
func WithOutcomeHook(ctx context.Context, hook func(method string)) context.Context {
	return context.WithValue(ctx, outcomeHookKey{}, hook)
}

func (c *Client) Notifications() <-chan Notification { return c.notifications }
func (c *Client) RequestMark() uint64                { c.mu.Lock(); defer c.mu.Unlock(); return c.total }
func (c *Client) RequestsSince(mark uint64, threadID string) RequestReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	report := RequestReport{ThisThread: []RequestRecord{}, Unattributed: []RequestRecord{}, Note: requestsNote}
	var retained uint64
	for _, r := range c.requests {
		if r.Index <= mark {
			continue
		}
		retained++
		switch {
		case r.ThreadID == threadID:
			report.ThisThread = append(report.ThisThread, r)
			switch {
			case r.Answered == AnsweredByResponder:
				report.DeniedByResponderForThisThread++
			case r.Approval:
				report.ApprovalsLeftForThisThread++
			default:
				report.RefusedForThisThread++
			}
		case r.ThreadID == "":
			report.Unattributed = append(report.Unattributed, r)
		default:
			report.OtherThreads++
		}
	}
	if c.total > mark+retained {
		report.NotRetained = c.total - mark - retained
	}
	if report.DeniedByResponderForThisThread > 0 {
		report.Note = responderRequestsNote
	}
	return report
}
func (c *Client) RefusalsSince(mark uint64, threadID string) []RequestRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []RequestRecord{}
	for _, r := range c.refusals {
		if r.Index > mark && r.ThreadID == threadID {
			out = append(out, r)
		}
	}
	return out
}
