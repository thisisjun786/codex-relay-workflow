// Package fakehost is a deterministic stand-in for the Codex App Server, for Go tests.
//
// It speaks the App Server envelope over a WebSocket on a unix socket: requests are
// {"id","method","params"}, notifications {"method","params"}, responses {"id","result"} or
// {"id","error"}, and there is no "jsonrpc" field. A frame that carries one, or that is not a
// JSON object at all, is recorded as malformed and never dispatched. Answers are scripted per
// method; every request, notification and client answer is recorded so a test can assert what
// was actually sent. Nothing here implements real App Server semantics: an unscripted method is
// answered -32601, exactly as the Python fake answers a method it does not know.
package fakehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// MaxFrameBytes is the largest frame either side of the real transport accepts (rpc.py:31).
const MaxFrameBytes = 16 * 1024 * 1024

// UserAgent is what the fake calls itself at initialize, as the Python fake does.
const UserAgent = "fake Codex/0.153.4"

// ApprovalMethods are the server-to-client requests that ask a human to decide (rpc.py:38-48).
var ApprovalMethods = [...]string{
	"execCommandApproval",
	"applyPatchApproval",
	"item/commandExecution/requestApproval",
	"item/fileChange/requestApproval",
	"item/permissions/requestApproval",
	"mcpServer/elicitation/request",
	"item/tool/requestUserInput",
}

// RPCError is the error member of a response.
type RPCError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

// Notification is a server-to-client message without an id.
type Notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// CloseFrame closes the connection instead of answering.
type CloseFrame struct {
	Code   websocket.StatusCode
	Reason string
}

// Reply is how the fake answers one request. The zero Reply answers with an empty result.
type Reply struct {
	Result map[string]any
	Error  *RPCError
	// Delay holds the answer back; it is abandoned when the connection or server closes.
	Delay time.Duration
	// PadBytes adds a "padding" member of that many bytes to Result: frame-size injection.
	PadBytes int
	// Before is sent ahead of the answer, in order.
	Before []Notification
	// ServerRequests names server-to-client requests raised ahead of the answer, about the
	// request's params.threadId.
	ServerRequests []string
	// Close, when set, closes the connection with this frame and sends no answer.
	Close *CloseFrame
}

// Request is one request or notification (ID is nil) the client sent.
type Request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Malformed is a frame the fake refused to dispatch, and why.
type Malformed struct {
	Raw    []byte
	Reason string
}

// ServerRequest is one server-to-client request the fake raised.
type ServerRequest struct {
	ID     string
	Method string
}

// Answer is the client's reply to a server-to-client request.
type Answer struct {
	ID  json.RawMessage
	Raw json.RawMessage
}

// Server is a running fake App Server.
type Server struct {
	// SocketPath is the unix socket the fake listens on.
	SocketPath string

	cancel   context.CancelFunc
	httpSrv  *http.Server
	handlers sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	conns     map[*websocket.Conn]struct{}
	scripted  map[string][]Reply
	standing  map[string]Reply
	requests  []Request
	malformed []Malformed
	raised    []ServerRequest
	answers   []Answer
}

// Start listens on a fresh unix socket and stops the fake when the test ends.
func Start(tb testing.TB) *Server {
	tb.Helper()
	// Not tb.TempDir: a long test name would push the socket path past the 108-byte limit.
	dir, err := os.MkdirTemp("", "crw-fakehost-")
	if err != nil {
		tb.Fatalf("fakehost: temp dir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "app.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		tb.Fatalf("fakehost: listen %s: %v", path, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		SocketPath: path,
		cancel:     cancel,
		conns:      map[*websocket.Conn]struct{}{},
		scripted:   map[string][]Reply{},
		standing:   map[string]Reply{},
	}
	s.httpSrv = &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.accept(ctx, w, r) }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.handlers.Go(func() {
		if err := s.httpSrv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			tb.Errorf("fakehost: serve: %v", err)
		}
	})
	tb.Cleanup(s.Close)
	return s
}

// Close stops the listener, drops every connection and waits for their handlers.
func (s *Server) Close() {
	s.cancel()
	_ = s.httpSrv.Close()
	s.mu.Lock()
	s.closed = true
	for conn := range s.conns {
		_ = conn.CloseNow()
	}
	s.mu.Unlock()
	s.handlers.Wait()
}

// Script queues one-shot answers for method, used in order before any standing answer.
func (s *Server) Script(method string, replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripted[method] = append(s.scripted[method], replies...)
}

// Respond sets the answer method gets whenever no scripted one is queued.
func (s *Server) Respond(method string, reply Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standing[method] = reply
}

// Requests returns every request and notification received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count returns how many requests and notifications named method were received.
func (s *Server) Count(method string) int {
	count := 0
	for _, request := range s.Requests() {
		if request.Method == method {
			count++
		}
	}
	return count
}

// Malformed returns every frame refused as outside the envelope, in order.
func (s *Server) Malformed() []Malformed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Malformed(nil), s.malformed...)
}

// ServerRequests returns every server-to-client request raised, in order.
func (s *Server) ServerRequests() []ServerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ServerRequest(nil), s.raised...)
}

// Answers returns every client answer to a server-to-client request, in order.
func (s *Server) Answers() []Answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Answer(nil), s.answers...)
}

func (s *Server) accept(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// Codex 0.153.4 closes unix handshakes offering permessage-deflate; the fake offers none.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(MaxFrameBytes)
	// Registered under the lock Close takes, so Close either sees this connection or this
	// handler sees Close, and handlers.Wait never races handlers.Add.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.CloseNow()
		return
	}
	s.conns[conn] = struct{}{}
	s.handlers.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.CloseNow()
		s.handlers.Done()
	}()
	(&session{server: s, conn: conn}).serve(ctx)
}

func (s *Server) reply(method string) (Reply, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queued := s.scripted[method]; len(queued) > 0 {
		s.scripted[method] = queued[1:]
		return queued[0], true
	}
	reply, ok := s.standing[method]
	return reply, ok
}

func (s *Server) record(apply func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(s)
}

func (s *Server) nextServerRequest(method string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("server-%d", len(s.raised)+1)
	s.raised = append(s.raised, ServerRequest{ID: id, Method: method})
	return id
}
