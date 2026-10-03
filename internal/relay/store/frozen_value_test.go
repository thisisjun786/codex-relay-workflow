package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// A frozen MANIFEST.json is the child's to write, so its size is too, and the intake, the omission
// reader and the Stop hook (whose budget is five seconds) all read it. It is read in time
// proportional to its length, as json.loads reads it: a key is not looked for among every key
// before it. 200000 keys took about 26 s when each was, and take a quarter of a second now; the
// bound leaves room for a loaded machine on either side.
func TestFrozenDocumentIsReadInTimeProportionalToItsLength(t *testing.T) {
	// Serial: asserts a wall-clock budget, which other running tests would eat into.
	const keys = 200000
	var b strings.Builder
	b.WriteString(`{"entries": []`)
	for i := range keys {
		fmt.Fprintf(&b, `, "k%d": %d`, i, i)
	}
	// A repeated key keeps its first place and takes its last value.
	b.WriteString(`, "k0": "last"}`)
	started := time.Now()
	revision, problems, unreadable, err := VerifyFrozenDocument(context.Background(), "/frozen", []byte(b.String()), nil)
	elapsed := time.Since(started)
	if err != nil || revision != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || len(problems) != 0 || len(unreadable) != 0 {
		t.Fatalf("%q %q %q %v", revision, problems, unreadable, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a %d-key frozen document took %s to read", keys, elapsed)
	}
	document, err := decodePythonJSON(b.String())
	if err != nil {
		t.Fatal(err)
	}
	object, _ := document.(contract.OrderedObject)
	if len(object) != keys+1 || object[1].Key != "k0" || object[1].Value != "last" || object[keys].Key != fmt.Sprintf("k%d", keys-1) {
		t.Fatalf("%d fields, second %v, last %v", len(object), object[1], object[len(object)-1])
	}
}
