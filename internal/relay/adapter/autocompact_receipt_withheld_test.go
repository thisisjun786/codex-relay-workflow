package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-1000 (verification round 2): the limit is stored before thread/resume goes out, so a send whose
// resume the transport certainly withheld withdraws it, and a send whose pre-resume store failed sends
// nothing and stays retryable under the same request ID.

func idleHost(t *testing.T) *fakehost.Server {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	return host
}

func assertNoLimitRecorded(t *testing.T, a *Adapter, request string, receipt map[string]any) {
	t.Helper()
	if receipt != nil {
		if value, present := autoCompactSentValue(t, receipt); present {
			t.Fatalf("no resume left, but the returned receipt records the limit %d as requested: %v", value, receipt)
		}
	}
	stored, err := a.GetOperation(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	storedMap, _ := plain(stored).(map[string]any)
	if value, present := autoCompactSentValue(t, storedMap); present {
		t.Fatalf("no resume left, but the stored receipt records the limit %d as requested: %v", value, storedMap)
	}
}

// The connection ends after the watch of the turn was admitted and the limit was stored, before the
// resume is written: the client withholds the request, and the receipt withdraws the limit.
func TestASendWhoseConnectionEndedAfterTheWatchRecordsNoLimit(t *testing.T) {
	host := idleHost(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	defer client.Close()
	closed := false
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.sqlite3"), ledger.Options{Encode: func(r ledger.Receipt) ([]byte, error) {
		if _, ok := r["settings"]; ok && !closed {
			closed = true
			_ = client.Close()
		}
		return encodeReceipt(r)
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := childRecord(false, "")
	a := New(Options{RPC: client, Ledger: l, Policy: autoCompactChildPolicy(t, record)})
	t.Cleanup(func() { _ = a.Close() })
	receipt := sendRecord(t, a, "send-lost-after-watch", record)
	if !closed || host.Count("thread/resume") != 0 {
		t.Fatalf("the injection did not keep the resume from the host: closed=%t resumes=%d", closed, host.Count("thread/resume"))
	}
	assertNoLimitRecorded(t, a, "send-lost-after-watch", receipt)
}

// A failure the client reports before the resume's frame reaches the socket is the same: nothing went out.
func TestASendWhoseResumeFailedBeforeTheWriteRecordsNoLimit(t *testing.T) {
	host := idleHost(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	defer client.Close()
	client.FailBeforeWrite("thread/resume")
	record := childRecord(false, "")
	a := lostResumeAdapter(t, client, autoCompactChildPolicy(t, record))
	receipt := sendRecord(t, a, "send-failed-before-write", record)
	if host.Count("thread/resume") != 0 {
		t.Fatalf("the injection did not keep the resume from the host: %d", host.Count("thread/resume"))
	}
	assertNoLimitRecorded(t, a, "send-failed-before-write", receipt)
}

// The store of the limit before the resume fails once: nothing is sent, the receipt is not_attempted
// and retry-safe, and the same request ID then sends normally.
func TestAFailedPreResumeStoreSendsNothingAndStaysRetryable(t *testing.T) {
	failed := false
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.sqlite3"), ledger.Options{Encode: func(r ledger.Receipt) ([]byte, error) {
		if _, ok := r["settings"]; ok && !failed {
			failed = true
			return nil, errors.New("injected transient pre-resume ledger failure")
		}
		return encodeReceipt(r)
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := childRecord(false, "")
	rpc := &mcpRPC{}
	a := New(Options{RPC: rpc, Ledger: l, Policy: autoCompactChildPolicy(t, record)})
	t.Cleanup(func() { _ = a.Close() })
	receipt := sendRecord(t, a, "send-store-failed", record)
	if !failed {
		t.Fatal("the injection did not run")
	}
	if _, resumed := rpc.params["thread/resume"]; resumed {
		t.Fatalf("a resume went out although its receipt could not be stored: %v", rpc.calls)
	}
	effects, _ := receipt["attemptedEffects"].([]any)
	if receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || receipt["attemptedEffects"] == nil || len(effects) != 0 {
		t.Fatalf("nothing was sent, but the receipt does not say the request can be retried: %v", receipt)
	}
	assertNoLimitRecorded(t, a, "send-store-failed", receipt)
	again := sendRecord(t, a, "send-store-failed", record)
	if again["status"] != "accepted" {
		t.Fatalf("the retry under the same request ID did not send: %v", again)
	}
	if value, present := autoCompactSentValue(t, again); !present || value != 550000 {
		t.Fatalf("the retried resume does not record the limit it sent: %v", again)
	}
}

// When the store before the resume fails and the receipt that says so cannot be stored either, the send
// returns the store's error and still sends nothing.
func TestAFailedPreResumeStoreWhoseRefusalCannotBeStoredReturnsTheError(t *testing.T) {
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.sqlite3"), ledger.Options{Encode: func(r ledger.Receipt) ([]byte, error) {
		if _, ok := r["settings"]; ok || r["status"] == "not_attempted" {
			return nil, errors.New("injected lasting ledger failure")
		}
		return encodeReceipt(r)
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := childRecord(false, "")
	rpc := &mcpRPC{}
	a := New(Options{RPC: rpc, Ledger: l, Policy: autoCompactChildPolicy(t, record)})
	t.Cleanup(func() { _ = a.Close() })
	_, err = a.Send(context.Background(), "send-store-lost", "thread-1", "hello", record, nil, 0)
	if err == nil {
		t.Fatal("the send reported success although no receipt could be stored")
	}
	if _, resumed := rpc.params["thread/resume"]; resumed {
		t.Fatalf("a resume went out although its receipt could not be stored: %v", rpc.calls)
	}
}

// CRW-1000 (post-evaluation d1): the transport's proof that the resume was withheld decides the withdrawal,
// whatever else happens to the caller. A caller that is cancelled in the same moment must not turn the
// withheld error into a bare cancellation that keeps a limit no frame carried.
func TestACancelledSendWhoseResumeWasWithheldRecordsNoLimit(t *testing.T) {
	host := idleHost(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := false
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.sqlite3"), ledger.Options{Encode: func(r ledger.Receipt) ([]byte, error) {
		if _, ok := r["settings"]; ok && !closed {
			closed = true
			_ = client.Close()
			cancel()
		}
		return encodeReceipt(r)
	}})
	if err != nil {
		t.Fatal(err)
	}
	record := childRecord(false, "")
	a := New(Options{RPC: client, Ledger: l, Policy: autoCompactChildPolicy(t, record)})
	if _, err := a.Send(ctx, "send-cancelled-withheld", "thread-1", "hello", record, nil, 0); err == nil {
		t.Fatal("the cancelled send reported success")
	}
	t.Cleanup(func() { _ = a.Close() })
	// Send answers the cancelled caller at once; the send settles its receipt on its own goroutine.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if stored, err := a.GetOperation(context.Background(), "send-cancelled-withheld"); err == nil {
			if m, _ := plain(stored).(map[string]any); m["status"] == "outcome_unknown" {
				break
			}
		}
	}
	if !closed || host.Count("thread/resume") != 0 {
		t.Fatalf("the injection did not keep the resume from the host: closed=%t resumes=%d", closed, host.Count("thread/resume"))
	}
	assertNoLimitRecorded(t, a, "send-cancelled-withheld", nil)
}
