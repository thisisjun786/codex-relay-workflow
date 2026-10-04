package spawn

import (
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const InlineSkillOpen = "<skill name=\"crw-"
const InlineSkillClose = "</skill>"

const spawnInlineJSSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

// LeafSafeSkillFolders returns the explicit oracle allowlist with CRW names.
// A new dev-* folder is invisible until explicitly added; callers own the slice.
func LeafSafeSkillFolders() []string {
	return []string{"crw-ast-grep", "crw-dev", "crw-dev-architecture", "crw-dev-backend", "crw-dev-code-reviewer", "crw-dev-data", "crw-dev-debugging", "crw-dev-devops", "crw-dev-visualizer", "crw-dev-frontend", "crw-dev-scaffolding", "crw-dev-security", "crw-dev-testing", "crw-dev-uiux-design", "crw-kwrite", "crw-qa", "crw-remote", "crw-repo-map", "crw-search", "crw-lunasearch"}
}

// MentionedFolders recognizes bare, plugin-native and skill:// link mentions.
// It intentionally scans prose/fences without normalization's line protections.
func MentionedFolders(message string) map[string]bool {
	out := map[string]bool{}
	bare := regexp.MustCompile(`\$(?:[Cc][Rr][Ww]:)?[Cc][Rr][Ww]-([a-zA-Z0-9-]+)`)
	for _, m := range bare.FindAllStringSubmatch(message, -1) {
		out["crw-"+strings.ToLower(m[1])] = true
	}
	space := strings.TrimSuffix(strings.TrimPrefix(spawnInlineJSSpace, "["), "]")
	link := regexp.MustCompile(`[Ss][Kk][Ii][Ll][Ll]://[^` + space + `]*?/([^/` + space + `)]+)/[Ss][Kk][Ii][Ll][Ll]\.[Mm][Dd]`)
	for _, m := range link.FindAllStringSubmatch(message, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// InlineSkillBodies appends all selected bodies or none. All cap comparisons
// count JavaScript UTF-16 units; the original message is never truncated.
func InlineSkillBodies(message, skillsDir string) string {
	if skillsDir == "" || spawnNormalizeOverLimit(message) {
		return message
	}
	blocks := SkillBlocks([]string{message}, skillsDir)
	if len(blocks) == 0 {
		return message
	}
	candidate := message + "\n\n" + strings.Join(blocks, "\n\n")
	if spawnNormalizeOverLimit(candidate) {
		return message
	}
	return candidate
}

// SkillBlocks keeps item boundaries and deduplicates globally. Its cap is on
// aggregate input only, like the oracle collector; callers cap the final output.
func SkillBlocks(texts []string, skillsDir string) []string {
	units := max(0, len(texts)-1) * 2
	for _, s := range texts {
		units += spawnInlineUTF16Units(s)
		if units > spawnNormalizeMaxLength {
			return nil
		}
	}
	root, err := os.OpenRoot(skillsDir)
	if err != nil {
		return nil
	}
	defer root.Close()
	closed, folders := map[string]bool{}, map[string]bool{}
	for _, s := range texts {
		seen, source := spawnInlineScanBlocks(s)
		for folder := range seen {
			closed[folder] = true
		}
		for folder := range MentionedFolders(source) {
			folders[folder] = true
		}
	}
	var ordered []string
	for folder := range folders {
		ordered = append(ordered, folder)
	}
	slices.Sort(ordered)
	var blocks []string
	for _, folder := range ordered {
		if !slices.Contains(LeafSafeSkillFolders(), folder) || closed[folder] {
			continue
		}
		body, ok := spawnInlineReadSkill(root, skillsDir, folder)
		if !ok {
			continue
		}
		body = text.Trim(body)
		if body != "" {
			blocks = append(blocks, "<skill name=\""+folder+"\">\n"+body+"\n"+InlineSkillClose)
		}
	}
	return blocks
}

func spawnInlinePositions(message, token string) []int {
	var out []int
	for from := 0; from < len(message); {
		p := strings.Index(message[from:], token)
		if p < 0 {
			break
		}
		p += from
		out = append(out, p)
		from = p + 1
	}
	return out
}

// Delimiter pointers only advance. Nested openers, including malformed ones
// inside a valid block, count toward depth exactly as in the oracle.
func spawnInlineScanBlocks(message string) (map[string]bool, string) {
	closed := map[string]bool{}
	var parts strings.Builder
	opens, closes := spawnInlinePositions(message, InlineSkillOpen), spawnInlinePositions(message, InlineSkillClose)
	oi, ci := 0, 0
	for i := 0; i < len(message); {
		for oi < len(opens) && opens[oi] < i {
			oi++
		}
		if oi >= len(opens) {
			parts.WriteString(message[i:])
			break
		}
		open := opens[oi]
		nameStart := open + len(InlineSkillOpen)
		j := nameStart
		for j < len(message) && spawnNormalizeFolderByte(message[j]) {
			j++
		}
		if j == nameStart || !strings.HasPrefix(message[j:], "\">") {
			parts.WriteString(message[i:nameStart])
			i = nameStart
			oi++
			continue
		}
		folder := "crw-" + message[nameStart:j]
		depth, blockEnd, wo, k := 1, -1, oi+1, j+2
		for ci < len(closes) || wo < len(opens) {
			for ci < len(closes) && closes[ci] < k {
				ci++
			}
			if ci >= len(closes) {
				break
			}
			for wo < len(opens) && opens[wo] < k {
				wo++
			}
			if wo < len(opens) && opens[wo] < closes[ci] {
				depth++
				k = opens[wo] + len(InlineSkillOpen)
				wo++
			} else {
				depth--
				k = closes[ci] + len(InlineSkillClose)
				ci++
				if depth == 0 {
					blockEnd = k
					break
				}
			}
		}
		if blockEnd < 0 {
			parts.WriteString(message[i:])
			break
		}
		closed[folder] = true
		parts.WriteString(message[i:open])
		i = blockEnd
		oi = wo
	}
	return closed, parts.String()
}

// Normalization keeps its existence semantics. Reads add a rooted boundary:
// no linked folder/file, nonregular file or changed inode. The caller chooses
// the root; hardlinks and privileged mounts are outside this boundary.
func spawnInlineReadSkill(root *os.Root, skillsDir, folder string) (string, bool) {
	if _, ok := spawnNormalizeSkillPath(skillsDir, folder); !ok {
		return "", false
	}
	dirInfo, err := root.Lstat(folder)
	if err != nil || !dirInfo.IsDir() {
		return "", false
	}
	dir, err := root.OpenRoot(folder)
	if err != nil {
		return "", false
	}
	defer dir.Close()
	actual, err := dir.Stat(".")
	if err != nil || !os.SameFile(dirInfo, actual) {
		return "", false
	}
	info, err := dir.Lstat("SKILL.md")
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := dir.Open("SKILL.md")
	if err != nil {
		return "", false
	}
	defer f.Close()
	actual, err = f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		return "", false
	}
	checked, err := dir.Lstat("SKILL.md")
	if err != nil || !checked.Mode().IsRegular() || !os.SameFile(info, checked) {
		return "", false
	}
	checked, err = root.Lstat(folder)
	if err != nil || !checked.IsDir() || !os.SameFile(dirInfo, checked) {
		return "", false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	return spawnInlineDecodeUTF8(data), true
}

func spawnInlineUTF16Units(s string) int {
	units := 0
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		i += n
		units++
		if r > 0xffff {
			units++
		}
	}
	return units
}

// JS slice can stop in a surrogate pair. Keep that high surrogate as WTF-8,
// the same lone-surrogate representation pyjson.CodePoint already understands.
func spawnInlineUTF16Prefix(s string, limit int) string {
	units := 0
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			if width == 2 && units < limit {
				high := 0xd800 + (r-0x10000)/0x400
				return s[:i] + string([]byte{byte(0xe0 | high>>12), byte(0x80 | (high>>6)&0x3f), byte(0x80 | high&0x3f)})
			}
			return s[:i]
		}
		units += width
		i += n
	}
	return s
}

// Node replaces maximal ill-formed UTF-8 subsequences; ToValidUTF8 would merge
// consecutive errors, while a per-byte replacement would split valid prefixes.
func spawnInlineDecodeUTF8(data []byte) string {
	var out strings.Builder
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || n > 1 {
			out.Write(data[i : i+n])
			i += n
			continue
		}
		width := 1
		lead := data[i]
		if lead >= 0xc2 && lead <= 0xdf {
			width = 2
		} else if lead >= 0xe0 && lead <= 0xef {
			width = 3
		} else if lead >= 0xf0 && lead <= 0xf4 {
			width = 4
		}
		taken := 1
		for taken < width && i+taken < len(data) {
			b := data[i+taken]
			if b < 0x80 || b > 0xbf {
				break
			}
			if taken == 1 && (lead == 0xe0 && b < 0xa0 || lead == 0xed && b > 0x9f || lead == 0xf0 && b < 0x90 || lead == 0xf4 && b > 0x8f) {
				break
			}
			taken++
		}
		out.WriteRune(utf8.RuneError)
		i += taken
	}
	return out.String()
}
