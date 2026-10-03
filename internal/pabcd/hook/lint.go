// Package hook holds the PABCD hook behaviours ported from CXC v0.2.40 (3c1459ac).
package hook

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// JS whitespace and dot, as used by comment-lint.ts's non-Unicode regexes.
const lintSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
const lintDot = `[^\n\r\x{2028}\x{2029}]`
const nativeFileDirective = `^\*\*\*` + lintSpace + `+(?:Add|Update|Delete)` + lintSpace + `+File:` + lintSpace + `*(` + lintDot + `+?)` + lintSpace + `*$`
const nativeMoveDirective = `^\*\*\*` + lintSpace + `+Move` + lintSpace + `+to:` + lintSpace + `*(` + lintDot + `+?)` + lintSpace + `*$`
const unifiedFileHeader = `^\+\+\+` + lintSpace + `+(?:b/)?(` + lintDot + `+?)` + lintSpace + `*$`
const lintPreviewUnits = 120

// ForbiddenPattern is a static added-line match and its explanation.
type ForbiddenPattern struct {
	Re  *regexp.Regexp
	Msg string
}

// ForbiddenPatterns constructs the fixed patterns at call time, never at package load.
func ForbiddenPatterns() []ForbiddenPattern {
	return []ForbiddenPattern{
		{regexp.MustCompile(`\bas any\b`), "`as any` cast — use a precise type or add a trailing `// justified: <reason>` to opt out"},        // justified: ported pattern/message
		{regexp.MustCompile(`\b(eval)` + lintSpace + `*\(`), "`eval(` — dynamic eval is forbidden; refactor or add `// justified: <reason>`"}, // justified: ported pattern/message
		{regexp.MustCompile(`\bdebugger\b`), "`debugger` statement — remove before committing or add `// justified: <reason>`"},               // justified: ported pattern/message
	}
}

func isJustified(line string) bool {
	// JS /i without /u does not fold non-ASCII long-s to ASCII s.
	return regexp.MustCompile(`(?://|#)` + lintSpace + `*[jJ][uU][sS][tT][iI][fF][iI][eE][dD]:`).MatchString(line)
}

func isProseTarget(target *string) bool {
	if target == nil {
		return false
	}
	i := strings.LastIndex(*target, ".")
	if i < 0 {
		return false
	}
	switch strings.ToLower((*target)[i:]) {
	case ".md", ".markdown", ".mdx", ".txt", ".rst", ".adoc":
		return true
	}
	return false
}

// AddedRecord retains the target of an added line; nil means it is unknown.
type AddedRecord struct {
	Line   string
	Target *string
}

// AddedRecords follows native directives and unified headers in patch order.
func AddedRecords(patch string) []AddedRecord {
	out := make([]AddedRecord, 0)
	var target *string
	native, move, unified := regexp.MustCompile(nativeFileDirective), regexp.MustCompile(nativeMoveDirective), regexp.MustCompile(unifiedFileHeader)
	for _, raw := range text.SplitLines(patch) {
		match := native.FindStringSubmatch(raw)
		if match == nil {
			match = move.FindStringSubmatch(raw)
		}
		if match != nil {
			target = &match[1]
			continue
		}
		if strings.HasPrefix(raw, "+++") {
			target = nil
			if match = unified.FindStringSubmatch(raw); match != nil && match[1] != "/dev/null" {
				target = &match[1]
			}
			continue
		}
		if strings.HasPrefix(raw, "+ +") {
			continue
		}
		if strings.HasPrefix(raw, "+") {
			out = append(out, AddedRecord{Line: raw[1:], Target: target})
		}
	}
	return out
}

// AddedLines is the target-free projection retained for consumers of addedLines.
func AddedLines(patch string) []string {
	out := make([]string, 0)
	for _, record := range AddedRecords(patch) {
		out = append(out, record.Line)
	}
	return out
}

// LintResult is either allowed, or the first static finding.
type LintResult struct {
	OK     bool
	Reason string
}

// LintApplyPatch scans only added source lines of a string command, failing open on other shapes.
func LintApplyPatch(command any) LintResult {
	patch, ok := command.(string)
	if !ok || patch == "" {
		return LintResult{OK: true}
	}
	patterns := ForbiddenPatterns()
	for _, record := range AddedRecords(patch) {
		if isProseTarget(record.Target) || isJustified(record.Line) {
			continue
		}
		for _, p := range patterns {
			if p.Re.MatchString(record.Line) {
				units := utf16.Encode([]rune(text.Trim(record.Line)))
				if len(units) > lintPreviewUnits {
					units = units[:lintPreviewUnits]
				}
				return LintResult{Reason: p.Msg + " (line: " + string(utf16.Decode(units)) + ")"}
			}
		}
	}
	return LintResult{OK: true}
}

func editTool(tool string) bool {
	switch tool {
	case "apply_patch", "Write", "Edit":
		return true
	}
	return false
}

// editObject is one JSON document; numbers unrelated to a consumed field may exceed float64.
func editObject(raw string) map[string]any {
	if !json.Valid([]byte(raw)) {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// editAnswer keeps the oracle's field order and JSON.stringify spelling.
func editAnswer(decision, reason, context string) string {
	type output struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		AdditionalContext        string `json:"additionalContext,omitempty"`
	}
	b, err := role.Stringify(struct {
		Output output `json:"hookSpecificOutput"`
	}{output{"PreToolUse", decision, reason, context}}, "")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// HandleApplyPatchLint is the fail-open PreToolUse lint; native Write/Edit fields stay unscanned.
func HandleApplyPatchLint(raw string) string {
	p := editObject(text.Trim(raw))
	tool, _ := p["tool_name"].(string)
	if p["hook_event_name"] != "PreToolUse" || !editTool(tool) {
		return ""
	}
	input, _ := p["tool_input"].(map[string]any)
	r := LintApplyPatch(input["command"])
	if r.OK {
		return ""
	}
	return editAnswer("deny", "[crw comment-lint] "+r.Reason, "")
}
