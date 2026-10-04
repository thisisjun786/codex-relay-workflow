package registry

import (
	"fmt"
	"testing"
)

// CRW-470: the rule that reads a naming of a suppressed receipt through that receipt.

func TestReadThroughFollowsASuppressedReceiptToWhatItReplaced(t *testing.T) {
	t.Parallel()
	// declaredBy: for each revision hash, the predecessor hash of every suppressed receipt holding
	// it. live: how many live revisions or requested predecessors hold a hash.
	live := map[string]int{"L": 1, "twice": 2, "held": 1}
	declaredBy := map[string][]string{
		"root": {""}, "overL": {"L"}, "overRoot": {"root"}, "overOverL": {"overL"},
		"overTwice": {"twice"}, "overNobody": {"nobody"}, "overOverNobody": {"overNobody"},
		"two": {"", "L"}, "loopA": {"loopB"}, "loopB": {"loopA"}, "self": {"self"},
		"held": {""},
	}
	held := func(hash string) int { return live[hash] }
	for _, tc := range []struct {
		name, named, effective string
		ok                     bool
	}{
		{"a receipt that replaced nothing is no predecessor", "root", "", true},
		{"a receipt that replaced a live revision stands for it", "overL", "L", true},
		{"through two receipts in a row", "overOverL", "L", true},
		{"through a receipt that replaced a receipt that replaced nothing", "overRoot", "", true},
		{"a suppressed receipt that replaced a live revision held twice", "overTwice", "", false},
		{"a suppressed receipt that replaced a revision nobody holds", "overNobody", "", false},
		{"through two receipts to a revision nobody holds", "overOverNobody", "", false},
		{"a hash two suppressed receipts hold", "two", "", false},
		{"a hash nobody holds", "nobody", "", false},
		{"a hash a live revision holds is not read through", "held", "", false},
		{"two suppressed receipts replacing each other", "loopA", "", false},
		{"a suppressed receipt replacing itself", "self", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			through := ReadThrough([]string{tc.named}, held, declaredBy)
			effective, ok := through[tc.named]
			if effective != tc.effective || ok != tc.ok {
				t.Fatalf("ReadThrough(%q) = %q, %v; want %q, %v", tc.named, effective, ok, tc.effective, tc.ok)
			}
		})
	}
}

// Every receipt of one long suppressed chain is named at once: each hash is answered the same
// whichever naming reaches it first, and the chain is walked once, not once per naming.
func TestReadThroughAnswersEveryNamingOfALongChainAlike(t *testing.T) {
	t.Parallel()
	const length = 5000
	declaredBy := map[string][]string{}
	var named []string
	for i := range length {
		declared := ""
		if i > 0 {
			declared = fmt.Sprintf("s%d", i-1)
		}
		declaredBy[fmt.Sprintf("s%d", i)] = []string{declared}
		named = append(named, fmt.Sprintf("s%d", length-1-i))
	}
	walks := 0
	through := ReadThrough(named, func(string) int { walks++; return 0 }, declaredBy)
	if len(through) != length {
		t.Fatalf("%d of %d namings read through", len(through), length)
	}
	for hash, effective := range through {
		if effective != "" {
			t.Fatalf("%s reads as naming %q", hash, effective)
		}
	}
	// the first naming walks the chain (two probes a step); the other namings find their end
	// remembered, so the probes stay linear in the chain, not in its square
	if walks > 8*length {
		t.Fatalf("%d probes for a chain of %d", walks, length)
	}
}

func TestReadThroughReadsNothingWhenNothingIsSuppressed(t *testing.T) {
	t.Parallel()
	held := func(hash string) int { return map[string]int{"held": 1}[hash] }
	for _, named := range [][]string{nil, {""}, {"", "held"}, {"nobody"}} {
		if through := ReadThrough(named, held, nil); through != nil {
			t.Fatalf("%v: %v", named, through)
		}
	}
}

// Several namings in one call share what is remembered, failures included: the ones that cannot
// be read through stay out of the answer whichever is asked first.
func TestReadThroughKeepsFailuresOutWhateverIsAskedFirst(t *testing.T) {
	t.Parallel()
	declaredBy := map[string][]string{"overNobody": {"nobody"}, "overOverNobody": {"overNobody"}, "root": {""}, "overRoot": {"root"}}
	held := func(string) int { return 0 }
	for _, named := range [][]string{
		{"overOverNobody", "overNobody", "overRoot", "root"},
		{"overNobody", "overOverNobody", "root", "overRoot"},
	} {
		through := ReadThrough(named, held, declaredBy)
		if len(through) != 2 || through["root"] != "" || through["overRoot"] != "" {
			t.Fatalf("%v read through as %v", named, through)
		}
		if _, ok := through["overNobody"]; ok {
			t.Fatalf("%v: a chain ending at a hash nobody holds read through: %v", named, through)
		}
	}
}
