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
func TestCreatedProgramCheckIsLinear(t *testing.T) {
	var b strings.Builder
	for b.Len() < MaxCommandBytes-32 {
		b.WriteString("echo a > f; ./g; ")
	}
	start := time.Now()
	_, err := Analyze(b.String(), "/work")
	if err != nil {
		t.Fatalf("unreadable: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("reading %d bytes took %v", b.Len(), d)
	}
}
