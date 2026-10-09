package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// within runs f and fails the test, without waiting for f, when it has not returned in d. A fetch that never
// returns is the defect under test, so the test must not hang with it.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
	}
}

func shortLimits(t *testing.T, source, command time.Duration) {
	t.Helper()
	oldSource, oldCommand := sourceTimeout, commandTimeout
	sourceTimeout, commandTimeout = source, command
	t.Cleanup(func() { sourceTimeout, commandTimeout = oldSource, oldCommand })
}

// stallServer answers each request with handler; the release channel ends every handler still waiting.
func stallServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r, release) }))
	t.Cleanup(func() { close(release); server.Close() })
	return server
}

func TestHTTPFetchDeadlinesAndBounds(t *testing.T) {
	chunkedEnded := make(chan struct{})
	server := stallServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		wait := func() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		switch r.URL.Path {
		case "/no-headers":
			wait()
		case "/stalled-body":
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			wait()
		case "/chunked-over":
			defer close(chunkedEnded)
			chunk := bytes.Repeat([]byte("x"), 1024)
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		case "/declared-over":
			w.Header().Set("Content-Length", "5000")
			_, _ = w.Write([]byte("short"))
			w.(http.Flusher).Flush()
			wait()
		case "/exact":
			_, _ = w.Write(bytes.Repeat([]byte("y"), 2048))
		}
	})
	for _, c := range []struct {
		path    string
		timeout time.Duration
		want    string
	}{
		{"/no-headers", 150 * time.Millisecond, "timed out"},
		{"/stalled-body", 150 * time.Millisecond, "timed out"},
		{"/chunked-over", 5 * time.Second, "exceeds 2048 bytes"},
		{"/declared-over", 5 * time.Second, "exceeds 2048 bytes"},
	} {
		t.Run(c.path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			var err error
			within(t, c.timeout+6*time.Second, func() { _, err = fetchHTTP(ctx, server.URL+c.path, 2048) })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	select {
	case <-chunkedEnded: // the client closed the connection once it had seen the limit exceeded
	case <-time.After(3 * time.Second):
		t.Error("the endless body was still being served after the fetch refused it")
	}
	t.Run("a body of exactly the limit is accepted", func(t *testing.T) {
		body, err := fetchHTTP(context.Background(), server.URL+"/exact", 2048)
		if err != nil || len(body) != 2048 {
			t.Fatalf("%d %v", len(body), err)
		}
	})
	t.Run("a cancelled request returns promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		var err error
		within(t, 6*time.Second, func() { _, err = fetchHTTP(ctx, server.URL+"/no-headers", 2048) })
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("err = %v", err)
		}
	})
}

func hangFetch(url string) (string, error) { select {} }

func TestSearchBoundsEachSourceAndKeepsTheOthers(t *testing.T) {
	cliHome(t)
	shortLimits(t, 200*time.Millisecond, 10*time.Second)
	fetch := func(url string) (string, error) {
		switch {
		case url == JAWRegistryURL:
			return hangFetch(url)
		case url == HermesCatalogURL:
			return "| [`tdd`](x) | tdd loop | `development/tdd` |", nil
		}
		return `{"results":[{"slug":"tdd","summary":"market"}]}`, nil
	}
	var code int
	var out, errOut string
	start := time.Now()
	within(t, 8*time.Second, func() { code, out, errOut = cliRun([]string{"search", "tdd", "--source", "all", "--json"}, fetch) })
	if code != 0 || !strings.Contains(out, `"source": "hermes"`) || !strings.Contains(out, `"source": "clawhub"`) || !strings.Contains(errOut, "skill-search: source jaw failed (") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("one stalled source held the command for %s", elapsed)
	}
}

func TestSearchRunsIndependentSourcesConcurrently(t *testing.T) {
	cliHome(t)
	var mu sync.Mutex
	started := map[string]bool{}
	both := make(chan struct{})
	fetch := func(url string) (string, error) {
		mu.Lock()
		started[url] = true
		if started[JAWRegistryURL] && started[HermesCatalogURL] {
			select {
			case <-both:
			default:
				close(both)
			}
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(3 * time.Second):
			return "", errors.New("the other source never started: sources ran one after another")
		}
		switch {
		case url == JAWRegistryURL:
			return cliRegistry, nil
		case url == HermesCatalogURL:
			return "| [`tdd`](x) | tdd | `development/tdd` |", nil
		}
		return `{"results":[]}`, nil
	}
	code, out, errOut := cliRun([]string{"search", "tdd", "--source", "all", "--limit", "5", "--json"}, fetch)
	if code != 0 || errOut != "" || !strings.Contains(out, `"jaw"`) || !strings.Contains(out, `"hermes"`) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestSearchTimeoutKeepsTheUsableCache(t *testing.T) {
	cliHome(t)
	shortLimits(t, 150*time.Millisecond, 10*time.Second)
	dir, _ := CacheDir(os.LookupEnv)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, CacheKey("jaw", JAWRegistryURL)+".cache")
	if err := os.WriteFile(file, []byte(cliRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	var code int
	var out, errOut string
	within(t, 8*time.Second, func() { code, out, errOut = cliRun([]string{"search", "telegram", "--json"}, hangFetch) })
	if code != 0 || !strings.Contains(out, "telegram-send") || !strings.Contains(errOut, "serving stale cache") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if kept, err := os.ReadFile(file); err != nil || string(kept) != cliRegistry {
		t.Fatalf("the cache was not kept: %q %v", kept, err)
	}
}

func TestSearchExitStatusByAvailability(t *testing.T) {
	down := func(string) (string, error) { return "", errors.New("down") }
	for _, c := range []struct {
		name  string
		args  []string
		fetch FetchText
		exit  int
		out   string
		err   string
	}{
		{"every selected source down", []string{"search", "x", "--source", "all", "--json"}, down, 3, "", "skill-search: all selected sources unavailable (jaw, hermes, clawhub)\n"},
		{"the one selected source down", []string{"search", "x", "--source", "hermes", "--json"}, down, 3, "", "skill-search: source hermes failed (down)\n"},
		{"one of three answers", []string{"search", "tdd", "--source", "all", "--json"}, func(url string) (string, error) {
			if url == JAWRegistryURL {
				return cliRegistry, nil
			}
			return "", errors.New("down")
		}, 0, `"id": "tdd"`, "skill-search: source hermes failed (down)\nskill-search: source clawhub failed (down)\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cliHome(t)
			code, out, errOut := cliRun(c.args, c.fetch)
			if code != c.exit || !strings.Contains(out, c.out) || (c.out == "" && out != "") || !strings.HasSuffix(errOut, c.err) {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
		})
	}
}

func TestShowBodyHasItsOwnLimit(t *testing.T) {
	cliHome(t)
	fetch := func(url string) (string, error) {
		if url == JAWRegistryURL {
			return cliRegistry, nil
		}
		return strings.Repeat("b", MaxShowBodyBytes+1), nil
	}
	code, out, errOut := cliRun([]string{"show", "telegram-send"}, fetch)
	if code != 1 || out != "" || !strings.Contains(errOut, fmt.Sprintf("exceeds %d bytes", MaxShowBodyBytes)) {
		t.Fatalf("%d %q %.200q", code, out, errOut)
	}
}

func TestRunContextCancellationReturnsPromptly(t *testing.T) {
	cliHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	var code int
	var errOut bytes.Buffer
	within(t, 6*time.Second, func() {
		code = RunContext(ctx, []string{"search", "x", "--source", "hermes"}, hangFetch, &bytes.Buffer{}, &errOut)
	})
	if code != 130 {
		t.Fatalf("%d %q", code, errOut.String())
	}
}

// A caller that cancels while `show` waits for the skill body ends the command with 130, not with a fetch error.
func TestShowCancelledWhileFetchingTheBodyIsInterrupted(t *testing.T) {
	cliHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fetch := func(url string) (string, error) {
		if url == JAWRegistryURL {
			return cliRegistry, nil
		}
		cancel()
		select {}
	}
	var code int
	var out, errOut bytes.Buffer
	within(t, 6*time.Second, func() {
		code = RunContext(ctx, []string{"show", "telegram-send"}, fetch, &out, &errOut)
	})
	if code != 130 || out.Len() != 0 || errOut.String() != "skill-search: interrupted\n" {
		t.Fatalf("%d %q %q", code, out.String(), errOut.String())
	}
}
