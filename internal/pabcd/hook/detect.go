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

// unfencedLines are the trimmed nonempty lines of the prompt outside a code fence. A fence opens with three or more backticks
// or tildes and closes with a line of the same character, at least as long, and nothing else; a different fence character
// inside it is text. CRW-1084 (port: fixed): the oracle knew only the backtick fence (hook.ts:236-322), so a tilde example of a
// request was read as one.
func unfencedLines(prompt string) []string {
	var out []string
	var fence byte
	fenceLen := 0
	for _, raw := range text.SplitLines(prompt) {
		line := text.Trim(raw)
		if c, n := fenceRun(line); n >= 3 {
			switch {
			case fence == 0:
				fence, fenceLen = c, n
				continue
			case c == fence && n >= fenceLen && strings.Trim(line, string(fence)) == "":
				fence, fenceLen = 0, 0
				continue
			}
		}
		if fence != 0 || line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// fenceRun is the fence character the line opens with and the length of its run, or zero.
func fenceRun(line string) (byte, int) {
	if line == "" || line[0] != '`' && line[0] != '~' {
		return 0, 0
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	return line[0], n
}

// requestLines keeps only clauses that can carry an advisory request. Quoted
// examples, lists, fences, explanatory leads and mode negations stay silent.
func requestLines(prompt string) []string {
	skip := detectorRE(`^(?:>|[-*] |\d+[.)] )`)
	explain := detectorRE(`^(?:(?:please|좀)\s+)?(?:explain|describe|how do|how to|what is|what does|why)\b|^(?:좀\s*)?(?:설명|어떻게|뭐야)`)
	lead, tail := detectorRE(negatedLead), detectorRE(negatedTail)
	var result []string
	for _, line := range unfencedLines(prompt) {
		if skip.MatchString(line) {
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

// DetectMemoryWriteRequest recognizes the eleven source idioms. Like the oracle,
// this scans the whole prompt and does not infer intent from quotation or negation.
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
		`\bremember\s+(this|that|it|these)\b`,
		`\bnote\s+(this|that|it)\s+down\b`,
		`\bmake\s+a\s+note\b`,
		`\bkeep\s+(this|that|it)\s+in\s+mind\b`,
		`\bdon'?t\s+forget\b`,
	}
	p := foldASCII(prompt)
	for _, pattern := range patterns {
		if detectorRE(pattern).MatchString(p) {
			return true
		}
	}
	return memoryDestination(p)
}

// LoopArmScope is what a loop-arm request says about its scope (CRW-1084).
type LoopArmScope int

const (
	// LoopScopeNone names no project and no current-task implementation: the request is about the task at hand, and the
	// implementation recipe is its answer.
	LoopScopeNone LoopArmScope = iota
	// LoopScopeProject names a project or a coordination of children, which crw-run owns (goal mode where a parent goal was
	// asked for) and the project parent never runs as a loop.
	LoopScopeProject
	// LoopScopeCurrentTask asks, in so many words, to implement a task in this session. The session is then the task's
	// implementer and not the project parent, which is the one exception goal-mode.md makes.
	LoopScopeCurrentTask
)

// PromptRole is what the relay registry says this session is. The hook reads it only through a verified registry read; a
// session with no such evidence is PromptRoleUnknown, and a project link never makes it a parent.
type PromptRole string

// The roles a verified registry read can report.
const (
	PromptRoleUnknown PromptRole = ""
	PromptRoleParent  PromptRole = "parent"
	PromptRoleTask    PromptRole = "task"
)

// ClassifyLoopArmScope reads the scope words of a prompt that has already been found to ask for a loop. It is lexical and reads
// only the request clauses requestLines keeps, so a quoted or listed example, a fence, an explanation and a negated clause
// ("do not implement this task") say nothing about the scope. A current-task implementation needs an implement verb and a
// this/current session, task or issue in the SAME clause, with no negation before the verb or after it ("without
// implementing", "구현하지 마"); a project is the word "project", a Linear project link or a coordination word in any clause.
// The current-task exception wins over a project mention.
func ClassifyLoopArmScope(prompt string) LoopArmScope {
	implement := detectorRE(`\b(?:implement|build|fix|code|develop|work\s+on)\b|구현|고쳐|수정|개발|작업해`)
	current := detectorRE(`\b(?:this|current|my)\s+(?:session|task|issue|thread|worktree|checkout)\b|\bin\s+the\s+current\s+(?:session|thread)\b|(?:이|현재)\s*(?:세션|작업|이슈|태스크|스레드)|지금\s*세션`)
	negatedBefore := detectorRE(`\b(?:not|never|without|don't|dont|cannot|can't|instead\s+of|rather\s+than)\b|\bno\s|말고`)
	negatedAfter := detectorRE(`^\S*\s*(?:(?:하|지|고)\s*)?(?:지\s*)?(?:마|말|않|못)`)
	project := detectorRE(`\bprojects?\b|프로젝트|linear\.app/\S+/project/|\bcoordinat\w*|\bchild(?:ren)?\b|\bsupervis\w*|조정|부모|자식|감독`)
	clauses := requestLines(prompt)
	for i := range clauses {
		clauses[i] = foldASCII(clauses[i])
	}
	for _, clause := range clauses {
		if !current.MatchString(clause) {
			continue
		}
		for _, at := range implement.FindAllStringIndex(clause, -1) {
			if !negatedBefore.MatchString(clause[:at[0]]) && !negatedAfter.MatchString(clause[at[1]:]) {
				return LoopScopeCurrentTask
			}
		}
	}
	if project.MatchString(strings.Join(clauses, "\n")) {
		return LoopScopeProject
	}
	return LoopScopeNone
}
