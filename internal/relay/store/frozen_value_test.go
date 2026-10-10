package store

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// A frozen MANIFEST.json is the child's to write, so its size is too, and the intake, the omission
// reader and the Stop hook (whose budget is five seconds) all read it. It is read in time
// proportional to its length, as json.loads reads it: a key is not looked for among every key
// before it. 200000 keys took about 26 s when each was, and take a quarter of a second now.
//
// The linearity is asserted as a ratio, which a loaded host scales on both sides: the same document read at a
// sixty-fourth of its length is timed too (best of five, so a descheduled run does not stand for the read), and 64
// times the keys may take at most 600 times as long (a linear read takes 64 times, 110 to 240 measured, collection
// being kept out of the timing; a quadratic read takes 4096 times, 17.5 s against about 25 ms with the repeated-key
// index of pyjson switched off). The 30 s on the whole read is only the hang guard; the 26 s of the quadratic read is
// under it, so it does not tell the two apart, the ratio does. The document's fields are asserted whatever the time.
func TestFrozenDocumentIsReadInTimeProportionalToItsLength(t *testing.T) {
	// Serial: the timings share the host with other running tests otherwise.
	const keys = 200000
	const small = keys / 64
	// A linear read of the whole takes 64 times the small one, and 110 to 240 times measured here over 30 runs (the
	// larger document falls out of the caches); a quadratic one takes 4096 times and more.
	const ratio = 600
	document := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"entries": []`)
		for i := range n {
			fmt.Fprintf(&b, `, "k%d": %d`, i, i)
		}
		// A repeated key keeps its first place and takes its last value.
		b.WriteString(`, "k0": "last"}`)
		return b.String()
	}
	read := func(text string) time.Duration {
		// Collection is kept out of the timing: it scales with the heap, not with the keys, and was most of the scatter.
		runtime.GC()
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		started := time.Now()
		revision, problems, unreadable, err := VerifyFrozenDocument(context.Background(), "/frozen", []byte(text), nil)
		elapsed := time.Since(started)
		if err != nil || revision != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || len(problems) != 0 || len(unreadable) != 0 {
			t.Fatalf("%q %q %q %v", revision, problems, unreadable, err)
		}
		return elapsed
	}
	best := func(text string, runs int, ceiling time.Duration) time.Duration {
		fastest := time.Duration(1<<63 - 1)
		for range runs {
			fastest = min(fastest, read(text))
			if fastest <= ceiling {
				break
			}
		}
		return fastest
	}
	smallText, text := document(small), document(keys)
	smallest := best(smallText, 5, 0)
	elapsed := best(text, 3, ratio*smallest)
	t.Logf("%d keys took %v, %d keys %v", keys, elapsed, small, smallest)
	if elapsed > 30*time.Second {
		t.Fatalf("a %d-key frozen document took %s to read, past the hang guard", keys, elapsed)
	}
	if elapsed > ratio*smallest {
		t.Fatalf("a %d-key frozen document took %s to read and a %d-key one %s: more than %d times as long for 64 times the keys, so a key is looked for among those before it", keys, elapsed, small, smallest, ratio)
	}
	object, err := decodePythonJSON(text)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := object.(contract.OrderedObject)
	if len(fields) != keys+1 || fields[1].Key != "k0" || fields[1].Value != "last" || fields[keys].Key != fmt.Sprintf("k%d", keys-1) {
		t.Fatalf("%d fields, second %v, last %v", len(fields), fields[1], fields[len(fields)-1])
	}
}
