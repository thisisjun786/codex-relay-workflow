package recall

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The recorded oracle grids keep what CXC v0.2.40 answered. A case whose answer the port changed on purpose is listed in
// testdata/<suite>/port-fixed.json by the case's key, with the port's answer in the grid's own shape, and the replay reads that answer
// in place of the recorded one. A key that names no case is a stale entry and fails the suite (portFixes.stale).

// portFixes are the port's answers of one suite, read by the case's key. A key that no case asked for is a stale entry (a typo, or a case
// that was renamed or removed): its evidence is not used, so the suite fails when it has run every case.
type portFixes struct {
	suite   string
	answers map[string]json.RawMessage
	used    map[string]bool
}

// lookup is the port's answer for the case, when it has one, and marks the entry as used.
func (p *portFixes) lookup(key string) (json.RawMessage, bool) {
	answer, ok := p.answers[key]
	if ok {
		p.used[key] = true
	}
	return answer, ok
}

// stale are the entries no case looked up, in key order.
func (p *portFixes) stale() []string {
	var out []string
	for key := range p.answers {
		if !p.used[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// portFixed reads testdata/<suite>/port-fixed.json: {"<case key>": <answer>}. When the test ends after a complete run (no -test.run
// filter, no failure), an entry that no case looked up fails it.
func portFixed(t *testing.T, suite string) *portFixes {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", suite, "port-fixed.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	p := &portFixes{suite: suite, answers: out, used: map[string]bool{}}
	t.Cleanup(func() {
		if t.Failed() || portFixedFiltered() {
			return
		}
		if stale := p.stale(); len(stale) != 0 {
			t.Errorf("testdata/%s/port-fixed.json has entries that no case asked for: %v", suite, stale)
		}
	})
	return p
}

// portFixedFiltered is true when the run selects tests or cases by name, so that not every case ran.
func portFixedFiltered() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}

// TestPortFixedReportsAStaleEntry pins the stale-entry check: an entry that no case looked up is reported, one that was is not.
func TestPortFixedReportsAStaleEntry(t *testing.T) {
	p := &portFixes{answers: map[string]json.RawMessage{"real": json.RawMessage(`1`), "typo": json.RawMessage(`2`)}, used: map[string]bool{}}
	if _, ok := p.lookup("real"); !ok {
		t.Fatal("a recorded entry is not found")
	}
	if _, ok := p.lookup("absent"); ok {
		t.Fatal("an absent entry is found")
	}
	if stale := p.stale(); !slices.Equal(stale, []string{"typo"}) {
		t.Fatalf("stale entries: %v", stale)
	}
}

// portFixedDump appends the answer of a case that differs from its recorded one to the file named by CRW_DUMP_FIXED, so that a reviewed
// change can regenerate the suite's port-fixed.json. It does nothing when the variable is unset.
func portFixedDump(suite, key string, answer any) {
	path := os.Getenv("CRW_DUMP_FIXED")
	if path == "" {
		return
	}
	b, err := json.Marshal(map[string]any{"suite": suite, "key": key, "answer": answer})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
