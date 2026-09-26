package appserver

import (
	"context"
	"encoding/json"
	"fmt"
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

type PhaseBounds struct{ Establish, Transmit, Ack time.Duration }

var DefaultBounds = PhaseBounds{20 * time.Second, 20 * time.Second, 20 * time.Second}

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

type TransportError struct{ Reason string }

func (e *TransportError) Error() string { return e.Reason }

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
}

const requestsNote = "Requests recorded between the two marks this receipt spans. A turn outlives that window, so a request the turn raises later is handled the same way and is simply not on this receipt. An approval-class request is never answered here: the host sends it to every client subscribed to the thread and applies the first answer, so it stays with the thread's own approver. Nothing here was granted or denied."

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
}

// Client owns a single connection and correlates concurrent calls by numeric or string IDs.
type Client struct {
	socket        string
	bounds        PhaseBounds
	maxFrame      int
	info          map[string]any
	connectGate   chan struct{}
	writeFrame    func(*websocket.Conn, context.Context, websocket.MessageType, []byte) error
	beforeWrite   func(string) error
	closeFrame    func(*websocket.Conn) error
	mu            sync.Mutex
	conn          *websocket.Conn
	counter       uint64
	pending       map[string]pending
	requests      []RequestRecord
	refusals      []RequestRecord
	total         uint64
	notifications chan Notification
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
			return &TransportError{Reason: method + ": simulated failure before write"}
		}
		return nil
	}
	c.mu.Unlock()
}

// BoundAck sets the acknowledgement deadline for subsequently sent requests.
func (c *Client) BoundAck(duration time.Duration) { c.bounds.Ack = duration }

// Info is the initialize result of the current connection (rpc.py AppServer.info).
func (c *Client) Info() map[string]any { c.mu.Lock(); defer c.mu.Unlock(); return c.info }

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
			if r.Approval {
				report.ApprovalsLeftForThisThread++
			} else {
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
