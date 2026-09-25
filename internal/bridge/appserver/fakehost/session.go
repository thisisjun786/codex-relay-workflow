package fakehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// session is one client connection. Frames are read and recorded in arrival order; an answer
// with a Delay is sent from its own goroutine so a later request can overtake it.
type session struct {
	server      *Server
	conn        *websocket.Conn
	initialized bool
	delayed     sync.WaitGroup
}

// envelope is a frame the fake accepted: only the App Server members are read.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method *string         `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (c *session) serve(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer c.delayed.Wait()
	defer cancel()
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				c.server.record(func(s *Server) {
					s.malformed = append(s.malformed, Malformed{Reason: fmt.Sprintf("frame exceeds %d bytes", MaxFrameBytes)})
				})
			}
			return
		}
		c.dispatch(ctx, raw)
	}
}

func (c *session) dispatch(ctx context.Context, raw []byte) {
	message, reason := parse(raw)
	if reason != "" {
		c.server.record(func(s *Server) { s.malformed = append(s.malformed, Malformed{Raw: raw, Reason: reason}) })
		return
	}
	if message.Method == nil {
		// The client's answer to a server-to-client request: kept, because what a client
		// replies to an approval request is exactly what a test has to prove.
		c.server.record(func(s *Server) { s.answers = append(s.answers, Answer{ID: message.ID, Raw: raw}) })
		return
	}
	method := *message.Method
	c.server.record(func(s *Server) {
		s.requests = append(s.requests, Request{ID: message.ID, Method: method, Params: message.Params})
	})
	if message.ID == nil {
		return
	}
	reply, scripted := c.server.reply(method)
	switch {
	case method == "initialize":
		if !scripted {
			reply = Reply{Result: map[string]any{"userAgent": UserAgent, "platformOs": "linux"}}
		}
		c.initialized = reply.Error == nil
	case !c.initialized:
		reply = Reply{Error: &RPCError{Message: "not initialized"}}
	case !scripted:
		reply = Reply{Error: &RPCError{Code: -32601, Message: method}}
	}
	if reply.Delay <= 0 {
		c.answer(ctx, message, reply)
		return
	}
	c.delayed.Go(func() {
		timer := time.NewTimer(reply.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			c.answer(ctx, message, reply)
		}
	})
}

// parse accepts a JSON object carrying no "jsonrpc" member, or names why it is refused.
func parse(raw []byte) (envelope, string) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return envelope{}, "not a JSON object"
	}
	if _, ok := members["jsonrpc"]; ok {
		return envelope{}, "jsonrpc member is outside the App Server envelope"
	}
	var message envelope
	if err := json.Unmarshal(raw, &message); err != nil {
		return envelope{}, "envelope member has the wrong type"
	}
	if message.Method == nil && message.ID == nil {
		return envelope{}, "neither method nor id"
	}
	return message, ""
}

func (c *session) answer(ctx context.Context, message envelope, reply Reply) {
	for _, notification := range reply.Before {
		c.send(ctx, map[string]any{"method": notification.Method, "params": orEmpty(notification.Params)})
	}
	for _, method := range reply.ServerRequests {
		var params struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(message.Params, &params)
		c.send(ctx, map[string]any{
			"id":     c.server.nextServerRequest(method),
			"method": method,
			"params": map[string]any{"threadId": params.ThreadID, "turnId": "turn-waiting", "command": []string{"rm", "-rf", "/"}},
		})
	}
	if reply.Close != nil {
		_ = c.conn.Close(reply.Close.Code, reply.Close.Reason)
		return
	}
	if reply.Error != nil {
		c.send(ctx, map[string]any{"id": message.ID, "error": reply.Error})
		return
	}
	result := orEmpty(reply.Result)
	if reply.PadBytes > 0 {
		padded := make(map[string]any, len(result)+1)
		for key, value := range result {
			padded[key] = value
		}
		padded["padding"] = string(bytes.Repeat([]byte{'x'}, reply.PadBytes))
		result = padded
	}
	c.send(ctx, map[string]any{"id": message.ID, "result": result})
}

// send writes one frame. A failed write means the client went away, which ends the session
// through its read loop; the fake has nobody to report it to.
func (c *session) send(ctx context.Context, frame map[string]any) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		panic(fmt.Sprintf("fakehost: scripted frame is not JSON: %v", err))
	}
	_ = c.conn.Write(ctx, websocket.MessageText, encoded)
}

func orEmpty(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	return params
}
