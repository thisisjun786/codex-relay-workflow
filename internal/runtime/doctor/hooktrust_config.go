// Hook trust config.toml reading, ported from CXC v0.2.40 hook-trust.ts:59-62 (HookTrustResult)
// and :217-343 (updateMultilineState, isEscaped, tomlLines, sections, escapeRegExp,
// readInstalledPluginKeys, trustedHashLines, exactHookSections, diagnoseHookTrust), commit
// 3c1459acadeb1906d97c00a598e1457327ae372d. DiagnoseHookTrust answers what Codex's config.toml
// records for each hook of a plugin; ReadInstalledPluginKeys answers which install keys of one
// plugin name the config holds. Neither reads the plugin's hook documents; that listing is
// ListHookTrustEntries (hooktrust_entries.go).
//
// The reader is the oracle's own hand-written TOML line reader, not a TOML library: an LF ends a
// line and a CR before it is dropped from the line's text while its offsets stay in the document;
// a line that starts outside a basic (""") or literal (”') multiline string is structural; a
// structural line may be a section header, and inside such a section a structural line may carry
// a trusted_hash or an enabled assignment, each with only spaces, tabs and an optional trailing
// comment around the tokens. Nothing here writes; the retrust write is ported later in this
// series and reuses these line/section/hash readers.
//
// The offsets the readers answer are byte offsets into the document. The oracle's are UTF-16
// code-unit offsets, and each side only ever slices its own string with them, so the answers are
// the same where the encodings differ; a later Go caller keeps its arithmetic in bytes.
//
// The behaviour is ported as-is. What the reader keeps from the oracle where it could be
// surprising is listed in docs/port-cxc/known-defects.md (section: the config.toml hook trust
// reader port).
package doctor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HookTrustResult is HookTrustResult (hook-trust.ts:59-62): one listed hook (Key, Hash,
// FileSha256, hooktrust_entries.go) with the trust record the config.toml holds for it. Status is
// "trusted", "drifted" or "untrusted": Actual is the value of the single structural trusted_hash
// line of the single section that reads exactly [hooks.state."<key>"] and is nil when the config
// holds no such pair (a missing config.toml leaves it nil for every hook); the hook is trusted when
// Actual equals Hash, drifted when it holds another value, and untrusted when it is nil.
type HookTrustResult struct {
	HookTrustEntry
	Status string
	Actual *string
}

// hookTrustTomlMultiline is the multiline-string state at a line start (hook-trust.ts:217-258):
// outside any string, inside a basic (""") or inside a literal (”') multiline string.
type hookTrustTomlMultiline int

const (
	hookTrustTomlOutside hookTrustTomlMultiline = iota
	hookTrustTomlBasic
	hookTrustTomlLiteral
)

// hookTrustTomlLine is TomlLine (hook-trust.ts:77-82): the text of one LF-terminated line with the
// CR before the LF dropped from the text, the offsets of the whole line (the CR included) in the
// document, and whether the line starts outside a multiline string.
type hookTrustTomlLine struct {
	Text       string
	Start      int
	End        int
	Structural bool
}

// hookTrustTomlSection is TomlSection (hook-trust.ts:70-75): one structural section-header line,
// its header including the brackets, and the body from the end of that line to the start of the
// next header or the end of the document.
type hookTrustTomlSection struct {
	Header    string
	Start     int
	BodyStart int
	End       int
}

// hookTrustTomlTrustedHash is TrustedHashLine (hook-trust.ts:84-88): one structural trusted_hash
// assignment, the value inside its quotes and the start offset and text of its line inside the
// body that was read.
type hookTrustTomlTrustedHash struct {
	Value string
	Start int
	Text  string
}

// hookTrustConfigCommentTail is the tail of three patterns (the section header, enabled and
// trusted_hash; the plugins header has none): spaces and tabs and an optional trailing comment up
// to the end of the line. The oracle writes it as (?:#.*)?, and JavaScript's
// "." matches neither LF nor CR nor U+2028 nor U+2029, while Go's "." excludes LF alone, so the
// class names all four; a comment that holds one of them is no comment and the line does not
// match (recorded as the comment_* cases of testdata/hooktrust/config-oracle.json).
const hookTrustConfigCommentTail = `[ \t]*(?:#[^\n\r\x{2028}\x{2029}]*)?$`

// hookTrustTomlLines is tomlLines (hook-trust.ts:266-281). A line ends at the next LF; the CR
// before it is dropped from the text but keeps its place in the offsets. The line is structural
// when it starts outside a multiline string; its text then updates the state for the next line.
func hookTrustTomlLines(content string) []hookTrustTomlLine {
	lines := []hookTrustTomlLine{}
	state := hookTrustTomlOutside
	for start := 0; start < len(content); {
		end := len(content)
		rawEnd := end
		if newline := strings.IndexByte(content[start:], '\n'); newline != -1 {
			end = start + newline + 1
			rawEnd = start + newline
			if newline > 0 && content[end-2] == '\r' {
				rawEnd--
			}
		}
		text := content[start:rawEnd]
		lines = append(lines, hookTrustTomlLine{Text: text, Start: start, End: end, Structural: state == hookTrustTomlOutside})
		state = hookTrustTomlUpdateMultilineState(text, state)
		start = end
	}
	return lines
}

// hookTrustTomlUpdateMultilineState is updateMultilineState (hook-trust.ts:217-256): the
// multiline state after one line, scanned in the oracle's order. A basic multiline closes on a
// """ it does not escape, a literal one closes on the first ”', an inline basic string swallows
// the character after each backslash, an inline literal string ends at its quote, and a # read
// outside a string comments out the rest of the line.
func hookTrustTomlUpdateMultilineState(line string, initial hookTrustTomlMultiline) hookTrustTomlMultiline {
	state := initial
	inline := hookTrustTomlOutside
	for index := 0; index < len(line); index++ {
		switch {
		case state == hookTrustTomlBasic:
			if strings.HasPrefix(line[index:], `"""`) && !hookTrustTomlIsEscaped(line, index) {
				state = hookTrustTomlOutside
				index += 2
			}
			continue
		case state == hookTrustTomlLiteral:
			if strings.HasPrefix(line[index:], `'''`) {
				state = hookTrustTomlOutside
				index += 2
			}
			continue
		case inline == hookTrustTomlBasic:
			if line[index] == '\\' {
				index++
			} else if line[index] == '"' {
				inline = hookTrustTomlOutside
			}
			continue
		case inline == hookTrustTomlLiteral:
			if line[index] == '\'' {
				inline = hookTrustTomlOutside
			}
			continue
		case line[index] == '#':
			return state
		case strings.HasPrefix(line[index:], `"""`):
			state = hookTrustTomlBasic
			index += 2
		case strings.HasPrefix(line[index:], `'''`):
			state = hookTrustTomlLiteral
			index += 2
		case line[index] == '"':
			inline = hookTrustTomlBasic
		case line[index] == '\'':
			inline = hookTrustTomlLiteral
		}
	}
	return state
}

// hookTrustTomlIsEscaped is isEscaped (hook-trust.ts:260-264): an odd number of backslashes
// directly before index escapes what index starts.
func hookTrustTomlIsEscaped(line string, index int) bool {
	slashes := 0
	for cursor := index - 1; cursor >= 0 && line[cursor] == '\\'; cursor-- {
		slashes++
	}
	return slashes%2 == 1
}

// hookTrustTomlSectionHeader is the header line of sections (hook-trust.ts:283-293): a line that
// is only spaces and tabs, one bracketed header, and an optional trailing comment, answers the
// header with its brackets.
func hookTrustTomlSectionHeader(text string) (string, bool) {
	// The pattern is a fixed literal, compiled where it is used so the package holds no
	// initializer that works at program start.
	match := regexp.MustCompile(`^[ \t]*(\[[^\r\n]+\])` + hookTrustConfigCommentTail).FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// hookTrustTomlSections is sections (hook-trust.ts:283-293): every structural section-header line
// of the document, each with the body that ends where the next header begins.
func hookTrustTomlSections(content string) []hookTrustTomlSection {
	var sections []hookTrustTomlSection
	for _, line := range hookTrustTomlLines(content) {
		if !line.Structural {
			continue
		}
		header, ok := hookTrustTomlSectionHeader(line.Text)
		if !ok {
			continue
		}
		sections = append(sections, hookTrustTomlSection{Header: header, Start: line.Start, BodyStart: line.End})
	}
	for index := range sections {
		if index+1 < len(sections) {
			sections[index].End = sections[index+1].Start
		} else {
			sections[index].End = len(content)
		}
	}
	return sections
}

// hookTrustConfigEscapeRegExp is escapeRegExp (hook-trust.ts:295-297): every character JavaScript
// gives a special meaning in a regular expression gets a backslash, so the plugin name is spliced
// into the header pattern as a literal.
func hookTrustConfigEscapeRegExp(value string) string {
	const special = `.*+?^${}()|[]\`
	var builder strings.Builder
	for _, character := range value {
		if strings.ContainsRune(special, character) {
			builder.WriteByte('\\')
		}
		builder.WriteRune(character)
	}
	return builder.String()
}

// ReadInstalledPluginKeys is readInstalledPluginKeys (hook-trust.ts:299-315): the install keys of
// the sections whose header reads exactly [plugins."<pluginName>@<anything but a quote>"], in
// document order, each only when its body holds no structural enabled = false line (spaces, tabs
// and a trailing comment allowed). A config.toml that does not exist answers no key.
func ReadInstalledPluginKeys(codexHome, pluginName string) ([]string, error) {
	content, present, err := hookTrustConfigReadConfig(codexHome)
	if err != nil {
		return nil, err
	}
	if !present {
		return []string{}, nil
	}
	pattern := regexp.MustCompile(`^\[plugins\."(` + hookTrustConfigEscapeRegExp(pluginName) + `@[^"\r\n]+)"\]$`)
	disabled := regexp.MustCompile(`^[ \t]*enabled[ \t]*=[ \t]*false` + hookTrustConfigCommentTail)
	found := []string{}
	for _, section := range hookTrustTomlSections(content) {
		match := pattern.FindStringSubmatch(section.Header)
		if match == nil {
			continue
		}
		body := content[section.BodyStart:section.End]
		off := false
		for _, line := range hookTrustTomlLines(body) {
			if line.Structural && disabled.MatchString(line.Text) {
				off = true
				break
			}
		}
		if !off {
			found = append(found, match[1])
		}
	}
	return found, nil
}

// hookTrustTomlTrustedHashLines is trustedHashLines (hook-trust.ts:317-323): every structural
// trusted_hash = "..." line of a body, in order, with its quoted value (which may be empty).
func hookTrustTomlTrustedHashLines(body string) []hookTrustTomlTrustedHash {
	hashes := []hookTrustTomlTrustedHash{}
	pattern := regexp.MustCompile(`^[ \t]*trusted_hash[ \t]*=[ \t]*"([^"]*)"` + hookTrustConfigCommentTail)
	for _, line := range hookTrustTomlLines(body) {
		if !line.Structural {
			continue
		}
		match := pattern.FindStringSubmatch(line.Text)
		if match == nil {
			continue
		}
		hashes = append(hashes, hookTrustTomlTrustedHash{Value: match[1], Start: line.Start, Text: line.Text})
	}
	return hashes
}

// hookTrustTomlExactHookSections is exactHookSections (hook-trust.ts:325-328): the sections whose
// header is exactly [hooks.state."<key>"], a trailing comment left out by the header reader.
func hookTrustTomlExactHookSections(content, key string) []hookTrustTomlSection {
	header := `[hooks.state."` + key + `"]`
	exact := []hookTrustTomlSection{}
	for _, section := range hookTrustTomlSections(content) {
		if section.Header == header {
			exact = append(exact, section)
		}
	}
	return exact
}

// DiagnoseHookTrust is diagnoseHookTrust (hook-trust.ts:330-343): one answer per hook the plugin
// declares, in listing order. A hook's Actual is the value of the single structural trusted_hash
// line when the document holds exactly one exact [hooks.state."<key>"] section and that section
// holds exactly one such line, and nil otherwise; a config.toml that does not exist reads as an
// empty document, so every hook is untrusted. The listing's own error is returned as it is.
func DiagnoseHookTrust(codexHome, pluginRoot, pluginKey string) ([]HookTrustResult, error) {
	content, present, err := hookTrustConfigReadConfig(codexHome)
	if err != nil {
		return nil, err
	}
	if !present {
		content = ""
	}
	entries, err := ListHookTrustEntries(pluginRoot, pluginKey)
	if err != nil {
		return nil, err
	}
	results := make([]HookTrustResult, 0, len(entries))
	for _, entry := range entries {
		sections := hookTrustTomlExactHookSections(content, entry.Key)
		var hashes []hookTrustTomlTrustedHash
		for _, section := range sections {
			hashes = append(hashes, hookTrustTomlTrustedHashLines(content[section.BodyStart:section.End])...)
		}
		result := HookTrustResult{HookTrustEntry: entry, Status: "untrusted"}
		if len(sections) == 1 && len(hashes) == 1 {
			actual := hashes[0].Value
			result.Actual = &actual
			result.Status = "drifted"
			if actual == entry.Hash {
				result.Status = "trusted"
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// hookTrustConfigReadConfig is the config.toml read the two entry points share (hook-trust.ts:300
// and :331): whether the file exists, and its bytes as Buffer.toString("utf8") holds them (one
// U+FFFD for each maximal invalid subpart). A path that cannot be looked at reads as absent, the
// oracle's existsSync; once it is known to exist a read that fails is an engine error.
func hookTrustConfigReadConfig(codexHome string) (string, bool, error) {
	path := filepath.Join(codexHome, "config.toml")
	if _, err := os.Stat(path); err != nil {
		return "", false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	return hookTrustEntriesUTF8(raw), true, nil
}
