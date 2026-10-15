package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// seed stores a row on id with params whose receipt is Begin's with status, and, for versionless, no fingerprintVersion
// (a pre-upgrade receipt).
func seed(t *testing.T, l *Ledger, id string, params map[string]any, status string, versionless bool) Receipt {
	t.Helper()
	ctx := context.Background()
	_, receipt, err := l.Begin(ctx, id, "create_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if versionless {
		delete(receipt, "fingerprintVersion")
	}
	receipt["status"] = status
	receipt["attemptedEffects"] = []string{}
	if status == "not_attempted" {
		receipt["retrySafe"] = true
		receipt["error"] = "Error: appserver dial: no such file"
	}
	saved, err := l.Save(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func raw(t *testing.T, l *Ledger, id string) string {
	t.Helper()
	stored, err := l.RawReceipt(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return string(stored)
}

// CRW-1506: BeginUnless asks refuse inside Begin's own transaction, so a row that turned not_attempted after another
// connection's lookup (another process on the same ledger) opens no attempt, and a refused insert leaves no row.
func TestLedgerBeginUnless_refusesInsideItsOwnTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.sqlite3")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	params := map[string]any{"prompt": nil}
	refused := errors.New("refused")
	var asked []Receipt
	refuse := func(held Receipt) error {
		asked = append(asked, held)
		return refused
	}

	// No row: the insert is refused and rolled back.
	if _, _, err := l.BeginUnless(ctx, "absent", "create_thread", params, nil, refuse); !errors.Is(err, refused) {
		t.Fatalf("absent: %v", err)
	}
	if _, err := l.Get(ctx, "absent"); !errors.Is(err, ErrUnknown) || len(asked) != 1 || asked[0] != nil {
		t.Fatalf("a refused insert left a row (%v) or was asked %v", err, asked)
	}

	// A row in flight when this connection looked it up turns not_attempted through the other connection before Begin.
	seed(t, l, "flight", params, "in_progress_or_unknown", false)
	looked, err := l.Lookup(ctx, "flight", "create_thread", params, nil)
	if err != nil || looked["status"] != "in_progress_or_unknown" {
		t.Fatalf("lookup: %v %v", looked, err)
	}
	stored, err := other.Get(ctx, "flight")
	if err != nil {
		t.Fatal(err)
	}
	stored["status"] = "not_attempted"
	if _, err = other.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	before := raw(t, l, "flight")
	asked = nil
	if _, _, err := l.BeginUnless(ctx, "flight", "create_thread", params, nil, refuse); !errors.Is(err, refused) {
		t.Fatalf("not_attempted: %v", err)
	}
	if after := raw(t, l, "flight"); after != before || len(asked) != 1 || asked[0]["status"] != "not_attempted" {
		t.Fatalf("a refused re-run changed the row or was asked %v:\n%s\n%s", asked, before, after)
	}

	// A row in any other status is answered for replay, refuse not asked.
	for _, status := range []string{"in_progress_or_unknown", "accepted", "failed", "outcome_unknown"} {
		seed(t, l, status, params, status, false)
		asked = nil
		fresh, replay, err := l.BeginUnless(ctx, status, "create_thread", params, nil, refuse)
		if err != nil || fresh || replay["status"] != status || len(asked) != 0 {
			t.Fatalf("%s: fresh=%v replay=%v err=%v asked=%v", status, fresh, replay, err, asked)
		}
	}

	// A nil answer begins as Begin does.
	fresh, receipt, err := l.BeginUnless(ctx, "allowed", "create_thread", params, nil, func(Receipt) error { return nil })
	if err != nil || !fresh || receipt["status"] != "in_progress_or_unknown" {
		t.Fatalf("allowed: fresh=%v receipt=%v err=%v", fresh, receipt, err)
	}
}

// CRW-1506: Retire writes only a not_attempted receipt of exactly these arguments, makes it failed with the reason and the
// outcome it ends as its error, keeps every other member, and leaves every other row as it was.
func TestLedgerRetire_endsOnlyANotAttemptedReservationOfTheseArguments(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	promptless := map[string]any{"cwd": "/resolved", "prompt": nil}
	const reason = "retired for the test"

	if retired, now, err := l.Retire(ctx, "absent", "create_thread", promptless, nil, reason); retired || now != nil || err != nil {
		t.Fatalf("absent: %v %v %v", retired, now, err)
	}
	if _, err := l.Get(ctx, "absent"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("retiring an absent id stored a row: %v", err)
	}

	// A reservation made after a retry carries attempt and priorAttempts; both are kept.
	seed(t, l, "reserved", promptless, "not_attempted", false)
	fresh, rerun, err := l.Begin(ctx, "reserved", "create_thread", promptless, nil)
	if err != nil || !fresh {
		t.Fatalf("re-run: %v %v", fresh, err)
	}
	rerun["status"] = "not_attempted"
	rerun["retrySafe"] = true
	rerun["error"] = "Error: second dial failed"
	rerun["attemptedEffects"] = []string{}
	if _, err = l.Save(ctx, rerun); err != nil {
		t.Fatal(err)
	}
	before, err := l.Get(ctx, "reserved")
	if err != nil {
		t.Fatal(err)
	}
	retired, now, err := l.Retire(ctx, "reserved", "create_thread", promptless, nil, reason)
	if err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	stored, err := l.Get(ctx, "reserved")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(now, stored) {
		t.Fatalf("Retire answered %v, the ledger holds %v", now, stored)
	}
	if stored["status"] != "failed" || stored["retrySafe"] != false || stored["error"] != reason+" (the not_attempted outcome it ends: Error: second dial failed)" {
		t.Fatalf("not retired: %v", stored)
	}
	if len(stored) != len(before) {
		t.Fatalf("members changed:\n%v\n%v", before, stored)
	}
	for key, value := range before {
		if key != "status" && key != "retrySafe" && key != "error" && key != "updatedAt" && !reflect.DeepEqual(stored[key], value) {
			t.Fatalf("%s changed: %v -> %v", key, value, stored[key])
		}
	}
	if stored["attempt"] != json.Number("2") || len(stored["priorAttempts"].([]any)) != 1 {
		t.Fatalf("attempt or priorAttempts not kept: %v", stored)
	}

	// Once failed, every build replays it: Begin re-runs nothing, and a second Retire writes nothing.
	settled := raw(t, l, "reserved")
	if fresh, replay, err := l.Begin(ctx, "reserved", "create_thread", promptless, nil); err != nil || fresh || replay["status"] != "failed" {
		t.Fatalf("a retired receipt re-ran: %v %v %v", fresh, replay, err)
	}
	if retired, now, err := l.Retire(ctx, "reserved", "create_thread", promptless, nil, reason); retired || err != nil || now["status"] != "failed" || raw(t, l, "reserved") != settled {
		t.Fatalf("a second retirement: %v %v %v", retired, now, err)
	}

	// Rows Retire never writes: one with a host effect or in flight, and one of other arguments (a prompted
	// reservation is other arguments for the prompt-less params).
	for _, status := range []string{"accepted", "failed", "outcome_unknown", "in_progress_or_unknown"} {
		seed(t, l, status, promptless, status, false)
		before := raw(t, l, status)
		retired, now, err := l.Retire(ctx, status, "create_thread", promptless, nil, reason)
		if err != nil || retired || now["status"] != status || raw(t, l, status) != before {
			t.Fatalf("%s: %v %v %v", status, retired, now, err)
		}
	}
	seed(t, l, "prompted", map[string]any{"cwd": "/resolved", "prompt": "hello"}, "not_attempted", false)
	prompted := raw(t, l, "prompted")
	if retired, _, err := l.Retire(ctx, "prompted", "create_thread", promptless, nil, reason); !errors.Is(err, ErrConflict) || retired || raw(t, l, "prompted") != prompted {
		t.Fatalf("prompted: %v %v", retired, err)
	}

	// A version-1 receipt is compared through the legacy params, as Lookup compares it.
	seed(t, l, "legacy", promptless, "not_attempted", true)
	alias := map[string]any{"cwd": "/alias", "prompt": nil}
	if retired, _, err := l.Retire(ctx, "legacy", "create_thread", alias, nil, reason); !errors.Is(err, ErrConflict) || retired {
		t.Fatalf("legacy without the legacy params: %v %v", retired, err)
	}
	resolved := func() map[string]any { return map[string]any{"cwd": "/resolved", "prompt": nil} }
	if retired, now, err := l.Retire(ctx, "legacy", "create_thread", alias, resolved, reason); err != nil || !retired || !strings.HasPrefix(now["error"].(string), reason) {
		t.Fatalf("legacy: %v %v %v", retired, now, err)
	}
}
