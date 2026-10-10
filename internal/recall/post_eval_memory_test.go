package recall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-1087 d1 -- memory file presence is judged paragraph by paragraph, as the hits are.
func TestMemoryFilePresenceCountsOnlyParagraphsTheScopeKeeps(t *testing.T) {
	home := memoryStage1Home(t)
	write := func(name, body string) {
		path := filepath.Join(memoriesDir(home), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := float64(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC).UnixMilli())
	write("here.md", "cwd: /proj/here\n\nLSP PR3956\n")
	run := func() MemorySearchResult {
		return memoryStage1Result(t, "3956 LSP", MemorySearchOptions{Home: &home, NowMs: &now, Cwd: memoryPtr("/proj/here"), CwdOnly: true})
	}
	alone := run()
	if len(alone.Hits) != 1 || !strings.Contains(strings.Join(alone.Warnings, "|"), "no word-boundary matches") {
		t.Fatalf("setup: %+v", alone)
	}
	write("other.md", "cwd: /proj/other\n\nSee /proj/here\n\n3956\n")
	got := run()
	if len(got.Hits) != 1 || got.Hits[0].Relpath != "here.md" || !strings.Contains(strings.Join(got.Warnings, "|"), "no word-boundary matches") {
		t.Fatalf("a paragraph the scope rejects blocked the relaxed pass: %+v", got)
	}
}
