package bundle

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBundleCommittedMaterial(t *testing.T) {
	r := newRepository(t)
	var lines []string
	for n := 1; n <= 60; n++ {
		lines = append(lines, fmt.Sprintf("line-%02d\n", n))
	}
	r.write("code.txt", strings.Join(lines, ""))
	for _, doc := range []string{"AGENTS.md", "POLICY.md", "CONTRIBUTING.md"} {
		r.write(doc, "rule-"+doc+"\n")
	}
	anchor := r.commit()
	r.write("base-only", "must not enter diff\n")
	baseTip := r.commit()
	r.git("checkout", "-qb", "topic", anchor)
	lines[29] = "edited-at-head\n"
	r.write("code.txt", strings.Join(lines, ""))
	head := r.commit()
	b, err := Build(context.Background(), r.dir, "base", head, Options{Context: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Chunks) != 1 {
		t.Fatalf("expected one complete chunk, got %d", len(b.Chunks))
	}
	for _, want := range []string{"@@ -28,5 +28,5 @@", "-line-30", "+edited-at-head", "60| line-60", "rule-AGENTS.md", "rule-POLICY.md", "rule-CONTRIBUTING.md"} {
		if !strings.Contains(b.Chunks[0].Text, want) {
			t.Errorf("missing committed material %q", want)
		}
	}
	if strings.Contains(b.Chunks[0].Text, "base-only") {
		t.Error("diff did not use merge-base")
	}
	m := b.Metadata
	if m.Base != baseTip || m.Head != head || m.MergeBase != anchor || len(m.PatchID) != 40 || m.FileCount != 1 || m.Additions != 1 || m.Deletions != 1 || m.ChunkCount != 1 || m.ChunkSizes[0] != len(b.Chunks[0].Text) || m.TotalBytes != len(b.Chunks[0].Text) || m.HeadBytes == 0 || m.RuleBytes == 0 || m.DiffBytes == 0 {
		t.Fatalf("incomplete metadata: %+v", m)
	}
	wide, err := Build(context.Background(), r.dir, "base", head, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wide.Metadata.PatchID != m.PatchID || !strings.Contains(wide.Chunks[0].Text, "@@ -5,51 +5,51 @@") {
		t.Fatal("default context or context-independent patch-id")
	}
	r.write("code.txt", "dirty worktree must not enter\n")
	r.write("POLICY.md", "dirty rule\n")
	again, err := Build(context.Background(), r.dir, "base", head, Options{Context: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b, again) {
		t.Error("same commits/options rendered different bytes or metadata")
	}
}
