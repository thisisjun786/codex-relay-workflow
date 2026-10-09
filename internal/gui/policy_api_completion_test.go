package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// TestPolicyWriteCarriesTheKeptPath is the pre-merge evaluation's d2 at the route: the write kept a
// document another writer put there, and the only place those bytes exist is the path the answer
// names. The path was reported only through the warnings, and a caller reading the recovery needs
// it as its own field of the refusal.
func TestPolicyWriteCarriesTheKeptPath(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	kept := filepath.Join(filepath.Dir(file), "kept-raced.json")
	if err := os.WriteFile(kept, []byte("a document another writer put there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{
		Running: unavailablePolicyRunning,
		Register: func(context.Context, string) policystore.RegisterAnswer {
			t.Fatal("the registration step ran although the publication kept another writer's bytes")
			return policystore.RegisterAnswer{}
		},
		Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
			return nil, kept, errors.New("the exchange could not be undone")
		},
	}
	t.Cleanup(func() { policyWriteSeams = previous })
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusInternalServerError || body["error"] != "recovery_needed" {
		t.Fatalf("kept recovery: %d %v", code, body)
	}
	if got, _ := body["kept"].(string); got != kept {
		t.Fatalf("the refusal does not name the kept file: kept = %q, want %q (body %v)", got, kept, body)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the kept bytes are gone: %v", err)
	}
}

// TestPolicyWriteAnswersNotAppliedWhenTheFileMovedToWhatTheRecordNames is CRW-1001 d1 at the route:
// the file moved under the write to a document the wiring record already names, so the answer is a
// conflict that says what the document is. It is not a 500 recovery_needed, and it carries no
// recovery advice.
func TestPolicyWriteAnswersNotAppliedWhenTheFileMovedToWhatTheRecordNames(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	moved := strings.Replace(policyWritableText, "devin/swe-2", "devin/swe-3", 1)
	if moved == policyWritableText {
		t.Fatal("the fixture's exception model changed; pick another text to move the file to")
	}
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{
		Running: unavailablePolicyRunning,
		Register: func(context.Context, string) policystore.RegisterAnswer {
			t.Fatal("the registration step ran although nothing was replaced")
			return policystore.RegisterAnswer{}
		},
		// The editor saves T and a re-registration registers it after the route read the original
		// bytes, so the exchange finds T and undoes itself.
		Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
			if err := os.WriteFile(file, []byte(moved), 0o644); err != nil {
				t.Fatal(err)
			}
			rewritePolicyRecord(t, file, digestOf(moved))
			return nil, "", policystore.ErrPolicyMoved
		},
	}
	t.Cleanup(func() { policyWriteSeams = previous })
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusConflict || body["error"] != "not_applied" {
		t.Fatalf("a moved file the record names: %d %v", code, body)
	}
	for _, key := range []string{"currentDigest", "fileDigest", "registeredDigest"} {
		if body[key] != digestOf(moved) {
			t.Fatalf("%s = %v, want the digest of the document on disk (%s)", key, body[key], digestOf(moved))
		}
	}
	if _, present := body["recovery"]; present {
		t.Fatalf("a state that needs no repair carries recovery advice: %v", body)
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "not applied") {
		t.Fatalf("the answer does not say the change was not applied: %v", body)
	}
}

// TestPolicyWriteCancelledAnswerCarriesItsCauseAndTheFileDigest is CRW-1001's third review finding at
// the route: a cancelled write names why it ended and the digest of the file it left alone.
func TestPolicyWriteCancelledAnswerCarriesItsCauseAndTheFileDigest(t *testing.T) {
	policyHost(t, policyWritableText, true)
	cause := errors.New("the browser tab closed")
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{
		Running: unavailablePolicyRunning,
		Register: func(context.Context, string) policystore.RegisterAnswer {
			t.Fatal("registration ran")
			return policystore.RegisterAnswer{}
		},
	}
	t.Cleanup(func() { policyWriteSeams = previous })
	ctx, cancel := context.WithCancelCause(context.Background())
	policyWriteSeams.Swap = func(ctx context.Context, _ string, _, _ []byte, _ os.FileMode) ([]byte, string, error) {
		cancel(cause)
		return nil, "", ctx.Err()
	}
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	req := httptest.NewRequest(http.MethodPost, "/api/policy", strings.NewReader(payload)).WithContext(ctx)
	req.Host = guardHost
	for name, value := range writeHeaders() {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	policyServer(t).ServeHTTP(recorder, req)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusInternalServerError || body["error"] != "cancelled" {
		t.Fatalf("cancelled write: %d %v", recorder.Code, body)
	}
	if body["fileDigest"] != digestOf(policyWritableText) {
		t.Fatalf("fileDigest = %v, want the digest of the file the write left alone", body["fileDigest"])
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, cause.Error()) {
		t.Fatalf("reason = %q, want it to name the cause %q", reason, cause)
	}
}
