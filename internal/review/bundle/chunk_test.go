package bundle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSpecialFiles(t *testing.T) {
	for _, kind := range []string{"deleted", "binary", "rename", "rename-edit", "file-directory", "directory-file", "symlink-file", "file-symlink", "gitlink", "empty-file"} {
		t.Run(kind, func(t *testing.T) {
			r := newRepository(t)
			old := strings.Repeat("same\n", 20)
			path := "p"
			if kind == "directory-file" {
				path = "p/q"
			}
			if kind == "binary" {
				old = "\x00old"
			}
			if kind == "symlink-file" {
				if err := os.Symlink("target", filepath.Join(r.dir, "p")); err != nil {
					t.Fatal(err)
				}
			} else {
				r.write(path, old)
			}
			base := r.commit()
			expectedFiles, adds, dels := 1, 0, 0
			switch kind {
			case "deleted":
				if err := os.Remove(filepath.Join(r.dir, path)); err != nil {
					t.Fatal(err)
				}
				dels = 20
			case "binary":
				r.write(path, "\x00new")
			case "rename", "rename-edit":
				if err := os.Rename(filepath.Join(r.dir, "p"), filepath.Join(r.dir, "renamed\t\n.txt")); err != nil {
					t.Fatal(err)
				}
				if kind == "rename-edit" {
					r.write("renamed\t\n.txt", strings.Repeat("same\n", 19)+"edited\n")
					adds, dels = 1, 1
				}
			case "file-directory", "directory-file":
				if err := os.Remove(filepath.Join(r.dir, path)); err != nil {
					t.Fatal(err)
				}
				if kind == "directory-file" {
					if err := os.Remove(filepath.Join(r.dir, "p")); err != nil {
						t.Fatal(err)
					}
					r.write("p", "replacement\n")
				} else {
					r.write("p/q", "replacement\n")
				}
				expectedFiles, adds, dels = 2, 1, 20
			case "symlink-file":
				if err := os.Remove(filepath.Join(r.dir, "p")); err != nil {
					t.Fatal(err)
				}
				r.write("p", "regular\n")
				adds, dels = 1, 1
			case "file-symlink":
				if err := os.Remove(filepath.Join(r.dir, "p")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target", filepath.Join(r.dir, "p")); err != nil {
					t.Fatal(err)
				}
				adds, dels = 1, 20
			case "gitlink":
				r.git("update-index", "--add", "--cacheinfo", "160000,"+base+",module")
				adds = 1
			case "empty-file":
				r.write("empty", "")
			}
			var head string
			if kind == "gitlink" {
				r.git("commit", "-qm", "fixture")
				head = strings.TrimSpace(r.git("rev-parse", "HEAD"))
			} else {
				head = r.commit()
			}
			b, err := Build(context.Background(), r.dir, base, head, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if b.Metadata.FileCount != expectedFiles || b.Metadata.Additions != adds || b.Metadata.Deletions != dels {
				t.Fatalf("wrong manual totals: %+v", b.Metadata)
			}
			text := b.Chunks[0].Text
			f := b.Metadata.Files[0]
			if strings.Count(text, "<<< BEGIN DIFF ") != expectedFiles {
				t.Fatal("duplicate or missing file groups")
			}
			switch kind {
			case "deleted":
				if f.Status != "D" || f.HeadBytes != 0 || !strings.Contains(text, "deleted; no head text") {
					t.Fatal("deleted head")
				}
			case "binary":
				if !f.Binary || strings.ContainsRune(text, 0) || !strings.Contains(text, "binary; not inlined") {
					t.Fatal("binary inlined")
				}
			case "rename", "rename-edit":
				if f.OldPath != "p" || !strings.HasPrefix(f.Status, "R") || f.Path != "renamed\t\n.txt" || f.HeadLines != 20 || !strings.Contains(text, "1| same") {
					t.Fatalf("rename lost: %+v", f)
				}
			case "symlink-file", "file-symlink":
				if f.Status != "T" || strings.Count(text, "| diff --git ") != 2 || f.HeadLines != 1 {
					t.Fatal("type-change grouping")
				}
			case "gitlink":
				if f.Mode != "160000" || !strings.Contains(text, "gitlink; no file text") {
					t.Fatal("gitlink")
				}
			}
		})
	}
}

func TestCapsAndGrouping(t *testing.T) {
	r := newRepository(t)
	var lines []string
	for i := 1; i <= 80; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d\n", i))
	}
	for _, p := range []string{"a", "b", "c"} {
		r.write(p, strings.Join(lines, ""))
	}
	for _, p := range []string{"AGENTS.md", "POLICY.md", "CONTRIBUTING.md"} {
		r.write(p, strings.Repeat("rule\n", 30))
	}
	base := r.commit()
	lines[39] = "edited\n"
	for _, p := range []string{"a", "b", "c"} {
		r.write(p, strings.Join(lines, ""))
	}
	head := r.commit()
	for _, tc := range []struct {
		name      string
		cap       int
		truncated bool
	}{{"full", 5000, false}, {"hunk", 120, true}, {"tiny", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Build(context.Background(), r.dir, base, head, Options{Context: 2, MaxFileBytes: tc.cap, MaxRuleBytes: 90})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range b.Metadata.Files {
				if f.TextBytes > tc.cap || f.Truncated != tc.truncated {
					t.Fatalf("cap: %+v", f)
				}
			}
			if b.Metadata.RuleBytes > 90 {
				t.Fatal("rule cap exceeded")
			}
			for _, rule := range b.Metadata.Rules {
				if !rule.Truncated || rule.Bytes != 30 {
					t.Fatalf("unequal excerpt: %+v", rule)
				}
			}
			if tc.truncated && (len(b.Metadata.TruncatedFiles) != 3 || !strings.Contains(b.Chunks[0].Text, "[TRUNCATED]")) {
				t.Fatal("truncation unmarked")
			}
			if tc.name == "hunk" && !strings.Contains(b.Chunks[0].Text, "40| edited") {
				t.Fatal("head snippet is not around the hunk")
			}
		})
	}
	for _, tc := range []struct {
		cap, chunks int
		paths       [][]string
	}{{2400, 1, [][]string{{"a", "b", "c"}}}, {1700, 2, [][]string{{"a", "b"}, {"c"}}}, {1200, 3, [][]string{{"a"}, {"b"}, {"c"}}}} {
		b, err := Build(context.Background(), r.dir, base, head, Options{Context: 2, MaxFileBytes: 120, MaxRuleBytes: 90, MaxChunkBytes: tc.cap})
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Chunks) != tc.chunks {
			t.Fatalf("cap %d: chunks %d sizes %v", tc.cap, len(b.Chunks), b.Metadata.ChunkSizes)
		}
		for i, c := range b.Chunks {
			if len(c.Text) > tc.cap || !reflect.DeepEqual(c.Paths, tc.paths[i]) || strings.Count(c.Text, "<<< BEGIN DIFF ") != len(c.Paths) || !strings.Contains(c.Text, "rule") {
				t.Fatalf("bad group at cap %d: %+v", tc.cap, c)
			}
		}
	}
	for _, o := range []Options{{MaxChunkBytes: 1}, {Context: 2, MaxFileBytes: 1, MaxRuleBytes: 1, MaxChunkBytes: 400}} {
		if b, err := Build(context.Background(), r.dir, base, head, o); b != nil || !errors.Is(err, ErrSizeLimit) {
			t.Fatalf("size error hidden: %v", err)
		}
	}
}
