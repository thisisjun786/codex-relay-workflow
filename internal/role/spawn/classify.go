package spawn

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the CXC v0.2.40 spawn hook's pure classifier helpers
// (subagent-config/src/spawn-attach-hook.ts:279-280, 409-419, 450-595) with the CRW names of
// contract/schema/cxc/name-substitution.json. Nothing here registers, or is, a hook.

// SubspawnToken is the oracle's SUBSPAWN_TOKEN: the explicit per-dispatch recursion grant token.
const SubspawnToken = "CRW-SUBSPAWN-ALLOWED"

// SubspawnGrantPattern is the oracle's SUBSPAWN_GRANT_RE as a Go pattern string. The oracle's
// flags are /gi: the spelled ASCII classes keep exactly the case folding JavaScript uses (Go's
// (?i) would fold s with U+017F), and ReplaceAllString is global. The sibling grant issue
// compiles it inside its own function, so this package keeps no initializer work.
const SubspawnGrantPattern = `\[[Cc][Rr][Ww]-[Ss][Uu][Bb][Ss][Pp][Aa][Ww][Nn]-[Gg][Rr][Aa][Nn][Tt]:([a-fA-F0-9]{64})\]`

// RecurseDenyReason is the oracle's RECURSE_DENY_REASON with the rename table's R32.
const RecurseDenyReason = "crw LEAF-TOPOLOGY-01: sub-agents are leaf agents and may not spawn their own " +
	"sub-agents (multi_agent_v2 enforces no depth limit upstream, so recursion is denied " +
	"by dispatcher policy). Finish your own scope and report the need for delegation in " +
	"your final answer. A dispatcher can authorize recursion for a specific spawn by " +
	"including the recursion grant token in the spawn message."

// spawnClassifyReviewKeywords is the oracle's REVIEW_KEYWORDS. A fixed array is static data, not
// work at startup.
var spawnClassifyReviewKeywords = [...]string{
	"review",
	"audit",
	"verify",
	"verification",
	"red-team",
	"red team",
	"\uB9AC\uBDF0",
	"\uAC80\uC99D",
	"\uAC10\uC0AC",
	"\uAC80\uD1A0",
}

// StripControlMarkers is the oracle's stripControlMarkers: it removes every recursion token
// (case-sensitively, like String.prototype.replaceAll) and every grant marker, then, unless
// whitespace is preserved, collapses runs of three or more LF to two and trims with the
// JavaScript whitespace set through text.Trim.
func StripControlMarkers(message string, preserveWhitespace bool) string {
	stripped := strings.ReplaceAll(message, SubspawnToken, "")
	stripped = regexp.MustCompile(SubspawnGrantPattern).ReplaceAllString(stripped, "")
	if preserveWhitespace {
		return stripped
	}
	stripped = regexp.MustCompile("\n{3,}").ReplaceAllString(stripped, "\n\n")
	return text.Trim(stripped)
}

// DenyEnvelope is the oracle's denyEnvelope: the JSON.stringify bytes of the D1 deny envelope
// (hookSpecificOutput.permissionDecision deny, output_parser.rs:144) plus exactly one newline.
// spawnHookRouteStringify is JSON.stringify for the hook's answers: HTML is not escaped, U+2028 and
// U+2029 stay literal, and a lone surrogate (three WTF-8 bytes in a Go string) is written as its
// \udXXX escape, where encoding/json would write U+FFFD.
func DenyEnvelope(reason string) string {
	return spawnHookRouteStringify(pyjson.Object{{Key: "hookSpecificOutput", Value: pyjson.Object{
		{Key: "hookEventName", Value: "PreToolUse"},
		{Key: "permissionDecision", Value: "deny"},
		{Key: "permissionDecisionReason", Value: reason},
	}}}) + "\n"
}

// InferRole is the oracle's inferRole. The order is the oracle's own: explicit worker/executor,
// then explicit architect/reviewer, then the producer header's CRW-ROLE: line (which wins over an
// explorer agent type), then explorer, then the review-keyword fallback over the lowercased
// message, matched at a word start (spawnClassifyWordStart). Non-string agent types match nothing.
func InferRole(agentType any, message string) role.RoleName {
	if s, ok := agentType.(string); ok {
		switch s {
		case "worker", "executor":
			return role.Executor
		case "architect", "reviewer":
			return role.RoleName(s)
		}
	}
	header := message
	if taskStart := spawnClassifyTaskStart(message); taskStart >= 0 {
		header = message[:taskStart]
	}
	if marker := spawnClassifyRoleMarker(header); marker != "" {
		return role.RoleName(marker)
	}
	if s, ok := agentType.(string); ok && s == "explorer" {
		return role.Explorer
	}
	lower := spawnInlineLowerJS(message)
	for _, keyword := range spawnClassifyReviewKeywords {
		if spawnClassifyWordStart(lower, keyword) {
			return role.Reviewer
		}
	}
	return role.Explorer
}

// spawnClassifyWordStart reports whether keyword occurs in s at the start of a word (CRW-1114; the oracle's substring search took
// "preview" for a review): an ASCII keyword must not follow a letter, digit or underscore, while an inflection after it still counts
// ("reviewer", "verifying"). A Hangul keyword is matched anywhere, since Korean writes compounds without a space ("코드리뷰").
func spawnClassifyWordStart(s, keyword string) bool {
	for from := 0; from < len(s); {
		at := strings.Index(s[from:], keyword)
		if at < 0 {
			return false
		}
		at += from
		if keyword[0] >= 0x80 || at == 0 || !spawnClassifyWordByte(s[at-1]) {
			return true
		}
		from = at + 1
	}
	return false
}

func spawnClassifyWordByte(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch >= 0x80
}

// spawnClassifyTaskStart is the byte index of the first line-start "TASK:" in message, the cut
// JavaScript's /^TASK:/m search makes, or -1.
func spawnClassifyTaskStart(message string) int {
	for at := 0; at <= len(message); {
		offset := strings.Index(message[at:], "TASK:")
		if offset < 0 {
			return -1
		}
		index := at + offset
		if index == 0 || spawnClassifyLineBoundaryBefore(message, index) {
			return index
		}
		at = index + 1
	}
	return -1
}

// spawnClassifyLineBoundaryBefore reports whether the position before at is a JavaScript line
// terminator, so at is a multiline ^ position: LF, CR, U+2028 or U+2029.
func spawnClassifyLineBoundaryBefore(s string, at int) bool {
	if at <= 0 || at > len(s) {
		return false
	}
	switch s[at-1] {
	case '\n', '\r':
		return true
	}
	return at >= 3 && (s[at-3:at] == "\u2028" || s[at-3:at] == "\u2029")
}

// spawnClassifyRoleMarker returns the value of the first CRW-ROLE: line in the header whose
// subject is architect, reviewer or explorer and which ends after only spaces and tabs, the
// oracle's /^CRW-ROLE: (architect|reviewer|explorer)[ 	]*$/m match.
func spawnClassifyRoleMarker(header string) string {
	for at := 0; at < len(header); at++ {
		if at != 0 && !spawnClassifyLineBoundaryBefore(header, at) {
			continue
		}
		rest := header[at:]
		for _, candidate := range []string{"architect", "reviewer", "explorer"} {
			prefix := "CRW-ROLE: " + candidate
			if !strings.HasPrefix(rest, prefix) {
				continue
			}
			tail := rest[len(prefix):]
			end := 0
			for end < len(tail) && (tail[end] == ' ' || tail[end] == '\t') {
				end++
			}
			tail = tail[end:]
			if tail == "" || tail[0] == '\n' || tail[0] == '\r' ||
				strings.HasPrefix(tail, "\u2028") || strings.HasPrefix(tail, "\u2029") {
				return candidate
			}
		}
	}
	return ""
}

// IsV2SpawnInput is the oracle's isV2SpawnInput: a v2 payload carries task_name or fork_turns. A
// JSON null member still counts as present, and a nil map reads safely.
func IsV2SpawnInput(toolInput map[string]any) bool {
	_, hasTaskName := toolInput["task_name"]
	_, hasForkTurns := toolInput["fork_turns"]
	return hasTaskName || hasForkTurns
}

// IsFullHistoryFork is the oracle's isFullHistoryFork. fork_context === true wins outright. A
// JSON number fork_turns is off-schema and never a full fork; any other non-string value behaves
// as the empty string, which is the v2 default. A string is trimmed with the JavaScript
// whitespace set and is a full fork when empty or "all" case-insensitively.
func IsFullHistoryFork(toolInput map[string]any) bool {
	if forkContext, ok := toolInput["fork_context"].(bool); ok && forkContext {
		return true
	}
	if !IsV2SpawnInput(toolInput) {
		return false
	}
	raw := toolInput["fork_turns"]
	// JavaScript's typeof raw === "number": every Go numeric kind, so the caller's JSON
	// decoder (float64 by default, json.Number with UseNumber, an integer type in a
	// programmatic map) cannot change the fork classification.
	switch raw.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr, float32, float64, json.Number:
		return false
	}
	value := ""
	if s, ok := raw.(string); ok {
		value = text.Trim(s)
	}
	if value == "" {
		return true
	}
	return spawnInlineLowerJS(value) == "all"
}

// IsFernetTokenShape is the oracle's isFernetTokenShape: a structural shape check of a native
// Fernet envelope, never authentication. Padding must be one canonical trailing run, the core
// must be non-empty base64url, its strict decode must re-encode unchanged, and the result must
// be version 0x80, at least 73 bytes and 57 + 16n bytes long.
func IsFernetTokenShape(token string) bool {
	firstPad := strings.IndexByte(token, '=')
	core := token
	if firstPad != -1 {
		core = token[:firstPad]
		if strings.Trim(token[firstPad:], "=") != "" {
			return false
		}
	}
	rem := len(core) % 4
	if firstPad == -1 {
		if rem == 1 {
			return false
		}
	} else if rem < 2 || len(token)-len(core) != 4-rem {
		return false
	}
	if core == "" {
		return false
	}
	for i := 0; i < len(core); i++ {
		c := core[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil {
		return false
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != core {
		return false
	}
	if len(decoded) < 73 || (len(decoded)-57)%16 != 0 {
		return false
	}
	return decoded[0] == 0x80
}

// IsSpawnToolName is the oracle's isSpawnToolName: the hook-facing names across surfaces.
func IsSpawnToolName(name any) bool {
	s, ok := name.(string)
	return ok && spawnClassifySpawnToolName(s)
}

// IsCollaborationToolName is the oracle's isCollaborationToolName: the same set minus the plain
// v1 name, so the name itself proves a V2 collaboration surface.
func IsCollaborationToolName(name any) bool {
	s, ok := name.(string)
	return ok && s != "spawn_agent" && spawnClassifySpawnToolName(s)
}

func spawnClassifySpawnToolName(name string) bool {
	switch name {
	case "spawn_agent", "collaborationspawn_agent", "collaboration.spawn_agent", "collaboration_spawn_agent":
		return true
	}
	return false
}
