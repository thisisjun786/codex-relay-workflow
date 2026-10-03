package fsm

import (
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// OrchestrateVerb is what a chat command asks for: a phase to enter, or a control verb. IDLE is not a verb.
type OrchestrateVerb string

// The verbs of VERB_TOKENS. VerbConstructor is not a verb of the grammar: the oracle looks the lower-cased token up in a plain
// object, so the inherited key "constructor" answers with Object, a function, where the other tokens answer with a string.
const (
	VerbI           OrchestrateVerb = "I"
	VerbP           OrchestrateVerb = "P"
	VerbA           OrchestrateVerb = "A"
	VerbB           OrchestrateVerb = "B"
	VerbC           OrchestrateVerb = "C"
	VerbD           OrchestrateVerb = "D"
	VerbStatus      OrchestrateVerb = "status"
	VerbReset       OrchestrateVerb = "reset"
	VerbConstructor OrchestrateVerb = "constructor"
)

// OrchestrateCommand is one parsed chat command. RawAttest is the brace-balanced text after --attest, nil when there is none (or
// no balanced object); Attest is its coercion, nil when absent or malformed; AttestError says why it is nil, and is empty otherwise.
type OrchestrateCommand struct {
	Verb        OrchestrateVerb
	RawAttest   *string
	Attest      *attest.Attestation
	AttestError string
}

const (
	word          = "orchestrate"
	attestFlag    = "--attest"
	errNoBalanced = "no balanced JSON object after --attest"
	errNotJSON    = "attest JSON is not valid JSON"
	errNoFromTo   = "attest JSON missing valid from/to. Every attest names the edge it advances: {\"from\":\"<phase>\",\"to\":\"<phase>\",\"did\":\"...\"} plus that edge's keys (ATTEST-SHAPE-01)."
)

// isJSSpace is JavaScript's \s: the 25 code points String.prototype.trim also strips, taken from package text.
func isJSSpace(r rune) bool { return text.Trim(string(r)) == "" }

func isASCIILetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// verbToken is VERB_TOKENS looked up with the lower-cased token, which is ASCII letters only.
func verbToken(token string) (OrchestrateVerb, bool) {
	switch lower := strings.ToLower(token); lower {
	case "i", "p", "a", "b", "c", "d":
		return OrchestrateVerb(strings.ToUpper(lower)), true
	case "status", "reset", "constructor":
		return OrchestrateVerb(lower), true
	}
	return "", false
}

// stripPrefix is s.replace(PREFIX, ""): PREFIX is /^(?:\$crw:crw-|\$crw-|crw\s+|\/)?/, so at most one prefix goes, and the match
// is case-sensitive.
func stripPrefix(s string) string {
	for _, p := range []string{"$crw:crw-", "$crw-", "/"} {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}
	if rest, ok := strings.CutPrefix(s, "crw"); ok {
		if trimmed := strings.TrimLeftFunc(rest, isJSSpace); trimmed != rest {
			return trimmed
		}
	}
	return s
}

// scanCommand is COMMAND = /^orchestrate\s+([A-Za-z]+)\s*(.*)$/i. The /i of a non-unicode expression folds ASCII only (U+017F
// and U+212A stay apart from s and k), and the dot stops at CR, LF, U+2028 and U+2029, so a rest holding one of them after
// the white space that follows the verb is no match. The rest it returns has no white space at either end: the line was trimmed.
func scanCommand(s string) (token, rest string, ok bool) {
	for i := 0; i < len(word); i++ {
		if i >= len(s) || s[i]|0x20 != word[i] {
			return "", "", false
		}
	}
	body := strings.TrimLeftFunc(s[len(word):], isJSSpace)
	if len(body) == len(s)-len(word) {
		return "", "", false
	}
	n := 0
	for n < len(body) && isASCIILetter(body[n]) {
		n++
	}
	rest = strings.TrimLeftFunc(body[n:], isJSSpace)
	if n == 0 || strings.ContainsAny(rest, "\n\r\u2028\u2029") {
		return "", "", false
	}
	return body[:n], rest, true
}

// startsAttest is /^--attest\s+\{/.
func startsAttest(s string) bool {
	rest, ok := strings.CutPrefix(s, attestFlag)
	body := strings.TrimLeftFunc(rest, isJSSpace)
	return ok && len(body) < len(rest) && strings.HasPrefix(body, "{")
}

// flagIndex is s.search(/--attest\b/): the first --attest followed by the end or by a character that is not a word character.
func flagIndex(s string) int {
	for from := 0; ; {
		i := strings.Index(s[from:], attestFlag)
		if i < 0 {
			return -1
		}
		i += from
		if end := i + len(attestFlag); end == len(s) || !(isASCIILetter(s[end]) || s[end] == '_' || '0' <= s[end] && s[end] <= '9') {
			return i
		}
		from = i + 1
	}
}

// ExtractBalancedJSON returns the brace-balanced object that begins at the first "{" of s; a "}" inside a string literal does not
// close it. Scanning bytes is scanning UTF-16 units here: every character it tests is ASCII.
func ExtractBalancedJSON(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		switch c := s[i]; {
		case inString:
			escaped, inString = !escaped && c == '\\', escaped || c != '"'
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// parseAttestTail parses the --attest tail of a command's argument text: its balanced JSON, the attestation coerced from it and
// the error text when there is none. The balanced text ends at its own closing brace, so nothing follows the one value decoded.
func parseAttestTail(rest string) (raw *string, att *attest.Attestation, errText string) {
	idx := flagIndex(rest)
	if idx < 0 {
		return nil, nil, ""
	}
	balanced, ok := ExtractBalancedJSON(rest[idx+len(attestFlag):])
	if !ok {
		return nil, nil, errNoBalanced
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(balanced))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return &balanced, nil, errNotJSON
	}
	if att = attest.Coerce(v); att == nil {
		return &balanced, nil, errNoFromTo
	}
	return &balanced, att, ""
}

// ParseOrchestrateCommand finds the first line of prompt that is a chat orchestrate command and parses it, or returns nil. A
// command is line-anchored: after trim and one optional prefix the line is "orchestrate <verb>", optionally followed by
// "--attest {json}" and nothing else, so a verb buried in prose, or a line with anything after its JSON, is skipped and a later
// line may still match. It never fails.
func ParseOrchestrateCommand(prompt string) *OrchestrateCommand {
	for _, line := range text.SplitLines(prompt) {
		token, rest, ok := scanCommand(stripPrefix(text.Trim(line)))
		if !ok {
			continue
		}
		if verb, ok := verbToken(token); ok && (rest == "" || startsAttest(rest)) {
			raw, att, errText := parseAttestTail(rest)
			if raw == nil || strings.Index(rest, *raw)+len(*raw) == len(rest) {
				return &OrchestrateCommand{Verb: verb, RawAttest: raw, Attest: att, AttestError: errText}
			}
		}
	}
	return nil
}
