package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

func decodedField(t *testing.T, value any, key string) any {
	t.Helper()
	object, ok := value.(contract.OrderedObject)
	if !ok {
		t.Fatalf("%T is not an object", value)
	}
	v, _ := pythonGet(object, key)
	return v
}

// The Stop hook (hook.Decode) and the omission reader read a stored receipt through DecodeRecord:
// json.loads' values and refusals, so what a typed Go decoder would reject or misread stays what
// Python read: a string where a count was expected, NaN, an integer past int64 and the last of a
// repeated key.
func TestDecodeRecordReadsAsJSONLoadsDoes(t *testing.T) {
	value, err := DecodeRecord([]byte("{\"bytes\": \"19\", \"nan\": NaN, \"big\": 99999999999999999999, \"k\": 1, \"k\": null}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodedField(t, value, "bytes"); got != "19" {
		t.Fatalf("bytes = %#v", got)
	}
	if got := decodedField(t, value, "k"); got != nil {
		t.Fatalf("the last of a repeated key is null, got %#v", got)
	}
	if got, ok := decodedField(t, value, "big").(json.Number); !ok || got.String() != "99999999999999999999" {
		t.Fatalf("an integer past int64 is kept as its digits, got %#v", got)
	}
	if got, ok := decodedField(t, value, "nan").(float64); !ok || !math.IsNaN(got) {
		t.Fatalf("NaN is a float that is not a number, got %#v", got)
	}
	for name, raw := range map[string][]byte{"truncated": []byte("{\"manifest\": ["), "empty": nil, "not UTF-8": []byte("{\"a\": \"\xff\"}"), "trailing": []byte("{} x")} {
		if _, err := DecodeRecord(raw); err == nil {
			t.Fatalf("%s was read", name)
		}
	}
}

// A stored receipt is unreadable only when its roots or its receipt text is not JSON, the roots
// first, and the detail is the first refusal's words; text that is JSON of any shape is read.
func TestDecodeStoredReceiptNamesTheFirstRefusal(t *testing.T) {
	roots, payload, unreadable := DecodeStoredReceipt("{}", "[1]")
	if unreadable != "" || roots == nil || payload == nil {
		t.Fatalf("JSON of another shape is readable: %v %v %q", roots, payload, unreadable)
	}
	_, _, both := DecodeStoredReceipt("[", "{\"a\"")
	_, _, receiptOnly := DecodeStoredReceipt("[]", "{\"a\"")
	_, _, rootsOnly := DecodeStoredReceipt("[", "{}")
	for _, got := range []string{both, receiptOnly, rootsOnly} {
		if !strings.HasPrefix(got, "JSONDecodeError: ") || len(got) <= len("JSONDecodeError: ") {
			t.Fatalf("detail %q", got)
		}
	}
	if both != rootsOnly || both == receiptOnly {
		t.Fatalf("the roots are read first: both=%q roots=%q receipt=%q", both, rootsOnly, receiptOnly)
	}
}

// The context bounds the comparison: a ctx that has ended is a comparison that did not happen,
// so the deliverable is unverifiable, never changed and never current, over the live bytes and over
// a frozen copy that answers for them; with a live ctx the same receipt is current.
func TestDeliverableStateHonorsTheContext(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(work, "deliver.txt")
	if err := os.WriteFile(artifact, []byte("the delivered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := BuildManifest([]string{artifact}, []string{work})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := ManifestRevision(entries)
	if err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(base, "frozen")
	if err := FreezeManifest(entries, reference); err != nil {
		t.Fatal(err)
	}
	payload, roots := storedValuesOf(t, entries, revision, work)

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	judge := func(ctx context.Context, reference string) (string, string, string) {
		t.Helper()
		state, binding, detail, err := DeliverableState(ctx, payload, reference, roots)
		if err != nil {
			t.Fatal(err)
		}
		return state, binding, detail
	}

	// The live bytes are the delivered ones.
	if state, binding, _ := judge(context.Background(), ""); state != DeliverableCurrent || binding != "live" {
		t.Fatalf("live ctx: %s %s", state, binding)
	}
	if state, _, detail := judge(ended, ""); state != DeliverableUnverifiable || !strings.Contains(detail, "context canceled") {
		t.Fatalf("ended ctx over live bytes: %s %q", state, detail)
	}

	// The working file moves on: the frozen copy answers for the receipt when it can be read.
	if err := os.WriteFile(artifact, []byte("a later revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, binding, _ := judge(context.Background(), reference); state != DeliverableCurrent || binding != "frozen" {
		t.Fatalf("live ctx over the frozen copy: %s %s", state, binding)
	}
	if state, _, _ := judge(context.Background(), ""); state != DeliverableChanged {
		t.Fatalf("no frozen copy: %s", state)
	}
	if state, _, detail := judge(ended, reference); state != DeliverableUnverifiable || detail == "" {
		t.Fatalf("ended ctx over the frozen copy: %s %q", state, detail)
	}
}

// endsAfterErrs is a context that ends at its n+1st Err call, so the check a function makes before
// it opens a file and the one it makes after it has read it are told apart.
type endsAfterErrs struct {
	context.Context
	calls, live int
}

func (c *endsAfterErrs) Err() error {
	c.calls++
	if c.calls > c.live {
		return context.Canceled
	}
	return nil
}

// The frozen MANIFEST.json is read whole, with the context checked before the file is opened and
// again once it has been read: a deadline that passes while a slow disk answers is a read that did
// not finish, and a path that is not a regular file is refused rather than waited on.
func TestReadWholeChecksTheContextBeforeAndAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := readWhole(context.Background(), path); err != nil || string(raw) != "{}" {
		t.Fatalf("%q %v", raw, err)
	}
	if _, err := readWhole(&endsAfterErrs{Context: context.Background(), live: 0}, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("a context that ended before the open: %v", err)
	}
	if _, err := readWhole(&endsAfterErrs{Context: context.Background(), live: 1}, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("a context that ended while the file was read: %v", err)
	}
	if _, err := readWhole(context.Background(), dir); err == nil {
		t.Fatal("a directory was read")
	}
}

// storedValuesOf is a stored receipt over entries and one artifact root, decoded as the readers
// decode it.
func storedValuesOf(t *testing.T, entries []ManifestEntry, revision, root string) (payload, roots any) {
	t.Helper()
	manifest := make([]map[string]any, len(entries))
	for i, e := range entries {
		manifest[i] = map[string]any{"path": e.Path, "sha256": e.SHA256, "bytes": *e.Bytes}
	}
	receipt, err := json.Marshal(map[string]any{"manifest": manifest, "revisionHash": revision})
	if err != nil {
		t.Fatal(err)
	}
	rootsText, err := json.Marshal([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	roots, payload, unreadable := DecodeStoredReceipt(string(rootsText), string(receipt))
	if unreadable != "" {
		t.Fatal(unreadable)
	}
	return payload, roots
}
