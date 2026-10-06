package gui

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The server tests run the real command in a goroutine and drive it over loopback, so the
// printed line, the bound port and the shutdown are observed as an operator sees them.

// Run binds an empty port, prints one token-bearing line, and serves on the port it printed.
func TestRunBindsAnEmptyPortAndPrintsOneLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &lineWriter{}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"--port", "0"}, out, io.Discard) }()
	line := waitForLine(t, out)
	const prefix = "crw gui: serving http://127.0.0.1:"
	if !strings.HasPrefix(line, prefix) || !strings.Contains(line, "/#token=") {
		t.Fatalf("the line is %q", line)
	}
	rest := strings.TrimPrefix(line, prefix)
	port, token, ok := strings.Cut(rest, "/#token=")
	if !ok || port == "" || token == "" {
		t.Fatalf("the line is %q", line)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(decoded) != 32 {
		t.Fatalf("the token is not 32 bytes in base64url: %q (%v)", token, err)
	}
	host := "127.0.0.1:" + port
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + host + "/api/health")
	if err != nil {
		t.Fatalf("the printed port does not serve: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var health struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("health body %q: %v", body, err)
	}
	if resp.StatusCode != http.StatusOK || !health.OK {
		t.Fatalf("health: %d %q", resp.StatusCode, body)
	}
	// The token the line printed is the one the guard accepts.
	post, err := http.NewRequest(http.MethodPost, "http://"+host+"/api/health", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set(tokenHeader, token)
	guarded, err := client.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	guarded.Body.Close()
	if guarded.StatusCode == http.StatusForbidden {
		t.Fatalf("the printed token was refused")
	}
	// No file was written under the isolated home.
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("the run wrote into the home: %v (%v)", entries, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Run returned %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// A cancelled context closes the listener and lets Run return promptly.
func TestRunStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &lineWriter{}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"--port", "0"}, out, io.Discard) }()
	line := waitForLine(t, out)
	address := strings.TrimPrefix(strings.SplitN(line, "/#token=", 2)[0], "crw gui: serving http://")
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("Run returned %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	// The listener is closed: a later dial is refused.
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		conn.Close()
		t.Fatalf("the listener is still accepting on %s", address)
	}
}

// A duplicate method and path fails server construction with an error naming the route.
func TestNewRejectsDuplicateRoutes(t *testing.T) {
	handler := func(*Env, *http.Request) (Response, error) { return Response{Status: http.StatusOK}, nil }
	_, err := New(Options{Port: 0, Token: "t", Version: "v", Routes: []Route{
		{Method: http.MethodGet, Path: "/api/dup", Handler: handler},
		{Method: http.MethodGet, Path: "/api/dup", Handler: handler},
	}})
	if err == nil {
		t.Fatal("a duplicate method+path was accepted")
	}
	if !strings.Contains(err.Error(), "GET /api/dup") {
		t.Fatalf("the error does not name the route: %v", err)
	}
	// A same path under another method is not a duplicate.
	if _, err := New(Options{Port: 0, Token: "t", Version: "v", Routes: []Route{
		{Method: http.MethodGet, Path: "/api/dup", Handler: handler},
		{Method: http.MethodPost, Path: "/api/dup", Handler: handler},
	}}); err != nil {
		t.Fatalf("a same path under another method was refused: %v", err)
	}
}

// The package-wide registry holds the health route, which its own file registered from
// init(): the shape a later issue copies, one file and no shared-table edit.
func TestHealthIsRegisteredFromItsOwnFile(t *testing.T) {
	found := false
	for _, route := range Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/health" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /api/health is not registered")
	}
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New over the package registry: %v", err)
	}
	recorder := request(server, http.MethodGet, "/api/health", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health: %d %q", recorder.Code, recorder.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Version != "test-version" {
		t.Fatalf("health body %q", recorder.Body.String())
	}
}

// An unregistered /api/ path is a JSON 404, and any other unknown path serves the embedded
// placeholder, which is the only asset of this issue.
func TestStaticFallbackAndAPINotFound(t *testing.T) {
	server := testServer(t)
	notFound := request(server, http.MethodGet, "/api/missing", guardHost, "", nil)
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), codeNotFound) {
		t.Fatalf("an API miss: %d %q", notFound.Code, notFound.Body.String())
	}
	for _, target := range []string{"/", "/index.html", "/some/deep/route"} {
		recorder := request(server, http.MethodGet, target, guardHost, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "placeholder") {
			t.Fatalf("%s: body %q", target, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("%s: content type %q", target, got)
		}
	}
}

// The placeholder asset of this issue is embedded in the binary, with the decided text.
func TestPlaceholderAssetIsEmbedded(t *testing.T) {
	data, err := assetFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("the asset is not embedded: %v", err)
	}
	if !strings.Contains(string(data), "CRW GUI: screens are not built into this binary yet") {
		t.Fatalf("the asset text is %q", data)
	}
}

// An `OPTIONS *` request reaches the guard. net/http answers it itself with a general OPTIONS
// handler that never consults the server's handler, so without disabling that handler the
// request would skip the Host check and every security header and be answered 200.
func TestServerRefusesGeneralOptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &lineWriter{}
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, Options{Port: 0, Token: "options-token", Version: "v"}, out) }()
	line := waitForLine(t, out)
	address := strings.TrimPrefix(strings.SplitN(line, "/#token=", 2)[0], "crw gui: serving http://")
	for _, request := range []string{
		"OPTIONS * HTTP/1.1\r\nHost: " + address + "\r\nConnection: close\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n",
	} {
		t.Run(strings.Split(request, "\r\n")[1], func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", address, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(conn, request); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			status, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("no response: %v", err)
			}
			if strings.Contains(status, "200") {
				t.Fatalf("an OPTIONS * request was answered %q, so it skipped the guard", strings.TrimSpace(status))
			}
			// The refusal carries the security headers, which proves the guard answered it.
			headers := map[string]bool{}
			for {
				line, err := reader.ReadString('\n')
				if err != nil || strings.TrimSpace(line) == "" {
					break
				}
				name, _, _ := strings.Cut(line, ":")
				headers[strings.ToLower(strings.TrimSpace(name))] = true
			}
			if !headers["content-security-policy"] {
				t.Fatalf("the refusal carries no security headers: %v", headers)
			}
		})
	}
}

// Run refuses a port outside the range and a stray argument with the usage status.
func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--port", "70000"}, {"--port", "-1"}, {"--port", "x"}, {"extra"}} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), args, &out, &errOut); code != usageExit {
			t.Errorf("%v: code %d, want %d (stderr %q)", args, code, usageExit, errOut.String())
		}
		if errOut.Len() == 0 {
			t.Errorf("%v: no usage on stderr", args)
		}
	}
}

// --help writes the help page to stdout and exits 0, as every other crw mode does; a usage
// error writes the usage to stderr and exits 2.
func TestRunHelpGoesToStdout(t *testing.T) {
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("--help: code %d (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "usage: crw gui") {
		t.Fatalf("--help wrote %q to stdout, want the usage line", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("--help wrote %q to stderr", errOut.String())
	}
	var out2, errOut2 strings.Builder
	if code := Run(context.Background(), []string{"--port", "70000"}, &out2, &errOut2); code != usageExit || out2.Len() != 0 || errOut2.Len() == 0 {
		t.Fatalf("a usage error: code %d stdout %q stderr %q", code, out2.String(), errOut2.String())
	}
}

// Serve derives every request context from the run context, so a write handler observes the
// cancellation that ends the server: a durable effect can check it right before it commits,
// and a handler waiting on cancellation lets the graceful shutdown finish instead of timing
// out. The request is made over the real listener, because that is where net/http derives the
// context from the server's base context.
func TestRequestContextFollowsTheRunContext(t *testing.T) {
	seen := make(chan error, 1)
	// entered is closed by the handler once it is running, so the test cancels only after the
	// handler has entered rather than after a fixed sleep that a slow machine can outrun.
	entered := make(chan struct{})
	token := "a-token-for-this-test"
	routes := []Route{{Method: http.MethodPost, Path: "/api/wait", Handler: func(_ *Env, r *http.Request) (Response, error) {
		close(entered)
		<-r.Context().Done()
		seen <- r.Context().Err()
		return Response{Status: http.StatusOK, Body: map[string]any{"ok": true}}, nil
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	out := &lineWriter{}
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, Options{Port: 0, Token: token, Version: "v", Routes: routes}, out) }()
	line := waitForLine(t, out)
	address := strings.TrimPrefix(strings.SplitN(line, "/#token=", 2)[0], "crw gui: serving http://")
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/wait", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(tokenHeader, token)
	clientDone := make(chan struct{})
	go func() {
		_, _ = (&http.Client{Timeout: 15 * time.Second}).Do(request)
		close(clientDone)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not enter before the cancellation")
	}
	cancel()
	select {
	case err := <-seen:
		if err == nil {
			t.Fatal("the handler saw a live context after the run context was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not observe the cancellation")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v after the run context was cancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the run context was cancelled")
	}
	<-clientDone
}

// lineWriter is the writer the tests hand a run that prints from its own goroutine. A plain
// strings.Builder is not safe for a concurrent read, so the test polls this under its mutex.
type lineWriter struct {
	mu   sync.Mutex
	text strings.Builder
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.Write(p)
}

func (w *lineWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

// waitForLine waits for the run to print its one line and returns it trimmed.
func waitForLine(t *testing.T, out *lineWriter) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if text := strings.TrimSpace(out.String()); text != "" {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatal("the run printed no line")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
