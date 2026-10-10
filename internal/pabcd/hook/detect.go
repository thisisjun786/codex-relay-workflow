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
	"unicode"
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
	negative := detectorRE(`\b(?:do\s+not|don['’]?t|dont|never|not\s+to|avoid|let['’]?s\s+not|(?:must|should|shall|will|would)\s+not|(?:mustn|shouldn|shan|won|wouldn)['’]?t)\s+(?:(?:ask|want|need|expect|have)\s+you\s+to\s+)?(?:(?:ever|actually|just|really|please)\s+)*(?:remember\b|keep\s+(?:this|that|it)\s+in\s+mind\b|make\s+a\s+note\b|note\s+(?:this|that|it)\s+down\b|(?:save|store|write|record|keep|note)\s+(?:(?:(?:this|that|it)\s+)?(?:to|in|into|as)\s+)?(?:memory|memories|a\s+note|notes?)\b|(?:save|store|write|record|keep|note)\s+(?:this|that|it)(?:\s+down)?\s*(?:[.!?;,]|$))|(?:기억|저장|기록|남기|적)\s*(?:하|해|해두|해 두|해둬|하라|해라|해줘|해 줘)?지\s*(?:마|말)|(?:기억|저장|기록)\s*금지`)
	explain := detectorRE(`^(?:please\s+)?(?:explain|describe|how\s+to|how\s+do|what\s+does)\b|^(?:설명|어떻게)`)
	list := detectorRE(`^(?:[-*+]\s+|[0-9]+[.)]\s+)`)
	packet := detectorRE(`(?i)<(?:task[-_ ]?packet|instructions|untrusted[-_ ]?text|untrusted[-_ ]?data)\b|^(?:#{1,6}\s+)?(?:begin\s+)?task[-_ ]?packet(?:\s*:|\s*$)`)
	packetEnd := detectorRE(`(?i)</(?:task[-_ ]?packet|instructions|untrusted[-_ ]?text|untrusted[-_ ]?data)\s*>`)
	inPacket := false
	fence := byte(0)
	fenceWidth := 0
	var prose strings.Builder
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
		if list.MatchString(line) {
			prose.WriteString(";")
		}
		line = list.ReplaceAllString(line, "")
		prose.WriteString(line)
		prose.WriteString("\n")
	}
	// Quote delimiters and sentence-level negation can span physical lines.
	// Preserve terminal punctuation so a recall question cannot become an
	// affirmative Korean sentence merely by deleting its question mark.
	eligible := foldASCII(memoryRequestUnquote(prose.String()))
	for _, sentence := range detectorRE(`[^.!?;]+[.!?;]?`).FindAllString(eligible, -1) {
		sentence = text.Trim(sentence)
		if explain.MatchString(sentence) || negative.MatchString(sentence) {
			continue
		}
		if detectorRE(`기억\s*해\s*\?$`).MatchString(sentence) {
			continue
		}
		for _, pattern := range patterns {
			if detectorRE(pattern).MatchString(sentence) {
				return true
			}
		}
		if memoryDestination(sentence) {
			return true
		}
	}
	return false
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
// only the request clauses requestLines keeps, after scopeText has dropped the example spans, so a quoted, listed, fenced or
// plainly introduced example ("Example: ...", "e.g.", "예: ..."), an explanation and a negated clause say nothing about the scope.
//
// A current-task implementation needs an implement verb and a this/current session, task or issue in the SAME clause, with no
// negation governing the verb: a negation word right before it, at most a few filler words apart ("do not actually implement",
// "not to implement"), or a Korean negation after it ("구현하지 마"). A negation elsewhere in the clause ("no questions, please
// implement ...") does not govern the verb. Nor does the verb count when another agent does it: a children, worker, subagent or
// "child task/lane/..." up to three words before it ("while child tasks implement their issues", "while the children then
// implement") makes the clause a coordination, even for a noun that is no coordination word on its own ("while the workers
// meanwhile implement"), unless the noun is the object of an earlier verb whose subject goes on ("consult the children and
// implement this task", "consult the children then implement", "자식 확인하고 구현해") or a child process (delegation, CRW-1166).
//
// A project is a Linear project link, a coordination word (the verb coordinate but not the noun - coordinateVerb -, supervise,
// children or child tasks, 감독, 부모, 자식 but not a child process, and 조정 unless it adjusts a value - coordinateKorean), or the word "project" - except where "project" only names the place of a single-task fix: "in this
// project", "of the project", "이 프로젝트에서" in a clause that carries its own ungoverned implement verb. A named project ("the
// migration project") stays a project. The current-task exception wins over a project mention.
func ClassifyLoopArmScope(prompt string) LoopArmScope {
	current := detectorRE(`\b(?:this|current|my)\s+(?:session|task|issue|thread|worktree|checkout)\b|\bin\s+the\s+current\s+(?:session|thread)\b|(?:이|현재)\s*(?:세션|작업|이슈|태스크|스레드)|지금\s*세션`)
	childProcess := detectorRE(`\bchild\s+process(?:es)?\b|\bsubprocess(?:es)?\b|자식\s*프로세스`)
	// "coordinate" and "coordinates" are also a plain noun (coordinate values, the coordinates in the parser); coordinateVerb
	// reads the bare forms. "coordinating" and "coordination" always count.
	coordination := detectorRE(`linear\.app/\S+/project/|\bcoordinat(?:ing|ion)\b|\bsupervis(?:e|es|ing|ion)\b|\bchildren\b|\bchild\s+(?:tasks?|issues?|sessions?|lanes?|threads?|agents?|goals?)\b|감독|부모|자식`)
	projectWord := detectorRE(`\bprojects?\b|프로젝트`)
	location := detectorRE(`\b(?:in|inside|within|across|throughout|of|for)\s+(?:the\s+current|this|the|current|my|our)\s+(?:project|repo|repository|codebase)\b|(?:이|현재|우리|내)\s*프로젝트\s*(?:에서|안에서|안의|의|에)`)
	clauses := requestLines(scopeText(prompt))
	for i := range clauses {
		clauses[i] = foldASCII(clauses[i])
	}
	for _, clause := range clauses {
		if current.MatchString(clause) && ungovernedImplement(clause) {
			return LoopScopeCurrentTask
		}
	}
	for _, clause := range clauses {
		if rest := childProcess.ReplaceAllString(clause, " "); coordination.MatchString(rest) || coordinateVerb(rest) || coordinateKorean(rest) || delegatedImplement(clause) {
			return LoopScopeProject
		}
		if ungovernedImplement(clause) {
			clause = location.ReplaceAllString(clause, " ")
		}
		if projectWord.MatchString(clause) {
			return LoopScopeProject
		}
	}
	return LoopScopeNone
}

// coordinateVerb reports a bare "coordinate" or "coordinates" in a folded clause that is the verb and not the noun
// (ClassifyLoopArmScope). The word is the noun when a determiner, a preposition, a coordinate-kind modifier or a transitive
// work verb comes right before it ("the coordinates", "of coordinate values", "fix coordinate rounding", "normalize these
// coordinates") or a noun it compounds with comes right after it ("coordinate values", "coordinate system"); anywhere else,
// at a clause start, after "to", an adverb or a subject ("to actively coordinate", "so we coordinate", "this session
// coordinates the lanes"), it is the verb.
func coordinateVerb(clause string) bool {
	word := detectorRE(`\bcoordinates?\b`)
	nounBefore := detectorRE(`\b(?:the|a|an|of|in|on|at|for|from|with|into|by|per|about|between|these|those|this|that|its|their|our|your|his|her|my|each|every|any|some|no|new|raw|x|y|z|xy|xyz|gps|geo|polar|cartesian|screen|world|local|global|pixel|map|texture|fix|fixes|fixing|implement|build|code|develop|debug|parse|convert|round|compute|normalize|validate|refactor|handle|store|read|write|test|update|change|clamp|sort|format|serialize|deserialize)\s+$`)
	nounAfter := detectorRE(`^\s+(?:values?|systems?|transforms?|transformation|conversions?|rounding|parsing|parsers?|math|bugs?|data|fields?|types?|pairs?|spaces?|frames?|formats?|precision|offsets?|grids?|mappings?|helpers?|utils?|functions?|logic|tests?|columns?|arrays?|lists?|points?|calculations?|projections?|are|is|was|were)\b`)
	for _, at := range word.FindAllStringIndex(clause, -1) {
		if !nounBefore.MatchString(clause[:at[0]]) && !nounAfter.MatchString(clause[at[1]:]) {
			return true
		}
	}
	return false
}

// ungovernedImplement reports an implement verb in a folded clause that no negation governs and no other agent owns
// (ClassifyLoopArmScope).
func ungovernedImplement(clause string) bool {
	session, _ := implementVerbs(clause)
	return session
}

// delegatedImplement reports an ungoverned implement verb that a named agent (a worker, a subagent, a child task, 자식, ...) owns
// as its subject or delegate: the clause then coordinates that agent even though no coordination word names it (CRW-1166, "while
// the workers meanwhile implement their issues"). The pronouns they and others carry no such signal.
func delegatedImplement(clause string) bool {
	_, delegated := implementVerbs(clause)
	return delegated
}

// implementVerbs scans the implement verbs of a folded clause that no negation governs: session when one is this session's own,
// delegated when one belongs to a named agent (delegation).
func implementVerbs(clause string) (session, delegated bool) {
	implement := detectorRE(`\b(?:implement|build|fix|code|develop|work\s+on)\b|구현|고쳐|수정|개발|작업해`)
	filler := `(?:(?:,|\s)+(?:actually|really|yet|ever|even|just|please|to|be|want|need|you|i|we|me|going|try|trying|start|starting|begin|bother|asking))*`
	negatedBefore := detectorRE(`(?:\b(?:not|never|don't|dont|cannot|can't|won't|wont|shouldn't|mustn't|avoid|stop|without|instead\s+of|rather\s+than|no\s+need\s+to)\b` +
		filler + `|\bno|말고)(?:,|\s)*$`)
	negatedAfter := detectorRE(`^\S*\s*(?:(?:하|지|고)\s*)?(?:지\s*)?(?:마|말|않|못)`)
	for _, at := range implement.FindAllStringIndex(clause, -1) {
		if negatedBefore.MatchString(clause[:at[0]]) || negatedAfter.MatchString(clause[at[1]:]) {
			continue
		}
		if noun, ok := delegation(clause[:at[0]], clause[at[1]:]); !ok {
			session = true
		} else if noun != "they" && noun != "others" && !nounUse(clause[:at[0]], noun, clause[at[0]:at[1]], clause[at[1]:]) {
			delegated = true
		}
	}
	return session, delegated
}

// nounUse reports an implement keyword that is a noun beside an agent noun and so no delegated verb (CRW-1166, "optimize the worker
// build cache", "워커 개발 환경"): a singular English agent noun right before a bare keyword is its compound modifier (a
// singular subject takes "builds", "will build") unless a causative (have, let, make, get, help) governs the noun, whose bare verb
// it then is ("have the worker build its part", "let the subagent implement", an ambiguous "have the worker build cache rebuilt"
// included: the pointer); a Korean keyword 구현/수정/개발 that no 하/해/시 verb ending follows is a noun.
// The session decision is untouched; only the project signal delegatedImplement reads this.
func nounUse(prefix, noun, verb, rest string) bool {
	if detectorRE(`^(?:worker|sub-?agent|child\s+(?:task|issue|session|lane|thread|agent|goal|worker)|other\s+(?:agent|session|task|thread))$`).MatchString(noun) {
		before, ok := strings.CutSuffix(strings.TrimRight(prefix, " \t"), noun)
		causative := detectorRE(`\b(?:have|has|had|having|let|lets|letting|make|makes|made|making|get|gets|got|getting|help|helps|helped|helping)\s+(?:(?:the|a|an|this|that|each|every|one|your|our|its|their|my|his|her|any)\s+)*$`)
		return ok && !causative.MatchString(before)
	}
	switch verb {
	case "구현", "수정", "개발":
		return !detectorRE(`^(?:(?:을|를)\s*)?(?:하|해|했|한|할|함|시)`).MatchString(rest)
	}
	return false
}

// delegation reports the agent noun whose verb the implement verb after the folded clause prefix is. Another agent as the
// subject or the delegate of the verb ("while child tasks implement", "ask the children to implement", "the workers will then
// implement") is not this session implementing: the verb must not follow such a noun by up to three words of the same clause.
// A child process or subprocess is no agent, and a noun is the object of an earlier verb whose subject goes on, so the session
// implements, in these structures:
//   - a comma or a coordinating conjunction (and, but, or, plus) right after the noun starts a gap of conjunctions only
//     ("consult the children and implement this task", "consult the children, then implement this task");
//   - a transitive verb right before the noun (objectLead) and only sentence adverbs and commas after it ("consult the
//     children then implement", "consult the children, then, implement") - CRW-1166; a subordinating conjunction or a
//     causative before the noun ("while/as/when the children then implement", "let the children then implement") leaves the
//     noun the subject;
//   - a Korean noun that is no subject (no 이/가/은/는 on it or on a word before the connective) followed by a word ending in
//     the connective -고 ("자식 작업을 확인하고 구현해", "워커에게 물어보고 구현해") and no explicit other subject after the
//     connective and no causative on the verb (CRW-1166: "... 확인하고 워커가 구현해", "... 확인하고 구현하게 해" stay delegated).
//
// A sentence adverb right after the noun with none of these ("the children then implement", "they also implement", "the
// children each implement") or set off by commas ("the children, then, implement") leaves the noun the subject of the verb.
// Anything else ambiguous stays delegated, which is the pointer.
func delegation(prefix, suffix string) (noun string, delegated bool) {
	childProcess := detectorRE(`\bchild\s+process(?:es)?\b|\bsubprocess(?:es)?\b|자식\s*프로세스`)
	// detectorRE expands \s and \S into bracket classes, so the separator and word classes here spell jsSpaceChars out.
	sep, word := `[`+jsSpaceChars+`,;]`, `[^`+jsSpaceChars+`,;]`
	agent := detectorRE(`(?:\b(?:children|workers?|sub-?agents?|others|they|child\s+(?:tasks?|issues?|sessions?|lanes?|threads?|agents?|goals?|workers?)|other\s+(?:agents?|sessions?|tasks?|threads?))\b|(?:자식|하위|워커)\S*)((?:` + sep + `+` + word + `+){0,3}` + sep + `*)$`)
	space := `[` + jsSpaceChars + `]*`
	conjunction := detectorRE(`^` + space + `(?:[,;]|\b(?:and|but|or|plus)\b)(?:` + sep + `|\b(?:and|then|but|or|also|plus)\b)*$`)
	parenthetical := detectorRE(`[,;]` + space + `\b(?:then|also)\b` + space + `,` + space + `$`)
	adverbs := detectorRE(`^(?:` + sep + `|\b(?:then|also|next|afterwards)\b)*$`)
	objectLead := detectorRE(`\b(?:consult|ask|read|check|inspect|review|ping|query|poll|notify|tell|brief|summari[sz]e|gather|collect|(?:check|consult|confer|sync|talk|speak|coordinate)\s+with|wait\s+for|look\s+at|report\s+to|hear\s+from|talk\s+to|speak\s+to)\s+(?:(?:all|each|of|the|your|its|their|our|my|these|those|other)\s+)*$`)
	folded := childProcess.ReplaceAllString(prefix, " ")
	m := agent.FindStringSubmatchIndex(folded)
	if m == nil {
		return "", false
	}
	noun, gap := folded[m[0]:m[2]], folded[m[2]:m[3]]
	noun = strings.TrimSpace(noun)
	if hangulConnective(noun, gap, suffix) {
		return noun, false
	}
	if objectLead.MatchString(folded[:m[0]]) && adverbs.MatchString(gap) {
		return noun, false
	}
	return noun, !conjunction.MatchString(gap) || parenthetical.MatchString(gap)
}

// hangulConnective reports a Korean agent noun (자식, 하위, 워커 plus its particle) that is the object of an earlier verb joined to
// the implement verb by the connective -고: a word of the gap ends in 고 and neither the noun nor a word before that word ends
// in a subject particle (이, 가, 은, 는) after at least one other character. A subject ("자식이 확인하고 구현해") stays delegated.
// The implement verb is then still this session's own only when nothing else takes it over: a later word of the gap that is
// another agent noun or ends in a subject particle other than a first-person one ("확인하고 워커가 구현해") is the verb's explicit
// subject, and a causative on the verb or on a later verb of its -고 chain (chainCausative) hands it to a delegate; both stay
// delegated.
func hangulConnective(noun, gap, suffix string) bool {
	if !strings.HasPrefix(noun, "자식") && !strings.HasPrefix(noun, "하위") && !strings.HasPrefix(noun, "워커") {
		return false
	}
	if chainCausative(suffix) {
		return false
	}
	subject := detectorRE(`.+[이가은는]$`)
	firstPerson := detectorRE(`^(?:제가|내가|저는|나는|우리가|우리는|저희가|저희는)$`)
	connective := false
	for _, w := range strings.FieldsFunc(noun+" "+gap, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) || r == '\ufeff' }) {
		if connective {
			if (strings.HasPrefix(w, "자식") || strings.HasPrefix(w, "하위") || strings.HasPrefix(w, "워커") || subject.MatchString(w)) && !firstPerson.MatchString(w) {
				return false
			}
			continue
		}
		if strings.HasSuffix(w, "고") {
			connective = true
		} else if subject.MatchString(w) {
			return false
		}
	}
	return connective
}

// chainCausative reports a causative that governs the Korean implement verb whose folded clause rest is suffix: on the verb
// itself ("구현하게 해", "구현하도록 해", "구현시켜") or on a later verb of its sentence that a chain of connectives (-고, -서, -며)
// joins it to ("구현하고 수정하게 해", "구현하고 테스트하고 수정시켜", CRW-1166), also past an object or a bare noun of a chain verb
// ("구현하고 테스트를 수정하게 해", "구현하고 테스트 수정하도록 해", "구현하고 개요 수정하도록 해"). The chain ends at a finite verb
// (finiteKorean) that carries no causative
// ("구현하고 수정해", "구현하고 테스트를 수정해", "구현하고 수정할게"), at the end of the sentence and at another agent noun
// ("구현하고 워커에게 테스트하게 해": that agent's own verb), which leave the implement verb this session's own.
func chainCausative(suffix string) bool {
	if detectorRE(`^(?:하게|하도록|하라고|토록|시켜|시키)`).MatchString(suffix) {
		return true
	}
	words := strings.FieldsFunc(suffix, func(r rune) bool { return r == ',' || unicode.IsSpace(r) || r == '\ufeff' })
	for i, w := range words {
		next := ""
		if i+1 < len(words) {
			next = words[i+1]
		}
		connective := strings.HasSuffix(w, "고") || strings.HasSuffix(w, "서") || strings.HasSuffix(w, "며")
		switch {
		case strings.Contains(w, "시켜") || strings.Contains(w, "시키"),
			strings.HasSuffix(w, "도록") || strings.HasSuffix(w, "토록") || strings.HasSuffix(w, "라고"),
			strings.HasSuffix(w, "게") && (strings.HasPrefix(next, "해") || strings.HasPrefix(next, "하") || strings.HasPrefix(next, "만들")):
			return true
		case strings.HasPrefix(w, "자식") || strings.HasPrefix(w, "하위") || strings.HasPrefix(w, "워커"):
			return false
		case connective:
			continue
		case strings.ContainsAny(w, ".!?。") || finiteKorean(w):
			return false
		}
	}
	return false
}

// finiteKorean reports a Korean word that ends in a finite verb ending, for chainCausative (CRW-1166): 해, 줘 or 죠, or 요, 다, 라,
// 래, 게 after a syllable that makes it a verb ending ("수정해요", "고쳐요", "수정합니다", "구현한다", "수정해라", "할래", "할게").
// A noun or a postposition that merely ends in such a syllable ("개요", "필요", "바다", "카메라", "미래", "무게", "에 대해",
// "위해", "이해") is no finite verb and does not end the chain; an unread ending keeps the search going, which errs to the pointer.
func finiteKorean(w string) bool {
	r := []rune(strings.TrimRight(w, ".!?。"))
	if len(r) == 0 {
		return false
	}
	last, prev := r[len(r)-1], rune(0)
	if len(r) > 1 {
		prev = r[len(r)-2]
	}
	finalRieul := prev >= 0xAC00 && prev <= 0xD7A3 && (prev-0xAC00)%28 == 8
	switch last {
	case '줘', '죠':
		return true
	case '해':
		return !detectorRE(`^(?:위해|대해|통해|관해|의해|인해|이해|피해|방해|손해|오해|견해|재해|침해|저해|장해)$`).MatchString(string(r))
	case '요':
		return prev != 0 && strings.ContainsRune("해세줘봐돼게래네데까지죠어아워와여쳐려겨", prev)
	case '다':
		return prev != 0 && strings.ContainsRune("니한했된됐는었았였겠렸쳤갔왔졌냈봤", prev)
	case '라':
		return prev != 0 && strings.ContainsRune("해하어아여쳐려봐거", prev)
	case '래', '게':
		return finalRieul
	}
	return false
}

// coordinateKorean reports 조정 in a folded clause that is coordination and not the adjustment of a value (CRW-1166, port: deviation
// from the oracle, which counted every 조정). The word is an adjustment when a setting-like noun comes right before it ("값 조정",
// "간격을 조정", "크기 재조정") or 값, 폭, 량, 치 right after it ("조정값"); anywhere else ("레인들을 조정", "작업 순서 조정") it is
// coordination, and an ambiguous structure keeps the pointer.
func coordinateKorean(clause string) bool {
	word := detectorRE(`조정`)
	nounBefore := detectorRE(`(?:값|설정|옵션|파라미터|매개변수|타임아웃|임계값|임계치|간격|크기|색상|색|여백|레이아웃|폰트|글꼴|가중치|좌표|해상도|밝기|볼륨|정렬|속도|버퍼|폭|너비|높이|패딩|마진|스타일|포맷|정밀도|오프셋|한도|배율|비율|세기|강도|주기|길이|두께|개수|수치)(?:을|를|의|도|은|는|들을|들)?\s*재?$`)
	nounAfter := detectorRE(`^(?:값|폭|량|치)`)
	for _, at := range word.FindAllStringIndex(clause, -1) {
		if !nounBefore.MatchString(clause[:at[0]]) && !nounAfter.MatchString(clause[at[1]:]) {
			return true
		}
	}
	return false
}

// scopeText is the prompt without its plainly introduced examples, for ClassifyLoopArmScope (CRW-1084): a parenthesis that
// opens with an example marker is dropped, a line is cut at an example marker ("example:", "for example", "for instance",
// "e.g.", "sample:", "such as", "예:", "예시", "예를 들어", "예컨대", "가령", "이를테면"), and a marker that ends its line
// ("Example:") drops the next nonempty line too, unless that line opens or closes a fence. Quotes, backticks, lists and fences
// are requestLines' own.
func scopeText(prompt string) string {
	paren := detectorRE(`\((?:e\.g\.?|eg\.|for\s+example|for\s+instance|examples?\b|samples?\b|예시|예\s*:|예를\s*들|예컨대|가령)[^)]*\)`)
	marker := detectorRE(`(?:^|[^a-z0-9_])(?:for\s+example|for\s+instance|e\.g\.|eg\.|examples?\s*:|samples?\s*:|such\s+as)|예\s*[:)]|예시|예를\s*들|예컨대|가령|이를테면`)
	headingRest := detectorRE(`^(?:\s|[:,.)-])*$`)
	var out []string
	skipNext := false
	for _, line := range text.SplitLines(prompt) {
		if skipNext && text.Trim(line) != "" {
			skipNext = false
			if _, n := fenceRun(text.Trim(line)); n < 3 {
				continue
			}
		}
		folded := paren.ReplaceAllString(foldASCII(line), " ")
		if at := marker.FindStringIndex(folded); at != nil {
			skipNext = headingRest.MatchString(folded[at[1]:])
			folded = folded[:at[0]]
		}
		out = append(out, folded)
	}
	return strings.Join(out, "\n")
}
