package gui

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// TestTheShutdownWaitsForAnInFlightWrite is the pre-merge evaluation's d3: a cancelled run must not
// end the process while a write that has already replaced the policy file is still registering it.
// Ending it in between leaves the file and the wiring record naming different digests, which no
// bridge starts under. This drives the real Serve path over a real listener: the registration is
// held past the grace the process used to wait, and the write must still finish with the file and the
// record agreeing.
func TestTheShutdownWaitsForAnInFlightWrite(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	// The registration is held here, so the write sits between its publication and its registration,
	// which is the window the shutdown must not cut short.
	registering := make(chan struct{})
	release := make(chan struct{})
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{
		Running: unavailablePolicyRunning,
		Register: func(_ context.Context, path string) policystore.RegisterAnswer {
			close(registering)
			<-release
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("the registration could not read the published policy: %v", err)
				return policystore.RegisterAnswer{ExitCode: 1}
			}
			rewritePolicyRecord(t, path, digestOf(string(raw)))
			return policystore.RegisterAnswer{ExitCode: 0, Stdout: []byte("{\"outcome\": \"record_updated\"}")}
		},
	}
	t.Cleanup(func() { policyWriteSeams = previous })

	ctx, cancel := context.WithCancel(context.Background())
	out := &lineWriter{}
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, Options{Port: 0, Version: "test-version"}, out) }()
	line := waitForLine(t, out)
	address := strings.TrimPrefix(strings.SplitN(line, "/#token=", 2)[0], "crw gui: serving http://")
	token := strings.SplitN(line, "/#token=", 2)[1]

	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/policy", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(tokenHeader, token)
	type answer struct {
		code int
		body string
	}
	answered := make(chan answer, 1)
	go func() {
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		if err != nil {
			answered <- answer{code: 0, body: err.Error()}
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		answered <- answer{code: response.StatusCode, body: string(body)}
	}()

	select {
	case <-registering:
	case <-time.After(15 * time.Second):
		t.Fatal("the write never reached its registration step")
	}
	cancel()
	// The process used to wait five seconds and then end; hold the registration past that so a grace
	// that does not cover the write's own phase fails here rather than in production.
	select {
	case err := <-served:
		t.Fatalf("Serve returned %v while the write was still registering: the process would end between the policy replacement and its registration", err)
	case <-time.After(6 * time.Second):
	}
	close(release)
	select {
	case got := <-answered:
		if got.code != http.StatusOK {
			t.Fatalf("the in-flight write answered %d %s, want a 200 after the shutdown was asked for", got.code, got.body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the in-flight write never answered")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return after the write finished")
	}
	// The two durable effects agree: the write ran to completion despite the cancellation.
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == policyWritableText {
		t.Fatal("the policy was not replaced")
	}
	record, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "crw-bridge-mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record), digestOf(string(after))) {
		t.Fatalf("the wiring record does not name the published bytes: %s", record)
	}
}
