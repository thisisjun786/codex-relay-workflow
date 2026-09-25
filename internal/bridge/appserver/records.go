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
	Seconds       time.Duration
}

func (e *PhaseTimeout) Error() string {
	return fmt.Sprintf("%s: %s phase exceeded %s; response unavailable; do not resend", e.Method, e.Phase, e.Seconds)
}

type ResponseTooLarge struct{ FrameBytes, Limit int }

func (e *ResponseTooLarge) Error() string {
	return fmt.Sprintf("App Server response exceeded %d byte limit (at least %d bytes); frame refused before its id was read", e.Limit, e.FrameBytes)
}

type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s: %s", e.Method, e.Message) }

type TransportError struct{ Reason string }

func (e *TransportError) Error() string { return e.Reason }

type RequestRecord struct {
	Index    uint64 `json:"index"`
	Method   string `json:"method"`
	Approval bool   `json:"approval"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Answered string `json:"answered"`
}
type RequestReport struct {
	ThisThread                 []RequestRecord `json:"thisThread"`
	Unattributed               []RequestRecord `json:"unattributed"`
	OtherThreads               int             `json:"otherThreads"`
	ApprovalsLeftForThisThread int             `json:"approvalsLeftForThisThread"`
	RefusedForThisThread       int             `json:"refusedForThisThread"`
	NotRetained                uint64          `json:"notRetained"`
}
type response struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type outcome struct {
	response response
	err      error
}
type pending struct {
	conn *websocket.Conn
	done chan outcome
}

// Client owns a single connection and correlates concurrent calls by numeric or string IDs.
type Client struct {
	socket        string
	bounds        PhaseBounds
	connectGate   chan struct{}
	writeFrame    func(*websocket.Conn, context.Context, websocket.MessageType, []byte) error
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

func (c *Client) Notifications() <-chan Notification { return c.notifications }
func (c *Client) RequestMark() uint64                { c.mu.Lock(); defer c.mu.Unlock(); return c.total }
func (c *Client) RequestsSince(mark uint64, threadID string) RequestReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	report := RequestReport{ThisThread: []RequestRecord{}, Unattributed: []RequestRecord{}}
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
