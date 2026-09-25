package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/websocket"
)

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) receive(ws *websocket.Conn) {
	var failure error
	for {
		_, raw, err := ws.Read(context.Background())
		if err != nil {
			failure = fmt.Errorf("App Server transport failed: %w", err)
			if errors.Is(err, websocket.ErrMessageTooBig) {
				failure = &ResponseTooLarge{FrameBytes: MaxFrameBytes + 1, Limit: MaxFrameBytes}
			}
			break
		}
		var msg incoming
		if err := json.Unmarshal(raw, &msg); err != nil {
			failure = fmt.Errorf("App Server invalid frame: %w", err)
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
	entry := RequestRecord{c.total, msg.Method, approval, params.ThreadID, params.TurnID, answered}
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
