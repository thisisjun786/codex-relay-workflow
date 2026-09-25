package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (c *Client) receive(ws *websocket.Conn) {
	var failure error
	for {
		_, raw, err := ws.Read(context.Background())
		if err != nil {
			failure = &TransportError{Reason: fmt.Sprintf("App Server transport failed: %v", err)}
			if errors.Is(err, websocket.ErrMessageTooBig) {
				failure = &ResponseTooLarge{FrameBytes: c.maxFrame + 1, Limit: c.maxFrame, Methods: c.carried(ws)}
			}
			break
		}
		var msg incoming
		if err := json.Unmarshal(raw, &msg); err != nil {
			// rpc.py:362: a frame that is not JSON reaches Python's generic handler.
			failure = &TransportError{Reason: fmt.Sprintf("App Server transport failed: JSONDecodeError: %v", err)}
			break
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 {
				c.serverRequest(ws, msg)
			} else {
				select {
				case c.notifications <- Notification{msg.Method, msg.Params}:
				default:
					// Notification history is intentionally bounded; a slow subscriber loses old events.
				}
			}
			continue
		}
		key := string(msg.ID)
		var id any
		if err := json.Unmarshal(msg.ID, &id); err == nil {
			key = fmt.Sprint(id)
		}
		c.mu.Lock()
		waiter, ok := c.pending[key]
		c.mu.Unlock()
		if ok && waiter.conn == ws {
			select {
			case waiter.done <- outcome{response: response{msg.Result, msg.Error}}:
			default:
			}
		}
	}
	c.failReader(ws, failure)
	_ = ws.CloseNow()
}

// carried lists the methods still pending on ws, in no particular order.
func (c *Client) carried(ws *websocket.Conn) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	methods := []string{}
	for _, p := range c.pending {
		if p.conn == ws {
			methods = append(methods, p.method)
		}
	}
	return methods
}

// rpcError keeps the host's error member verbatim; Code and Message are read where typed.
func rpcError(method string, raw json.RawMessage) *RPCError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		object = map[string]any{"message": string(raw)}
	}
	e := &RPCError{Method: method, Object: object}
	if n, ok := object["code"].(json.Number); ok {
		if code, err := n.Int64(); err == nil {
			e.Code = int(code)
		}
	}
	e.Message, _ = object["message"].(string)
	return e
}

func (c *Client) failReader(ws *websocket.Conn, failure error) {
	c.mu.Lock()
	if c.conn == ws {
		c.conn = nil
	}
	for _, p := range c.pending {
		if p.conn == ws {
			select {
			case p.done <- outcome{err: failure}:
			default:
			}
		}
	}
	c.mu.Unlock()
}

func (c *Client) serverRequest(ws *websocket.Conn, msg incoming) {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	approval := approvalMethods[msg.Method]
	answered := "refused"
	if approval {
		answered = "left_for_thread_approver"
	}
	c.mu.Lock()
	c.total++
	entry := RequestRecord{c.total, msg.Method, approval, params.ThreadID, params.TurnID, answered, float64(time.Now().UnixNano()) / 1e9}
	c.requests = append(c.requests, entry)
	if len(c.requests) > requestsKept {
		c.requests = c.requests[1:]
	}
	if !approval {
		c.refusals = append(c.refusals, entry)
		if len(c.refusals) > requestsKept {
			c.refusals = c.refusals[1:]
		}
	}
	c.mu.Unlock()
	if approval {
		return
	}
	var id any
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		c.retire(ws)
		return
	}
	if err := c.write(context.Background(), ws, map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Unsupported client action; this bridge runs no client-side tool. Nothing was run."}}, msg.Method); err != nil {
		c.retire(ws)
	}
}
