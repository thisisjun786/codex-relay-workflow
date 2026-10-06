package gui

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// The guard tests build a Server with a fixed port and token, so every rejection and every
// pass is asserted against exactly the host, origin and token the decided answers name.
const (
	guardPort  = 51234
	guardToken = "guard-test-token"

	// guardHost is the Host header a passing request carries.
	guardHost = "127.0.0.1:51234"
)

// testServer builds a Server over a fixed route set: one GET route and one POST route, so a
// write can be observed passing the guard. The route table is passed explicitly, so these
// tests never depend on the package-wide registry.
func testServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{
		Port:    guardPort,
		Token:   guardToken,
		Version: "test-version",
		Routes: []Route{
			{Method: http.MethodGet, Path: "/api/thing", Handler: func(*Env, *http.Request) (Response, error) {
				return Response{Status: http.StatusOK, Body: map[string]any{"ok": true}}, nil
			}},
			{Method: http.MethodPost, Path: "/api/thing", Handler: func(_ *Env, r *http.Request) (Response, error) {
				return Response{Status: http.StatusOK, Body: map[string]any{"read": true}}, nil
			}},
		},
		Assets: fstest.MapFS{"assets/index.html": &fstest.MapFile{Data: []byte("<html>placeholder</html>")}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// request drives one request through the guard and returns its recorder. host overrides the
// Host header; headers adds or replaces request headers.
func request(server *Server, method, target, host string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, reader)
	}
	if host != "" {
		req.Host = host
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

// writeHeaders is the header set a passing write carries: JSON content type and the token.
func writeHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", tokenHeader: guardToken}
}

// The Host check passes the loopback name and port this server bound and refuses a missing
// Host, another name and another port.
func TestGuardHost(t *testing.T) {
	server := testServer(t)
	for _, test := range []struct {
		name string
		host string
		want int
	}{
		{"the loopback address", "127.0.0.1:51234", http.StatusOK},
		{"the loopback name", "localhost:51234", http.StatusOK},
		{"the loopback name in another case", "LOCALHOST:51234", http.StatusOK},
		{"no Host at all", "", http.StatusForbidden},
		{"another name", "evil.example:51234", http.StatusForbidden},
		{"another port", "127.0.0.1:51235", http.StatusForbidden},
		{"the name without a port", "127.0.0.1", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := request(server, http.MethodGet, "/api/thing", test.host, "", nil)
			if recorder.Code != test.want {
				t.Fatalf("host %q: status %d, want %d (%s)", test.host, recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

// The write check refuses a missing or wrong token, a non-JSON content type and a foreign
// Origin, and passes a well-formed write.
func TestGuardWrite(t *testing.T) {
	server := testServer(t)
	for _, test := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"a well-formed write", writeHeaders(), http.StatusOK},
		{"a JSON content type with parameters", map[string]string{"Content-Type": "application/json; charset=utf-8", tokenHeader: guardToken}, http.StatusOK},
		{"the loopback address as Origin", map[string]string{"Content-Type": "application/json", tokenHeader: guardToken, "Origin": "http://127.0.0.1:51234"}, http.StatusOK},
		{"the loopback name as Origin", map[string]string{"Content-Type": "application/json", tokenHeader: guardToken, "Origin": "http://localhost:51234"}, http.StatusOK},
		{"no token", map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		{"a wrong token", map[string]string{"Content-Type": "application/json", tokenHeader: "not-the-token"}, http.StatusForbidden},
		{"a token of another length", map[string]string{"Content-Type": "application/json", tokenHeader: guardToken + "x"}, http.StatusForbidden},
		{"a plain-text write", map[string]string{"Content-Type": "text/plain", tokenHeader: guardToken}, http.StatusForbidden},
		{"no content type", map[string]string{tokenHeader: guardToken}, http.StatusForbidden},
		{"a foreign Origin", map[string]string{"Content-Type": "application/json", tokenHeader: guardToken, "Origin": "https://attacker.example"}, http.StatusForbidden},
		{"the loopback name on another port as Origin", map[string]string{"Content-Type": "application/json", tokenHeader: guardToken, "Origin": "http://127.0.0.1:51235"}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := request(server, http.MethodPost, "/api/thing", guardHost, `{"a":1}`, test.headers)
			if recorder.Code != test.want {
				t.Fatalf("status %d, want %d (%s)", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

// A write body over 1 MiB is refused with 413.
func TestGuardBodyLimit(t *testing.T) {
	server := testServer(t)
	body := `{"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	recorder := request(server, http.MethodPost, "/api/thing", guardHost, body, writeHeaders())
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body: status %d, want %d (%s)", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
	// A body just under the limit still passes, so the check is a limit and not a refusal.
	fitting := `{"pad":"` + strings.Repeat("x", maxBodyBytes-64) + `"}`
	if got := request(server, http.MethodPost, "/api/thing", guardHost, fitting, writeHeaders()); got.Code != http.StatusOK {
		t.Fatalf("a body under the limit: status %d (%s)", got.Code, got.Body.String())
	}
}

// A path segment holding two consecutive dots is refused with 400, for a read and a write.
func TestGuardTraversal(t *testing.T) {
	server := testServer(t)
	for _, test := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/../etc/passwd"},
		{http.MethodGet, "/a/../../b"},
		{http.MethodGet, "/..%2f..%2fetc"},
		{http.MethodGet, "/%2e%2e/etc/passwd"},
		{http.MethodGet, "/assets/%2E%2E/%2E%2E/etc"},
		{http.MethodGet, "/a%3F.."},
		{http.MethodGet, "/a%23.."},
		{http.MethodGet, "/%3f/../index.html"},
		{http.MethodPost, "/api/../../etc"},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			recorder := request(server, test.method, test.target, guardHost, `{"a":1}`, writeHeaders())
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
		})
	}
}

// Every response carries the three security headers, a /api/ response is no-store and no
// response carries a CORS allow header.
func TestGuardSecurityHeaders(t *testing.T) {
	server := testServer(t)
	responses := map[string]*httptest.ResponseRecorder{
		"a refused Host":    request(server, http.MethodGet, "/api/thing", "evil.example:51234", "", nil),
		"a refused path":    request(server, http.MethodGet, "/../x", guardHost, "", nil),
		"an oversized body": request(server, http.MethodPost, "/api/thing", guardHost, `{"pad":"`+strings.Repeat("x", maxBodyBytes)+`"}`, writeHeaders()),
		"an API 404":        request(server, http.MethodGet, "/api/missing", guardHost, "", nil),
		"an API 200":        request(server, http.MethodGet, "/api/thing", guardHost, "", nil),
		"a static 200":      request(server, http.MethodGet, "/index.html", guardHost, "", nil),
	}
	for name, recorder := range responses {
		t.Run(name, func(t *testing.T) {
			if got := recorder.Header().Get("Content-Security-Policy"); got != cspHeader {
				t.Errorf("Content-Security-Policy = %q, want %q", got, cspHeader)
			}
			if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q", got)
			}
			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("a CORS allow header is present: %q", got)
			}
		})
	}
	for name, want := range map[string]string{"an API 200": "no-store", "an API 404": "no-store", "a refused Host": "no-store"} {
		if got := responses[name].Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", name, got, want)
		}
	}
	if got := responses["a static 200"].Header().Get("Cache-Control"); got == "no-store" {
		t.Errorf("a static response must not be no-store")
	}
}

// A rejection body is the short English code and carries no token and no environment value.
func TestGuardRefusalBodies(t *testing.T) {
	server := testServer(t)
	for name, test := range map[string]struct {
		recorder *httptest.ResponseRecorder
		want     string
	}{
		"a refused Host":    {request(server, http.MethodGet, "/api/thing", "evil.example:51234", "", nil), codeForbidden},
		"a refused path":    {request(server, http.MethodGet, "/../x", guardHost, "", nil), codeBadPath},
		"an oversized body": {request(server, http.MethodPost, "/api/thing", guardHost, `{"pad":"`+strings.Repeat("x", maxBodyBytes)+`"}`, writeHeaders()), codeTooLarge},
		"an API 404":        {request(server, http.MethodGet, "/api/missing", guardHost, "", nil), codeNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			want := fmt.Sprintf("{\"error\":%q}\n", test.want)
			if got := test.recorder.Body.String(); got != want {
				t.Fatalf("body %q, want %q", got, want)
			}
			if strings.Contains(test.recorder.Body.String(), guardToken) {
				t.Fatalf("the body carries the token")
			}
		})
	}
}

// A read is not subject to the write checks: a GET with no token and no content type passes.
func TestGuardReadNeedsNoWriteCheck(t *testing.T) {
	server := testServer(t)
	if got := request(server, http.MethodGet, "/api/thing", guardHost, "", nil); got.Code != http.StatusOK {
		t.Fatalf("a read: status %d (%s)", got.Code, got.Body.String())
	}
}

// An absolute-form request line is refused. net/http promotes the request-line authority to
// r.Host and drops the Host header, so `GET http://127.0.0.1:<port>/... ` with `Host: evil`
// would otherwise pass the Host check while naming another host. This server takes origin-form
// only, so the raw request is written over a socket where net/http really parses it.
func TestGuardRefusesAbsoluteForm(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	// The server's port is the listener's, so the Host check would pass for the loopback
	// spelling; the point of the test is that an absolute-form request line must not.
	server, err := New(Options{
		Port:    port,
		Token:   guardToken,
		Version: "test-version",
		Routes: []Route{{Method: http.MethodGet, Path: "/api/thing", Handler: func(*Env, *http.Request) (Response, error) {
			return Response{Status: http.StatusOK, Body: map[string]any{"ok": true}}, nil
		}}},
		Assets: fstest.MapFS{"assets/index.html": &fstest.MapFile{Data: []byte("<html>placeholder</html>")}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	defer httpServer.Close()
	for _, request := range []string{
		"GET http://127.0.0.1:%d/api/thing HTTP/1.1\r\nHost: evil.example:%d\r\nConnection: close\r\n\r\n",
		"GET http://evil.example:%d/api/thing HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nConnection: close\r\n\r\n",
	} {
		t.Run(fmt.Sprintf("%q", strings.Split(request, "\r\n")[0]), func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(conn, request, port, port); err != nil {
				t.Fatal(err)
			}
			status, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				t.Fatalf("no response: %v", err)
			}
			if !strings.Contains(status, "403") {
				t.Fatalf("status line %q, want a 403", strings.TrimSpace(status))
			}
		})
	}
}

// A write handler receives the request context, so a durable effect can check cancellation
// immediately before it commits (the decided answer's requirement for write handles).
func TestWriteHandlerSeesTheRequestContext(t *testing.T) {
	type contextKey string
	const marker contextKey = "crw-gui-test"
	seen := make(chan any, 1)
	server, err := New(Options{
		Port:    guardPort,
		Token:   guardToken,
		Version: "test-version",
		Routes: []Route{{Method: http.MethodPost, Path: "/api/ctx", Handler: func(_ *Env, r *http.Request) (Response, error) {
			seen <- r.Context().Value(marker)
			return Response{Status: http.StatusOK, Body: map[string]any{"ok": true}}, nil
		}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/ctx", strings.NewReader(`{}`)).WithContext(context.WithValue(context.Background(), marker, "carried"))
	request.Host = guardHost
	for name, value := range writeHeaders() {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d (%s)", recorder.Code, recorder.Body.String())
	}
	if got := <-seen; got != "carried" {
		t.Fatalf("the handler saw %v, want the context value carried on the request", got)
	}
}

// The static handler serves only from the fs.FS it was built with: a path that names a file
// outside the tree is not read, and an unknown path falls back to index.html.
func TestStaticServesOnlyFromTheTree(t *testing.T) {
	server := testServer(t)
	if got := request(server, http.MethodGet, "/index.html", guardHost, "", nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "placeholder") {
		t.Fatalf("index.html: %d %q", got.Code, got.Body.String())
	}
	if got := request(server, http.MethodGet, "/some/unknown/route", guardHost, "", nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "placeholder") {
		t.Fatalf("the SPA fallback: %d %q", got.Code, got.Body.String())
	}
	if got := request(server, http.MethodGet, "/etc/passwd", guardHost, "", nil); got.Code != http.StatusOK || strings.Contains(got.Body.String(), "root:") {
		t.Fatalf("an outside path was read: %d %q", got.Code, got.Body.String())
	}
	if _, err := fs.Stat(server.assets, "assets/index.html"); err != nil {
		t.Fatalf("the test tree is not the served tree: %v", err)
	}
}
