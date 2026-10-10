// Package spawn contains the skill mention library ported from CXC v0.2.40.
// Callers supply the skills directory; this package registers no hook.
package spawn

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const spawnNormalizeMaxLength = 256 * 1024 // JavaScript UTF-16 code units

type spawnNormalizeFence struct {
	prefix string
	marker byte
	run    int
	rest   string
}

type spawnNormalizeLink struct{ folder, target, trailing string }

// NormalizeSkillMentions is CXC's conservative line-based scanner, with CRW
// skill names and full crw-X folder names. It rewrites known explicit mentions
// only on unambiguous lines, protecting fences, backticks and mixed brackets.
// The input size guard counts UTF-16 units; the result is never truncated.
func NormalizeSkillMentions(message, skillsDir string) string {
	if skillsDir == "" || spawnNormalizeOverLimit(message) {
		return message
	}
	lines := text.SplitLinesByteExact(message)
	var fence spawnNormalizeFence
	fenced := false
	for i, line := range lines {
		delimiter, found := spawnNormalizeFenceDelimiter(line)
		if fenced {
			if found && delimiter.prefix == fence.prefix && delimiter.marker == fence.marker && delimiter.run >= fence.run && spawnNormalizeFenceTail(delimiter.rest) {
				fenced = false
			}
			continue
		}
		if found {
			fence, fenced = delimiter, true
			continue
		}
		if strings.Contains(line, "`") {
			continue
		}
		prefix := spawnNormalizeAfterContainerPrefix(line, 8)
		body := line[prefix:]
		if link, ok := spawnNormalizeStandaloneLink(body); ok {
			lines[i] = line[:prefix] + spawnNormalizeRepairedStandaloneLink(body, link, skillsDir)
			continue
		}
		if strings.ContainsAny(line, "[]") {
			continue
		}
		lines[i] = spawnNormalizeBareLine(line, skillsDir)
	}
	return strings.Join(lines, "\n")
}

func spawnNormalizeOverLimit(message string) bool {
	units := 0
	for i := 0; i < len(message); {
		r, size := pyjson.CodePoint(message, i)
		i += size
		units++
		if r > 0xffff {
			units++
		}
		if units > spawnNormalizeMaxLength {
			return true
		}
	}
	return false
}

// A negative budget is uncapped for fence detection. Each space, quote or
// list marker consumes a token; the following space is a separate token.
func spawnNormalizeAfterContainerPrefix(line string, maxTokens int) int {
	i := 0
	for tokens := 0; (maxTokens < 0 || tokens < maxTokens) && i < len(line); tokens++ {
		ch := line[i]
		if ch == ' ' || ch == '>' {
			i++
			continue
		}
		if (ch == '-' || ch == '*' || ch == '+') && i+1 < len(line) && line[i+1] == ' ' {
			i++
			continue
		}
		if ch >= '0' && ch <= '9' {
			d := i
			for d < len(line) && d-i < 9 && line[d] >= '0' && line[d] <= '9' {
				d++
			}
			if d+1 < len(line) && (line[d] == '.' || line[d] == ')') && line[d+1] == ' ' {
				i = d + 1
				continue
			}
		}
		break
	}
	return i
}

func spawnNormalizeFenceDelimiter(line string) (spawnNormalizeFence, bool) {
	start := spawnNormalizeAfterContainerPrefix(line, -1)
	if start == len(line) || (line[start] != '`' && line[start] != '~') {
		return spawnNormalizeFence{}, false
	}
	end := start + 1
	for end < len(line) && line[end] == line[start] {
		end++
	}
	if end-start < 3 {
		return spawnNormalizeFence{}, false
	}
	return spawnNormalizeFence{line[:start], line[start], end - start, line[end:]}, true
}

func spawnNormalizeFenceTail(rest string) bool {
	return strings.Trim(strings.TrimSuffix(rest, "\r"), " ") == ""
}

// These two helpers are shared with the later inlining files in this package. A skill is there when its SKILL.md is a regular file
// whose real path stays inside the skills directory (CRW-1114; the oracle's existsSync took a directory named SKILL.md, and a
// folder that links out of the skills directory).
func spawnNormalizeSkillPath(skillsDir, folder string) (string, bool) {
	path, err := filepath.Abs(filepath.Join(skillsDir, folder, "SKILL.md"))
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return path, false
	}
	return path, spawnNormalizeContained(skillsDir, path)
}

// spawnNormalizeContained reports whether path, links resolved, lies inside skillsDir, links resolved.
func spawnNormalizeContained(skillsDir, path string) bool {
	// Both sides are absolute before their links are resolved: a relative root resolves to a relative path, which no absolute skill
	// file lies "inside" (CRW-1114).
	abs, err := filepath.Abs(skillsDir)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// spawnNormalizeCanonicalMention is the load link of a skill, or the plugin mention when the skills directory cannot stand as a raw
// Markdown link target: a parenthesis or white space (the oracle's rule), and an angle bracket, a quote, a backtick, a backslash or
// a control character (CRW-1114; the oracle wrote those raw).
func spawnNormalizeCanonicalMention(skillsDir, folder, path string) string {
	for _, r := range skillsDir {
		if r == '(' || r == ')' || spawnNormalizeJSSpace(r) || strings.ContainsRune("<>\"'`\\", r) || r < 0x20 || r == 0x7f {
			return "$crw:" + folder
		}
	}
	return "[$" + folder + "](skill://" + path + ")"
}

func spawnNormalizeJSSpace(r rune) bool { return text.Trim(string(r)) == "" }

// The oracle's standalone link expression is scanned explicitly: Go regexp's
// whitespace class differs from JS, and no regexp is initialized at startup.
func spawnNormalizeStandaloneLink(body string) (spawnNormalizeLink, bool) {
	for _, prefix := range []string{"[$crw:crw-", "[$crw-"} {
		if !strings.HasPrefix(body, prefix) {
			continue
		}
		start, end := len(prefix), len(prefix)
		for end < len(body) && spawnNormalizeFolderByte(body[end]) {
			end++
		}
		if end == start || !strings.HasPrefix(body[end:], "](") {
			return spawnNormalizeLink{}, false
		}
		targetStart := end + 2
		end = targetStart
		for end < len(body) && body[end] != ')' {
			r, size := pyjson.CodePoint(body, end)
			if spawnNormalizeJSSpace(r) || strings.ContainsRune("(\"'\\<>`", r) {
				return spawnNormalizeLink{}, false
			}
			end += size
		}
		if end == len(body) {
			return spawnNormalizeLink{}, false
		}
		trailing := body[end+1:]
		if strings.Trim(strings.TrimSuffix(trailing, "\r"), " \t") != "" {
			return spawnNormalizeLink{}, false
		}
		return spawnNormalizeLink{"crw-" + body[start:targetStart-2], body[targetStart:end], trailing}, true
	}
	return spawnNormalizeLink{}, false
}

func spawnNormalizeRepairedStandaloneLink(body string, link spawnNormalizeLink, skillsDir string) string {
	target := strings.TrimPrefix(link.target, "skill://")
	if filepath.Base(target) == "SKILL.md" {
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() { // a directory named SKILL.md is no skill (CRW-1114)
			return body
		}
	}
	if path, ok := spawnNormalizeSkillPath(skillsDir, link.folder); ok {
		return spawnNormalizeCanonicalMention(skillsDir, link.folder, path) + link.trailing
	}
	return body
}

func spawnNormalizeFolderByte(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-'
}

func spawnNormalizeMentionAt(line string, start int, skillsDir string) (end int, mention string, ok bool) {
	for _, prefix := range []string{"$crw:crw-", "$crw-"} {
		if !strings.HasPrefix(line[start:], prefix) {
			continue
		}
		folderStart := start + len(prefix)
		end = folderStart
		for end < len(line) && spawnNormalizeFolderByte(line[end]) {
			end++
		}
		if end == folderStart {
			return 0, "", false
		}
		if end < len(line) {
			ch := line[end]
			if spawnNormalizeFolderByte(ch) || ch >= 'A' && ch <= 'Z' || ch == '_' || ch == ':' {
				return 0, "", false
			}
		}
		folder := "crw-" + line[folderStart:end]
		if path, exists := spawnNormalizeSkillPath(skillsDir, folder); exists {
			return end, spawnNormalizeCanonicalMention(skillsDir, folder, path), true
		}
		return 0, "", false
	}
	return 0, "", false
}

func spawnNormalizeBareLine(line, skillsDir string) string {
	if !strings.Contains(line, "$") {
		return line
	}
	var out strings.Builder
	for i := 0; i < len(line); {
		// A mention starts a token: one escaped with a backslash or inside a word is text (CRW-1114; the oracle rewrote both).
		if line[i] == '$' && (i == 0 || !spawnNormalizeTokenByte(line[i-1])) {
			if end, mention, ok := spawnNormalizeMentionAt(line, i, skillsDir); ok {
				out.WriteString(mention)
				i = end
				continue
			}
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String()
}

// spawnNormalizeTokenByte is a byte a mention cannot follow: a backslash escape, an ASCII letter, digit or underscore, or a byte of
// a non-ASCII character.
func spawnNormalizeTokenByte(ch byte) bool {
	return ch == '\\' || ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch >= 0x80
}
