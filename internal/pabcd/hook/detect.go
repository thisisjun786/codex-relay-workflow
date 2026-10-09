// Package hook holds the prompt detectors of CXC v0.2.40 (commit 3c1459ac):
// pabcd-state/src/hook.ts:236-322 and memory-write-gate.ts:78-105. The declared
// input-name substitutions use crw spellings without retaining cxc aliases.
//
// These are lexical hints, not authorization or state transitions. The phase
// type is state.Phase, the canonical type consumed by package fsm. The library
// does no IO, starts no process, and compiles its regexes only during calls.
package hook

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const (
	// JavaScript's non-unicode \s; RE2's built-in class is narrower.
	jsSpaceChars = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
	negatedLead  = `^(?:(?:please|좀)\s+)?(?:(?:i|we)\s+(?:do\s+not|don't|dont)\s+(?:want|need)\b|(?:i'd|i\s+would|we'd|we\s+would)\s+(?:rather|prefer)\s+(?:not|you\s+not|you\s+didn't)\b|(?:can|could|would|will)\s+you\s+(?:not|please\s+not)\b|do\s+not|don't|dont|never|no\s+need\s+to|avoid|stop)\b`
	negatedTail  = `(?:crw-?(?:loop|pabcd)|pabcd)\S*\s*(?:을|를|은|는)?\s*(?:(?:돌리|쓰|사용하|실행하|켜|하)지\s*(?:마|말)|말고|금지)`
)

// detectorRE preserves JS whitespace; callers fold only ASCII for the oracle's
// non-unicode /i. RE2's \b and \w already have the required ASCII semantics.
func detectorRE(pattern string) *regexp.Regexp {
	pattern = strings.ReplaceAll(pattern, `\s`, "["+jsSpaceChars+"]")
	pattern = strings.ReplaceAll(pattern, `\S`, "[^"+jsSpaceChars+"]")
	return regexp.MustCompile(pattern)
}

func foldASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

func wordBefore(s string, at int) bool {
	if at == 0 {
		return false
	}
	c := s[at-1]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// guardedMatch implements the lookarounds RE2 lacks. A refused match resumes
// one byte after its ASCII starting character, allowing overlapping candidates.
// These patterns have no left-boundary assertion affected by slicing at from.
func guardedMatch(re *regexp.Regexp, s string, from int, accept func(int, int) bool) []int {
	for from < len(s) {
		at := re.FindStringIndex(s[from:])
		if at == nil {
			return nil
		}
		at[0], at[1] = at[0]+from, at[1]+from
		if accept(at[0], at[1]) {
			return at
		}
		from = at[0] + 1
	}
	return nil
}

func stripQuotes(s string) string {
	// JS dot in an escape excludes all four line terminators, not just LF.
	re := detectorRE(`"(?:\\[^\n\r\x{2028}\x{2029}]|[^"\\])*"|“[^”]*”|'(?:\\[^\n\r\x{2028}\x{2029}]|[^'\\])*'`)
	var out strings.Builder
	from := 0
	for {
		at := guardedMatch(re, s, from, func(start, end int) bool {
			return s[start] != '\'' || !wordBefore(s, start)
		})
		if at == nil {
			out.WriteString(s[from:])
			return out.String()
		}
		out.WriteString(s[from:at[0]])
		out.WriteByte(' ')
		from = at[1]
	}
}

func stripBackticks(line string, explanatory bool) string {
	re := detectorRE("`([^`]*)`")
	allowed := detectorRE(`^(?:\$?(?:crw:)?crw-(?:loop|pabcd)|orchestrate\s+[ipabc])$`)
	beforeVerb := detectorRE(`^(?:(?:please|좀)\s+)?(?:run|use|start|invoke|실행|돌려)\s*$`)
	beforeParticle := detectorRE(`^(?:(?:please|좀)\s*)?$`)
	afterParticle := detectorRE(`^\s*(?:로|으로|써서)`)
	var out strings.Builder
	from := 0
	for _, at := range re.FindAllStringSubmatchIndex(line, -1) {
		out.WriteString(line[from:at[0]])
		inner := line[at[2]:at[3]]
		addressed := beforeVerb.MatchString(foldASCII(line[:at[0]])) ||
			(beforeParticle.MatchString(foldASCII(line[:at[0]])) && afterParticle.MatchString(line[at[1]:]))
		if !explanatory && allowed.MatchString(foldASCII(text.Trim(inner))) && addressed {
			out.WriteString(inner)
		} else {
			out.WriteByte(' ')
		}
		from = at[1]
	}
	out.WriteString(line[from:])
	return out.String()
}

func requestClauses(s string) []string {
	re := detectorRE(`[.;!?]\s*|,\s*|\s+but\s+|\s*(?:하지만|그런데)\s*`)
	nextRequest := detectorRE(`^(?:please\s+)?(?:use|run|start|invoke)\b`)
	folded := foldASCII(s)
	var clauses []string
	from := 0
	for {
		at := guardedMatch(re, folded, from, func(start, end int) bool {
			return folded[start] != ',' || nextRequest.MatchString(folded[end:])
		})
		if at == nil {
			return append(clauses, s[from:])
		}
		clauses = append(clauses, s[from:at[0]])
		from = at[1]
	}
}

// requestLines keeps only clauses that can carry an advisory request. Quoted
// examples, lists, fences, explanatory leads and mode negations stay silent.
func requestLines(prompt string) []string {
	skip := detectorRE(`^(?:>|[-*] |\d+[.)] )`)
	explain := detectorRE(`^(?:(?:please|좀)\s+)?(?:explain|describe|how do|how to|what is|what does|why)\b|^(?:좀\s*)?(?:설명|어떻게|뭐야)`)
	lead, tail := detectorRE(negatedLead), detectorRE(negatedTail)
	var result []string
	fenced := false
	for _, raw := range text.SplitLines(prompt) {
		line := text.Trim(raw)
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced || line == "" || skip.MatchString(line) {
			continue
		}
		explanatory := explain.MatchString(foldASCII(line))
		unquoted := text.Trim(stripQuotes(stripBackticks(line, explanatory)))
		if explanatory || unquoted == "" {
			continue
		}
		for _, clause := range requestClauses(unquoted) {
			clause = text.Trim(clause)
			folded := foldASCII(clause)
			if clause != "" && !lead.MatchString(folded) && !tail.MatchString(folded) {
				result = append(result, clause)
			}
		}
	}
	return result
}

// DetectTrigger returns an advisory phase hint, not a phase transition. Only
// explicit commands I..C or named PABCD requests are recognized, in source order.
func DetectTrigger(prompt string) (state.Phase, bool) {
	command := detectorRE(`^orchestrate\s+([ipabc])(?:\s|$)`)
	marker := detectorRE(`(?:\bcrw-pabcd\b|\bcrw:crw-pabcd\b|\[\$?crw-pabcd\]\(skill://[^)]+\)|\bpabcd\s*(?:로|phase\b))`)
	requested := detectorRE(`(?:\b(?:use|run|start|invoke|enter|apply)\b|(?:시작|진행|적용|실행|돌려|써서|으로|들어가))`)
	phases := []struct {
		pattern string
		phase   state.Phase
	}{
		{`\binterview\b|(?:^|\s)인터뷰(?:\s|$)|\bphase\s*i\b`, state.PhaseI},
		{`\bplan\b|\bphase\s*p\b|계획`, state.PhaseP},
		{`\baudit\b|\bphase\s*a\b|감사`, state.PhaseA},
		{`\bbuild\b|\bphase\s*b\b|구현`, state.PhaseB},
		{`\bcheck\b|\bphase\s*c\b|검증`, state.PhaseC},
		{`pabcd\s*로|\bcrw-pabcd\b`, state.PhaseP},
	}
	for _, line := range requestLines(prompt) {
		line = foldASCII(line)
		if found := command.FindStringSubmatch(line); found != nil {
			return state.Phase(strings.ToUpper(found[1])), true
		}
		if !marker.MatchString(line) || !requested.MatchString(line) {
			continue
		}
		for _, phase := range phases {
			if detectorRE(phase.pattern).MatchString(line) {
				return phase.phase, true
			}
		}
	}
	return "", false
}

// DetectAgbrowseSearchRequest is the source's whole-prompt heuristic, including
// its accepted agbrowe typo. It does not use requestLines' quote/negation filter.
func DetectAgbrowseSearchRequest(prompt string) bool {
	// JS lowercasing expands U+0130; Go's simple lowercase would erase the boundary.
	p := strings.ToLower(strings.ReplaceAll(prompt, "İ", "i\u0307"))
	if !detectorRE(`\bagbrows?e\b`).MatchString(p) {
		return false
	}
	return detectorRE(`\b(?:through|via|using|use|with|ask|question|research|search|browse|look\s*up|find|fetch|verify)\b`).MatchString(p) ||
		detectorRE(`(?:통해|통해서|사용|써서|가지고|질문|물어|조사|검색|리서치|알아봐|찾아|확인|검증|브라우즈)`).MatchString(p)
}

// DetectLoopArmRequest requires both a named mode and an action in an eligible clause.
func DetectLoopArmRequest(prompt string) bool {
	marker := detectorRE(`\bcrw-?loop\b|\bcrw:crw-loop\b|\[\$?crw-loop\]\(skill://[^)]+\)|\bgoal\s*plan\b|\bgoalplan\b|골플랜|\bhotl\b`)
	bare := detectorRE(`i?pabcd\b`)
	action := detectorRE(`\b(?:use|run|start|invoke|arm|create|init|repeat|cycle|resume|continue)\b|(?:실행|돌려|돌리|써서|으로|시작|등록|진행|적용|해줘|이어서|재개)`)
	for _, line := range requestLines(prompt) {
		line = foldASCII(line)
		bareMode := guardedMatch(bare, line, 0, func(start, end int) bool {
			return !wordBefore(line, start) && (start == 0 || line[start-1] != '-' && line[start-1] != '/')
		}) != nil
		if (marker.MatchString(line) || bareMode) && action.MatchString(line) {
			return true
		}
	}
	return false
}

// memoryDestination is the pattern with a 24-UTF16-unit gap. A rune-counted RE2
// repetition would incorrectly accept prompts containing too many astral characters.
func memoryDestination(p string) bool {
	verbs := detectorRE(`\b(?:add|save|write|store|put|record)\b`)
	dest := detectorRE(`^to\s+(?:your\s+|the\s+|my\s+)?(?:memory|memories)\b`)
	const maxGap = 24
	for _, at := range verbs.FindAllStringIndex(p, -1) {
		for pos, units := at[1], 0; pos < len(p) && units <= maxGap; {
			if !wordBefore(p, pos) && dest.MatchString(p[pos:]) {
				return true
			}
			r, size := utf8.DecodeRuneInString(p[pos:])
			if r == '.' || r == '\n' {
				break
			}
			units++
			if r > 0xffff {
				units++
			}
			pos += size
		}
	}
	return false
}

// DetectMemoryWriteRequest recognizes affirmative requests in user prose.
// Quoted examples and task packets cannot authorize durable memory (CRW-1093).
func DetectMemoryWriteRequest(prompt string) bool {
	if text.Trim(prompt) == "" {
		return false
	}
	patterns := []string{
		`기억\s*(해둬|해 둬|해줘|해라|하자|해$|해[.!,]|해서\s*(둬|놔))`,
		`(기억|메모리?)\s*(에다|에도|에|도|해서)?\s*(남겨|남겨둬|적어|적어둬|저장|기록)`,
		`메모리\s*(에|에다)?\s*(남겨|기록|추가|저장|적어|넣어|써)`,
		`(노트|메모)\s*(로|를|에)?\s*(남겨|남겨둬|추가|저장|기록)`,
		`잊지\s*(말고|마|마라|말아)`,
		`\bremember\s+(this|that|it|these|the\s+following)\b`,
		`\bnote\s+(this|that|it)\s+down\b`,
		`\bmake\s+a\s+note\b`,
		`\bkeep\s+(this|that|it)\s+in\s+mind\b`,
		`\bdon'?t\s+forget\b`,
	}
	// Unlike requestLines, actual requests in lists are eligible and don't
	// forget is affirmative. A memory-specific negative wins in its sentence.
	negative := detectorRE(`\b(?:do\s+not|don['’]?t|dont|never|not\s+to|avoid)\s+(?:(?:ever|actually|just|really|please)\s+)*(?:remember|save|store|write|record|keep|make\s+a\s+note|note)\b|(?:기억|저장|기록|남기|적)\s*(?:하|해|해두|해 두|해둬|하라|해라|해줘|해 줘)?지\s*(?:마|말)|(?:기억|저장|기록)\s*금지`)
	explain := detectorRE(`^(?:please\s+)?(?:explain|describe|how\s+to|how\s+do|what\s+does)\b|^(?:설명|어떻게)`)
	list := detectorRE(`^(?:[-*+]\s+|[0-9]+[.)]\s+)`)
	packet := detectorRE(`(?i)<(?:task[-_ ]?packet|instructions|untrusted[-_ ]?text|untrusted[-_ ]?data)\b|^(?:#{1,6}\s+)?(?:begin\s+)?task[-_ ]?packet(?:\s*:|\s*$)`)
	packetEnd := detectorRE(`(?i)</(?:task[-_ ]?packet|instructions|untrusted[-_ ]?text|untrusted[-_ ]?data)\s*>`)
	inPacket := false
	fence := byte(0)
	fenceWidth := 0
	for _, raw := range text.SplitLines(prompt) {
		line := text.Trim(raw)
		if inPacket {
			if packetEnd.MatchString(line) {
				inPacket = false
			}
			continue
		}
		if packet.MatchString(line) {
			inPacket = !packetEnd.MatchString(line)
			continue
		}
		if len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
			n := 0
			for n < len(line) && line[n] == line[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					fence, fenceWidth = line[0], n
				} else if fence == line[0] && n >= fenceWidth && text.Trim(line[n:]) == "" {
					fence = 0
				}
				continue
			}
		}
		if fence != 0 || strings.HasPrefix(line, ">") {
			continue
		}
		line = list.ReplaceAllString(line, "")
		line = foldASCII(memoryRequestUnquote(line))
		if explain.MatchString(line) {
			continue
		}
		// Keep each sentence whole: a negative after 'but' also takes precedence.
		for _, sentence := range detectorRE(`[.!?;]`).Split(line, -1) {
			if negative.MatchString(sentence) {
				continue
			}
			for _, pattern := range patterns {
				if detectorRE(pattern).MatchString(text.Trim(sentence)) {
					return true
				}
			}
			if memoryDestination(sentence) {
				return true
			}
		}
	}
	return false
}
