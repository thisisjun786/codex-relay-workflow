// Package appserver implements the Codex App Server websocket transport over a unix socket.
package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// New constructs a reconnecting client; Dial also performs the initial handshake.
func New(socketPath string, bounds PhaseBounds) *Client {
	return &Client{socket: socketPath, bounds: bounds, maxFrame: MaxFrameBytes, pending: make(map[string]pending), notifications: make(chan Notification, 64), connectGate: make(chan struct{}, 1)}
}
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	c := New(socketPath, DefaultBounds)
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
func (c *Client) connect(ctx context.Context) error {
	select {
	case c.connectGate <- struct{}{}:
		defer func() { <-c.connectGate }()
	case <-ctx.Done():
		return fmt.Errorf("connection establishment: %w", ctx.Err())
	}
	c.mu.Lock()
	live := c.conn != nil
	c.mu.Unlock()
	if live {
		return nil
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: c.bounds.Establish}
	ws, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return fmt.Errorf("appserver dial: %w", err)
	}
	ws.SetReadLimit(int64(c.maxFrame))
	c.mu.Lock()
	c.conn = ws
	c.mu.Unlock()
	go c.receive(ws)
	handshake := map[string]any{"clientInfo": map[string]any{"name": "codex_thread_bridge", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}
	raw, err := c.request(ctx, ws, "initialize", handshake)
	if err != nil {
		c.retire(ws)
		return fmt.Errorf("initialize: %w", err)
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		c.retire(ws)
		return fmt.Errorf("initialize result: %w", err)
	}
	c.mu.Lock()
	c.info = info
	c.mu.Unlock()
	if err := c.write(ctx, ws, map[string]any{"method": "initialized", "params": map[string]any{}}, "initialized"); err != nil {
		c.retire(ws)
		return err
	}
	return nil
}

// Connect establishes the connection and handshake now rather than on the first call.
func (c *Client) Connect(ctx context.Context) error { return c.connect(ctx) }

func (c *Client) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	establish, cancel := context.WithTimeout(ctx, c.bounds.Establish)
	defer cancel()
	if err := c.connect(establish); err != nil {
		if errors.Is(establish.Err(), context.DeadlineExceeded) || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
			return nil, &PhaseTimeout{method, "establish", c.bounds.Establish}
		}
		var phase *PhaseTimeout
		if errors.As(err, &phase) {
			return nil, fmt.Errorf("%w: handshake %s phase: %v", &PhaseTimeout{method, "establish", c.bounds.Establish}, phase.Phase, err)
		}
		return nil, err
	}
	c.mu.Lock()
	ws := c.conn
	c.mu.Unlock()
	return c.request(ctx, ws, method, params)
}
func (c *Client) request(ctx context.Context, ws *websocket.Conn, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.counter++
	id := c.counter
	key := fmt.Sprint(id)
	ch := make(chan outcome, 1)
	c.pending[key] = pending{ws, ch, method}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	c.mu.Lock()
	var injected error
	if c.beforeWrite != nil {
		injected = c.beforeWrite(method)
	}
	c.mu.Unlock()
	if injected != nil {
		return nil, injected
	}
	if hook, ok := ctx.Value(sendHookKey{}).(func(string)); ok {
		hook(method)
	}
	if err := c.write(ctx, ws, map[string]any{"id": id, "method": method, "params": params}, method); err != nil {
		c.retire(ws)
		return nil, err
	}
	ack, cancel := context.WithTimeout(ctx, c.bounds.Ack)
	defer cancel()
	select {
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.response.Error != nil {
			return nil, rpcError(method, result.response.Error)
		}
		if result.response.Result == nil {
			return nil, &TransportError{method + ": invalid response; outcome unknown"}
		}
		return result.response.Result, nil
	case <-ack.Done():
		if errors.Is(ack.Err(), context.DeadlineExceeded) {
			return nil, &PhaseTimeout{method, "ack", c.bounds.Ack}
		}
		return nil, fmt.Errorf("%s: response unavailable: %w", method, ack.Err())
	}
}
func (c *Client) write(ctx context.Context, ws *websocket.Conn, message map[string]any, method string) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}
	transmit, cancel := context.WithTimeout(ctx, c.bounds.Transmit)
	defer cancel()
	write := c.writeFrame
	if write == nil {
		write = (*websocket.Conn).Write
	}
	err = write(ws, transmit, websocket.MessageText, raw)
	if errors.Is(transmit.Err(), context.DeadlineExceeded) {
		return &PhaseTimeout{method, "transmit", c.bounds.Transmit}
	}
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%s: interrupted write; do not resend: %w", method, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%s: response unavailable; do not resend: %w", method, err)
	}
	return nil
}
func (c *Client) retire(ws *websocket.Conn) {
	c.mu.Lock()
	if c.conn == ws {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = ws.CloseNow()
}
func (c *Client) Close() error {
	c.mu.Lock()
	ws := c.conn
	c.conn = nil
	c.mu.Unlock()
	if ws == nil {
		return nil
	}
	closeFrame := c.closeFrame
	if closeFrame == nil {
		closeFrame = func(conn *websocket.Conn) error { return conn.Close(websocket.StatusNormalClosure, "") }
	}
	done := make(chan error, 1)
	go func() { done <- closeFrame(ws) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = ws.CloseNow()
		return fmt.Errorf("appserver close exceeded 2s: %w", context.DeadlineExceeded)
	}
}
