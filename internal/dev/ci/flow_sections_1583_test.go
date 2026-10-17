//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var flowSectionLink = regexp.MustCompile(`\]\(((?:script|flow)/[^)#\s]+\.md)(?:#[^)\s]+)?\)`)

// readFlowReference follows the section index for the two split references.
// Other documents are still read as one file.
func readFlowReference(t *testing.T, path string) string {
	t.Helper()
	text := readRepoText(t, path)
	if path != "docs/flow/script.md" && path != canonRefDir+"flow.md" {
		return text
	}
	matches := flowSectionLink.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		t.Fatalf("%s has no section links", path)
	}
	var out strings.Builder
	for _, match := range matches {
		out.WriteString(readRepoText(t, filepath.ToSlash(filepath.Join(filepath.Dir(path), match[1]))))
	}
	return out.String()
}

// joinFlowSectionTexts retains the context of each reference for the existing
// terminology checks after its sections move to separate files.
func joinFlowSectionTexts(t *testing.T, texts map[string]string) {
	t.Helper()
	for _, index := range []string{"docs/flow/script.md", canonRefDir + "flow.md"} {
		matches := flowSectionLink.FindAllStringSubmatch(texts[index], -1)
		if len(matches) == 0 {
			continue
		}
		var out strings.Builder
		for _, match := range matches {
			path := filepath.ToSlash(filepath.Join(filepath.Dir(index), match[1]))
			text, ok := texts[path]
			if !ok {
				t.Fatalf("%s indexes an unscanned section %s", index, path)
			}
			out.WriteString(text)
			delete(texts, path)
		}
		texts[index] = out.String()
	}
}

// TestFlowSectionIndexes checks both directions: every index entry is a file,
// and every section file is indexed exactly once.
func TestFlowSectionIndexes(t *testing.T) {
	for _, index := range []string{"docs/flow/script.md", canonRefDir + "flow.md"} {
		t.Run(index, func(t *testing.T) {
			text := readRepoText(t, index)
			dir := strings.TrimSuffix(index, ".md")
			prefix := filepath.Base(dir) + "/"
			seen := map[string]bool{}
			for _, match := range flowSectionLink.FindAllStringSubmatch(text, -1) {
				if !strings.HasPrefix(match[1], prefix) {
					t.Errorf("%s links a different section directory: %s", index, match[1])
					continue
				}
				path := filepath.ToSlash(filepath.Join(filepath.Dir(index), match[1]))
				if seen[path] {
					t.Errorf("%s is indexed more than once", path)
				}
				seen[path] = true
				info, err := os.Stat(filepath.Join(repoRoot(), path))
				if err != nil || info.IsDir() {
					t.Errorf("index entry %s is not a section file: %v", path, err)
				}
			}
			if len(seen) == 0 {
				t.Errorf("%s has no section entries", index)
			}
			files, err := os.ReadDir(filepath.Join(repoRoot(), dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".md") {
					continue
				}
				path := dir + "/" + file.Name()
				if !seen[path] {
					t.Errorf("section file %s is not indexed", path)
				}
			}
		})
	}
}
