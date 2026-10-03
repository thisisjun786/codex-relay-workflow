package pipeline

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

func TestReviewerThresholds(t *testing.T) {
	for _, c := range []struct{ lines, count int }{{0, 2}, {100, 2}, {101, 3}, {400, 3}, {401, 4}, {1000, 4}, {1001, 5}} {
		b := testBundle()
		b.Metadata.Additions = c.lines
		cfg := Config{}
		got, err := reviewers(b.Metadata, &cfg)
		if err != nil || len(got) != c.count {
			t.Errorf("%d lines: %v %v", c.lines, got, err)
		}
	}
	for _, c := range []struct {
		path, old, mode string
		binary          bool
		count           int
	}{{"guide.md", "", "", false, 1}, {"GUIDE.MD", "old.md", "", false, 1}, {"guide.md", "code.go", "", false, 3}, {"guide.md", "", "", true, 3}, {"guide.md", "", "160000", false, 3}, {"code.go", "old.md", "", false, 3}} {
		b := testBundle()
		f := &b.Metadata.Files[0]
		f.Path, f.OldPath, f.Mode, f.Binary = c.path, c.old, c.mode, c.binary
		cfg := Config{}
		got, err := reviewers(b.Metadata, &cfg)
		if err != nil || len(got) != c.count {
			t.Errorf("%+v: %v %v", c, got, err)
		}
	}
	b := testBundle()
	cfg := Config{Thresholds: [3]int{10, 20, 30}}
	got, _ := reviewers(b.Metadata, &cfg)
	if len(got) != 5 || got[4] != Correctness {
		t.Fatal(got)
	}
	for _, bad := range []Config{{Thresholds: [3]int{0, 20, 30}}, {Thresholds: [3]int{10, 10, 30}}, {Perspectives: []Perspective{"author packet text"}}} {
		bad.Head = fakeHead{}
		calls := 0
		_, err := Run(context.Background(), b, func(context.Context, agy.Config, agy.Request) (agy.Result, error) { calls++; return findings(), nil }, bad)
		if err == nil || calls != 0 {
			t.Errorf("invalid config made calls: %v %d", err, calls)
		}
	}
}
