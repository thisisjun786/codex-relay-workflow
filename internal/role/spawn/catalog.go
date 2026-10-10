package spawn

import (
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// spawnCatalogReadMax bounds how much of a skill file the catalog reads: 1024 UTF-16 units are at most 4 KiB of UTF-8.
const spawnCatalogReadMax = 4096

// BuildLeafSkillCatalog returns the sorted leaf-safe skills with their descriptions. The metadata is read from the YAML
// frontmatter that opens the first 1024 UTF-16 units of SKILL.md only (CRW-1114; the oracle matched name: and description: lines
// anywhere in that head, and its pattern could take the next line), and only that prefix of the file is read. A skill without a
// frontmatter name is left out. Each entry names the skill's folder, the directory the self-load path names, not its display name.
// Read failures omit the skill.
func BuildLeafSkillCatalog(skillsDir string) string {
	root, err := os.OpenRoot(skillsDir)
	if err != nil {
		return ""
	}
	defer root.Close()
	folders, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return ""
	}
	var entries []string
	for _, folder := range folders {
		if !folder.IsDir() || !slices.Contains(LeafSafeSkillFolders(), folder.Name()) {
			continue
		}
		body, ok := spawnInlineReadSkillPrefix(root, skillsDir, folder.Name(), spawnCatalogReadMax)
		if !ok {
			continue
		}
		name, desc, ok := spawnCatalogFrontmatter(spawnInlineUTF16Prefix(body, 1024))
		if !ok || name == "" {
			continue
		}
		entries = append(entries, "- "+folder.Name()+": "+spawnInlineUTF16Prefix(desc, 120))
	}
	if len(entries) == 0 {
		return ""
	}
	return "Available skills (self-load from " + skillsDir + "/<name>/SKILL.md):\n" + strings.Join(entries, "\n")
}

// spawnCatalogFrontmatter reads the frontmatter that opens head: a first line "---" and the lines up to the next "---" line. The
// name and description are the values of their own lines (white space after the colon is spaces and tabs, so a value never comes
// from the next line), trimmed, and the description loses one pair of surrounding double quotes. ok is false without a closed
// frontmatter.
func spawnCatalogFrontmatter(head string) (name, desc string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return "", "", false
	}
	for _, line := range lines[1:] {
		if strings.TrimRight(line, " \t") == "---" {
			return name, desc, true
		}
		if value, found := strings.CutPrefix(line, "name:"); found && name == "" {
			name = text.Trim(value)
		} else if value, found := strings.CutPrefix(line, "description:"); found && desc == "" {
			desc = text.Trim(value)
			if len(desc) >= 2 && desc[0] == '"' && desc[len(desc)-1] == '"' {
				desc = text.Trim(desc[1 : len(desc)-1])
			}
		}
	}
	return "", "", false
}
