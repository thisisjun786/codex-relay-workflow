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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
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
	// ErrorObject, when set, is sent verbatim as the error member, so a code may be any JSON
	// value (the Python fake sends whatever reject[method] holds).
	ErrorObject map[string]any
	// Delay holds the answer back; it is abandoned when the connection or server closes.
	Delay time.Duration
	// PadBytes adds a "padding" member of that many bytes to Result: frame-size injection.
	PadBytes int
	// Paused signals when this request reaches the fake and blocks its answer until Release.
	Paused  chan<- struct{}
	Release <-chan struct{}
	// Before is sent ahead of the answer, in order.
	Before []Notification
	// ServerRequests names server-to-client requests raised ahead of the answer, about the
	// request's params.threadId.
	ServerRequests []string
	// SchemaParams gives those requests the params the generated codex schema defines for each method:
	// the two legacy approvals (execCommandApproval, applyPatchApproval) name their thread in
	// conversationId and carry no threadId. Without it every request carries threadId, turnId and a
	// command, the shape the older tests were written against.
	SchemaParams bool
	// Close, when set, closes the connection with this frame and sends no answer.
	Close *CloseFrame
	// After runs once the answer was written, with the connection that asked: the way a handler pushes the
	// notifications that follow a request (a turn's progress) at a later time. It runs on the connection's reader
	// unless the answer has a Delay; a hook that waits must start its own goroutine.
	After func(*Conn)
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
	Params json.RawMessage
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

	mu             sync.Mutex
	closed         bool
	conns          map[*websocket.Conn]struct{}
	scripted       map[string][]Reply
	handlerFor     map[string]Handler
	connHandlerFor map[string]ConnHandler
	standing       map[string]Reply
	requests       []Request
	recorded       chan struct{}
	malformed      []Malformed
	raised         []ServerRequest
	answers        []Answer
	compressionOff int
	threads        *Threads
}

// Start listens on a fresh unix socket and stops the fake when the test ends.
func Start(tb testing.TB) *Server {
	tb.Helper()
	path := SocketPath(tb, "app.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		tb.Fatalf("fakehost: listen %s: %v", path, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		SocketPath:     path,
		cancel:         cancel,
		conns:          map[*websocket.Conn]struct{}{},
		scripted:       map[string][]Reply{},
		handlerFor:     map[string]Handler{},
		connHandlerFor: map[string]ConnHandler{},
		standing:       map[string]Reply{},
		recorded:       make(chan struct{}),
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

// socketPattern names the directory SocketPath makes below tmploc.SocketRoot.
const socketPattern = "crw-fakehost-"

// SocketPath returns the path of a unix socket named base in a fresh directory below
// tmploc.SocketRoot that is removed when the test ends (tmploctest.SocketDir). The directory is
// never below TMPDIR, whatever its length: a socket's place does not depend on the run's TMPDIR
// (CRW-1568). The path is measured by tmploctest.CheckSocket, so a name longer than
// tmploc.SocketTailMax fails the test with its length.
func SocketPath(tb testing.TB, base string) string {
	tb.Helper()
	if base == "" || base == "." || base == ".." || strings.ContainsRune(base, filepath.Separator) {
		tb.Fatalf("fakehost: socket name %q is not a file name", base)
	}
	dir := tmploctest.SocketDir(tb, socketPattern)
	path := filepath.Join(dir, base)
	tmploctest.CheckSocket(tb, dir, path)
	return path
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

// Handler computes the answer to one request from its params. It runs on the connection's read
// loop, so a handler that blocks holds that connection exactly as a busy host would.
type Handler func(params json.RawMessage) Reply

// ConnHandler is a Handler that also sees the connection the request came on.
type ConnHandler func(conn *Conn, params json.RawMessage) Reply

// HandleConn answers method through h like Handle, handing it the connection the request came on.
func (s *Server) HandleConn(method string, h ConnHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connHandlerFor[method] = h
}

// Handle answers method through h whenever no scripted answer is queued; it takes precedence
// over a standing answer.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlerFor[method] = h
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

// WaitCount blocks until method has been recorded at least n times, or ctx expires.
// Subscribe under the recording lock so a request cannot be missed between checks.
func (s *Server) WaitCount(ctx context.Context, method string, n int) error {
	for {
		s.mu.Lock()
		count := 0
		for _, request := range s.requests {
			if request.Method == method {
				count++
			}
		}
		if count >= n {
			s.mu.Unlock()
			return nil
		}
		recorded := s.recorded
		s.mu.Unlock()
		select {
		case <-recorded:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
// CompressionOff counts handshakes without a compression extension offer.
func (s *Server) CompressionOff() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compressionOff
}

func (s *Server) Answers() []Answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Answer(nil), s.answers...)
}

func (s *Server) accept(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Websocket-Extensions") == "" {
		s.mu.Lock()
		s.compressionOff++
		s.mu.Unlock()
	}
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

func (s *Server) replyOn(conn *Conn, method string, params json.RawMessage) (Reply, bool) {
	s.mu.Lock()
	if queued := s.scripted[method]; len(queued) > 0 {
		s.scripted[method] = queued[1:]
		s.mu.Unlock()
		return queued[0], true
	}
	connHandler, connHandled := s.connHandlerFor[method]
	handler, handled := s.handlerFor[method]
	reply, ok := s.standing[method]
	s.mu.Unlock()
	switch {
	case connHandled:
		return connHandler(conn, params), true
	case handled:
		return handler(params), true
	}
	return reply, ok
}

func (s *Server) record(apply func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(s)
	close(s.recorded)
	s.recorded = make(chan struct{})
}

func (s *Server) nextServerRequest(method string, params map[string]any) string {
	raw, _ := json.Marshal(params)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("server-%d", len(s.raised)+1)
	s.raised = append(s.raised, ServerRequest{ID: id, Method: method, Params: raw})
	return id
}
