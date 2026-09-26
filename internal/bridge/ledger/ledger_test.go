package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestFingerprint_whenPythonGenerated(t *testing.T) {
	// Given: pairs generated through the real Python Ledger._fingerprint.
	root := filepath.Join("..", "..", "..", "contract", "fixtures", "ledger-fingerprint")
	raw, err := os.ReadFile(filepath.Join(root, "goldens.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// DecodeJSON, not encoding/json: the lone-surrogate goldens must survive the boundary.
	decoded, err := DecodeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	pairs, _ := decoded.([]any)
	if len(pairs) < 20 {
		t.Fatalf("only %d pairs", len(pairs))
	}
	for _, item := range pairs { // When: Go fingerprints the same JSON.
		pair, _ := item.(map[string]any)
		method, _ := pair["method"].(string)
		params, _ := pair["params"].(map[string]any)
		actual, err := Fingerprint(method, params)
		if err != nil {
			t.Fatal(err)
		}
		// Then: every hash equals the one Python's Ledger._fingerprint produced.
		if actual != pair["hash"] {
			t.Errorf("params=%q: got %s want %s", params, actual, pair["hash"])
		}
	}
}
func TestPythonReceipt_whenOpenedUnchanged(t *testing.T) {
	// Given: a Python-written SQLite ledger copied to private state.
	source := filepath.Join("..", "..", "..", "contract", "fixtures", "ledger-fingerprint", "operations.sqlite3")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "operations.sqlite3")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// When: a retained request is replayed.
	fresh, receipt, err := l.Begin(context.Background(), "retained-create", "create_thread", map[string]any{"cwd": "/checkout"}, nil)
	// Then: every field of the original Python receipt is returned unchanged.
	if err != nil || fresh {
		t.Fatalf("fresh=%v receipt=%v err=%v", fresh, receipt, err)
	}
	var original string
	if err := l.db.QueryRow(`SELECT receipt FROM operations WHERE request_id=?`, "retained-create").Scan(&original); err != nil {
		t.Fatal(err)
	}
	var expected Receipt
	decoder := json.NewDecoder(bytes.NewBufferString(original))
	decoder.UseNumber()
	if err := decoder.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("receipt=%s want=%s", actual, want)
	}
	var retained string
	if err := l.db.QueryRow(`SELECT receipt FROM operations WHERE request_id=?`, "retained-create").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != original {
		t.Fatalf("Python receipt bytes changed: %q != %q", retained, original)
	}
}
func TestFingerprint_whenJSONKeyOrderOrValueChanges(t *testing.T) {
	// Given: serialized inputs with opposite key orders at both object levels.
	texts := []string{
		`{"outer":{"a":1,"b":2},"other":3}`,
		`{"other":3,"outer":{"b":2,"a":1}}`,
		`{"other":3,"outer":{"b":2,"a":4}}`,
	}
	var hashes []string
	// When: each JSON text crosses the decoder and canonical fingerprint encoder.
	for _, text := range texts {
		var params map[string]any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if err := decoder.Decode(&params); err != nil {
			t.Fatal(err)
		}
		hash, err := Fingerprint("create", params)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
	}
	// Then: source ordering is irrelevant, but a changed value changes identity.
	if hashes[0] != hashes[1] || hashes[0] == hashes[2] {
		t.Fatal(hashes)
	}
}
func TestBegin_whenNotAttemptedRearmsOnlyOnce(t *testing.T) {
	// Given: two real connections and a retry-safe receipt.
	path := filepath.Join(t.TempDir(), "operations.sqlite3")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, r, err := l.Begin(ctx, "id", "create", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r["status"] = "not_attempted"
	if _, err = l.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	l.Close()
	// When: two independent ledger connections race on that row.
	var wg sync.WaitGroup
	start := make(chan struct{})
	outcomes := make(chan bool, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := Open(path)
			if err != nil {
				failures <- err
				return
			}
			defer conn.Close()
			<-start
			fresh, _, err := conn.Begin(ctx, "id", "create", map[string]any{}, nil)
			if err != nil {
				failures <- err
				return
			}
			outcomes <- fresh
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	close(outcomes)
	// Then: only one caller may re-arm.
	seen := []bool{}
	for fresh := range outcomes {
		seen = append(seen, fresh)
	}
	if !reflect.DeepEqual(map[bool]int{true: count(seen, true), false: count(seen, false)}, map[bool]int{true: 1, false: 1}) {
		t.Fatalf("%v", seen)
	}
}
func count(values []bool, want bool) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}
	return n
}
func TestFingerprint_whenNegativeZeroInteger(t *testing.T) {
	// Given: Python json.loads("-0") becomes integer zero.
	zero, err := Fingerprint("create_thread", map[string]any{"integer": json.Number("0")})
	if err != nil {
		t.Fatal(err)
	}
	// When: Go receives the same literal at its JSON boundary.
	negative, err := Fingerprint("create_thread", map[string]any{"integer": json.Number("-0")})
	// Then: Python's two integer spellings share a hash.
	if err != nil || zero != negative {
		t.Fatalf("zero=%s negative=%s error=%v", zero, negative, err)
	}
}

func TestBegin_whenOnlyNotAttemptedRearms(t *testing.T) {
	// Given: each retained terminal or uncertain status under its own request ID.
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	for _, status := range []string{"not_attempted", "accepted", "failed", "in_progress_or_unknown"} {
		t.Run(status, func(t *testing.T) {
			_, receipt, err := l.Begin(ctx, status, "create", map[string]any{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			receipt["status"] = status
			if _, err := l.Save(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			// When: the same ID and arguments are submitted again.
			fresh, replay, err := l.Begin(ctx, status, "create", map[string]any{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Then: only the request that began nothing re-arms.
			if fresh != (status == "not_attempted") {
				t.Fatalf("status=%s fresh=%v receipt=%v", status, fresh, replay)
			}
			if !fresh && replay["status"] != status {
				t.Fatal(replay)
			}
		})
	}
}

func TestBegin_whenRequestIDOutsidePythonBounds(t *testing.T) {
	// Given: invalid identifiers outside the Python ledger's 1–128 character bound.
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, id := range []string{"", strings.Repeat("x", 129)} {
		// When: an operation begins under an invalid ID.
		_, _, err := l.Begin(context.Background(), id, "create", map[string]any{}, nil)
		// Then: the caller receives the exact Python-compatible message.
		if err == nil || err.Error() != "request_id must contain 1–128 characters" {
			t.Fatalf("id length %d: %v", len(id), err)
		}
	}
}

func TestBegin_whenArgumentsChange(t *testing.T) {
	// Given: a retained operation.
	l, err := Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if _, _, err := l.Begin(ctx, "id", "create", map[string]any{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	// When: the same ID is reused with different arguments.
	_, _, err = l.Begin(ctx, "id", "create", map[string]any{"a": 2}, nil)
	// Then: no new operation starts.
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
}
