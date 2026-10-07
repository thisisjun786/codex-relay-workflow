package gui

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// This file pins the boundary the decided answer documents: net/http answers some requests
// itself, before it consults the Handler, and those responses carry none of the security
// headers. The class is closed by construction rather than enumerated: the package adds no HTTP
// parser, no connection wrapper that rewrites response bytes and no second listener to reach
// them, so nothing here can attach a header to a response net/http already wrote. The cases
// below pin the common members of the class. Each drives the real server over a real loopback
// connection and asserts the fixed status, that no route handler ran, that the bytes carry
// neither the token nor an asset, and that the connection was closed.

// listenerToken is the token these tests pin and listenerAsset is the marker the served tree
// carries; neither may appear in a pre-handler response body.
const (
	listenerToken = "listener-test-token"
	listenerAsset = "LISTENER-ASSET-MARKER"
)

// preHandlerServer starts the real server on an empty loopback port with routes whose handlers
// signal that they ran. It returns the bound address and a channel a route writes to when it is
// reached, so a test can prove the Handler was never called for a pre-handler refusal.
func preHandlerServer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 4)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	handler := func(*Env, *http.Request) (Response, error) {
		reached <- struct{}{}
		return Response{Status: http.StatusOK, Body: map[string]any{"ok": true}}, nil
	}
	server, err := New(Options{
		Port:    port,
		Token:   listenerToken,
		Version: "test-version",
		Routes: []Route{
			{Method: http.MethodGet, Path: "/api/thing", Handler: handler},
			{Method: http.MethodPost, Path: "/api/thing", Handler: handler},
		},
		Assets: fstest.MapFS{"assets/index.html": &fstest.MapFile{Data: []byte(listenerAsset)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	return listener.Addr().String(), reached
}

// listenerRawExchange writes one raw request and reads the whole response, to EOF, under a
// deadline. Reaching EOF is itself the proof that the connection was closed rather than left
// open. A write error is not fatal: net/http may close the connection while a request that it
// is about to refuse is still being written, and the read below is what decides the outcome.
func listenerRawExchange(t *testing.T, address, request string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, request)
	data, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("the response did not reach EOF, so the connection was not closed: %v", err)
	}
	return string(data)
}

// listenerStatusLine returns the first line of a raw response, trimmed.
func listenerStatusLine(response string) string {
	line, _, _ := strings.Cut(response, "\r\n")
	return strings.TrimSpace(line)
}

// A request net/http answers before the Handler gets net/http's own fixed status, no security
// header, no route handler run, no token or asset in the body, and a closed connection.
func TestPreHandlerResponsesArePinned(t *testing.T) {
	address, reached := preHandlerServer(t)
	for _, test := range []struct {
		name    string
		request string
		want    string
	}{
		{"no Host", "GET /api/thing HTTP/1.1\r\n\r\n", "400"},
		{"a repeated Host", "GET /api/thing HTTP/1.1\r\nHost: 127.0.0.1\r\nHost: evil.example\r\n\r\n", "400"},
		{"a malformed Host", "GET /api/thing HTTP/1.1\r\nHost: bad host\r\n\r\n", "400"},
		{"an Expect that is not 100-continue", "POST /api/thing HTTP/1.1\r\nHost: " + address + "\r\nExpect: unsupported-thing\r\nContent-Length: 2\r\n\r\n{}", "417"},
		{"an unsupported protocol version", "GET /api/thing HTTP/2.0\r\nHost: " + address + "\r\n\r\n", "505"},
		{"an unsupported transfer encoding", "POST /api/thing HTTP/1.1\r\nHost: " + address + "\r\nTransfer-Encoding: bogus\r\n\r\n", "501"},
		{"an oversized header", "GET /api/thing HTTP/1.1\r\nHost: " + address + "\r\nX-Big: " + strings.Repeat("a", 2<<20) + "\r\n\r\n", "431"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := listenerRawExchange(t, address, test.request)
			if got := listenerStatusLine(response); !strings.HasPrefix(got, "HTTP/1.1 "+test.want) {
				t.Fatalf("status line %q, want a %s", got, test.want)
			}
			// The guard sets Content-Security-Policy before any other logic, so its presence is
			// exactly the fact that the Handler ran; net/http writes no such header itself.
			if strings.Contains(strings.ToLower(response), "content-security-policy") {
				t.Fatalf("the response carries a security header, so the Handler ran: %q", response)
			}
			if strings.Contains(response, listenerToken) {
				t.Fatalf("the response carries the token: %q", response)
			}
			if strings.Contains(response, listenerAsset) {
				t.Fatalf("the response carries the served asset: %q", response)
			}
			select {
			case <-reached:
				t.Fatalf("a route handler ran for a request net/http answered before the Handler")
			default:
			}
		})
	}
}

// The 100-continue case is the control: it is not a pre-handler refusal, so it reaches the
// Handler. Its response carries the security headers, which proves the suite is not simply
// asserting that every response is bare.
func TestPreHandlerControlExpectContinueReachesTheHandler(t *testing.T) {
	address, _ := preHandlerServer(t)
	request := "POST /api/thing HTTP/1.1\r\nHost: " + address + "\r\nExpect: 100-continue\r\n" +
		"Content-Type: application/json\r\n" + tokenHeader + ": " + listenerToken + "\r\n" +
		"Content-Length: 2\r\nConnection: close\r\n\r\n{}"
	response := listenerRawExchange(t, address, request)
	if !strings.Contains(strings.ToLower(response), "content-security-policy") {
		t.Fatalf("the 100-continue control did not reach the Handler: %q", response)
	}
}

// A write refused by the guard, not by net/http, still carries the security headers, and the
// connection is closed. This separates the guard's own refusals from the pre-handler boundary.
func TestGuardRefusalOnTheListenerCarriesSecurityHeaders(t *testing.T) {
	address, reached := preHandlerServer(t)
	request := "POST /api/thing HTTP/1.1\r\nHost: " + address + "\r\n" +
		"Content-Type: application/json\r\n" + tokenHeader + ": wrong-token\r\n" +
		"Content-Length: 2\r\nConnection: close\r\n\r\n{}"
	response := listenerRawExchange(t, address, request)
	if got := listenerStatusLine(response); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Fatalf("status line %q, want a 403", got)
	}
	if !strings.Contains(strings.ToLower(response), "content-security-policy") {
		t.Fatalf("the guard's own refusal carries no security headers: %q", response)
	}
	select {
	case <-reached:
		t.Fatalf("a route handler ran for a write the guard refused")
	default:
	}
}
