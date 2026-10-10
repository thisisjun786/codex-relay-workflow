package shellir

import (
	"strings"
	"testing"
	"time"
)

// TestProgramCreatedByTheText: a program word that the same text creates (ln -sf /bin/bash X, cp /bin/bash X, a redirect into X)
// is not the program the reader sees by its name: the file the text wrote is what runs (CRW-765, 5th generation). The text is
// unreadable. An unrelated name, and a created file that is only data, stay readable.
func TestProgramCreatedByTheText(t *testing.T) {
	unreadable := []string{
		"ln -sf /bin/bash X\nX <<'EOF'\necho hi\nEOF",
		"ln -sf /bin/bash X; ./X",
		"cp /bin/bash X && ./X",
		"mv /bin/bash X; X",
		"install /bin/bash X; ./X",
		"printf '#!/bin/sh\\nrm -rf .' > X; chmod +x X; ./X",
		"tee X </dev/null; ./X",
		"cd sub; ln -s /bin/bash X; ./X",
	}
	for _, cmd := range unreadable {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q: read, want unreadable (the text creates the program it runs)", cmd)
		}
	}
	readable := []string{
		"ln -sf /bin/bash X; Y",
		"cp a.txt b.txt; ./c",
		"echo hi > X; cat X",
		"ln -sf /bin/bash X",
	}
	for _, cmd := range readable {
		if _, err := Analyze(cmd, "/work"); err != nil {
			t.Errorf("%q: unreadable: %v", cmd, err)
		}
	}
}

// TestCreatedProgramCheckIsLinear: a text of thousands of redirections and commands is read in time proportional to its size.
//
// The linearity is asserted on the work, not on the clock, so a loaded host cannot move it: the allocations of one
// reading are counted (testing.AllocsPerRun is deterministic) for the text at the size limit and at a quarter of it, and
// four times the text may cost at most five times the allocations. A linear read costs exactly four times (12962,
// 51974 and 207954 allocations at 4, 16 and 64 KB); a read that rescans the records for each program word, which is
// what the check was before CRW-1028 and what it does with the created set rebuilt on each call, costs fifteen times
// (940270 and 14386934 at 16 and 64 KB). The time (30 s) is only the hang guard, and the text must be read whatever the cost.
func TestCreatedProgramCheckIsLinear(t *testing.T) {
	text := func(size int) string {
		var b strings.Builder
		for b.Len() < size-32 {
			b.WriteString("echo a > f; ./g; ")
		}
		return b.String()
	}
	full, quarter := text(MaxCommandBytes), text(MaxCommandBytes/4)
	cost := func(command string) float64 {
		return testing.AllocsPerRun(3, func() {
			if _, err := Analyze(command, "/work"); err != nil {
				t.Fatalf("unreadable: %v", err)
			}
		})
	}
	small := cost(quarter)
	start := time.Now()
	large := cost(full)
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("reading %d bytes took %v, past the hang guard", len(full), d)
	}
	if large > 5*small {
		t.Errorf("reading %d bytes cost %.0f allocations and %d bytes %.0f: more than 5 times as much for 4 times the text", len(full), large, len(quarter), small)
	}
}
