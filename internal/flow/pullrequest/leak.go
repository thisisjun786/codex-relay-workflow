package pullrequest

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

// The kinds of Leak.
const (
	LeakPath     = "path"
	LeakHostname = "hostname"
	LeakThreadID = "thread_id"
	LeakCallID   = "call_id"
	LeakRunID    = "run_id"
	LeakSecret   = "secret"
)

// Leak is one thing in a pull request's title or body that must not reach a public repository (CRW-1451). Match is the text found;
// for a secret it is only the line number, so a secret is never copied into a message. Use Short to print a leak.
type Leak struct {
	Kind  string
	Match string
}

// LeakContext is what is local to the host the command runs on: the machine's name and the user's home directory.
type LeakContext struct {
	Hostname string
	Home     string
}

// shortLen is how much of a match Short prints.
const shortLen = 12

// Short is the kind and the first 12 characters of the match, safe to print: a whole match is never echoed.
func (l Leak) Short() string {
	m := []rune(l.Match)
	if len(m) > shortLen {
		return fmt.Sprintf("%s %s...", l.Kind, string(m[:shortLen]))
	}
	return l.Kind + " " + string(m)
}

// pathPattern finds the top directories home, state and scratch as the root of a path. The left side is the start of the text or of
// a line, or a character that cannot end a path segment (so `file:///home/x`, a backticked, quoted or bracketed path and `x=/state/y` match,
// and `docs/state/x.md`, `a/home/b` and `.../home/` do not). The right side is a slash, the end of the line or a closing character.
var pathPattern = regexp.MustCompile(pathLeft + "(/(?:home|state|scratch))" + pathRight)

// pathLeft and pathRight are the boundaries of a path root, shared by the fixed roots and the home directory.
const (
	pathLeft  = "(?m)(?:^|[^A-Za-z0-9_.\\-])"
	pathRight = "(?:/|$|[\\s`'\")\\]>,;:*])"
)

// pathRest is the rest of the path token after its root, so Match holds the path and not only its first segment.
var pathRest = regexp.MustCompile("^[^\\s`'\"<>)\\]*]*")

// fencePattern is the inside of a fenced code block, the lines after the opening fence up to the closing one.
var fencePattern = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```")

// idPatterns are the kinds that are a fixed shape. None matches a 40-hex or 12-hex SHA, CRW-NNNN, OWNER/NAME#N or OWNER:NAME:N.
var idPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{LeakThreadID, regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)},
	{LeakCallID, regexp.MustCompile(`\bc_[0-9a-f]{12}\b`)},
	{LeakCallID, regexp.MustCompile(`\bcall_[A-Za-z0-9]{16,}\b`)},
	{LeakCallID, regexp.MustCompile(`\b(?:fc|toolu)_[A-Za-z0-9]{16,}\b`)},
	{LeakCallID, regexp.MustCompile(`crw-flow-op:|crwh1:`)},
	{LeakRunID, regexp.MustCompile(`\brun-[0-9a-f]{12}\b`)},
	{LeakRunID, regexp.MustCompile(`\b(?:req|rep)_[0-9a-f]{24}\b`)},
}

// Leaks is every path under the host's home, state or scratch, the host's name, a thread, call or run id and a secret in text. Run it
// over the title and the whole body (the Carries line included). The result is in the order the kinds are listed above, then by
// position, without repeats; it is empty when nothing leaks.
func Leaks(text string, c LeakContext) []Leak {
	var out []Leak
	seen := map[Leak]bool{}
	add := func(kind, match string) {
		l := Leak{Kind: kind, Match: match}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	type found struct {
		at    int
		match string
	}
	var paths []found
	for _, m := range pathPattern.FindAllStringSubmatchIndex(text, -1) {
		start := m[2]
		paths = append(paths, found{start, text[start:m[3]] + pathRest.FindString(text[m[3]:])})
	}
	if home := strings.TrimRight(strings.TrimSpace(c.Home), "/"); len(home) > 1 {
		re := regexp.MustCompile(pathLeft + "(" + regexp.QuoteMeta(home) + ")" + pathRight)
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			paths = append(paths, found{m[2], text[m[2]:m[3]] + pathRest.FindString(text[m[3]:])})
		}
	}
	sort.SliceStable(paths, func(i, j int) bool { return paths[i].at < paths[j].at })
	for _, p := range paths {
		add(LeakPath, p.match)
	}
	if host := strings.TrimSpace(c.Hostname); host != "" && !strings.EqualFold(host, "localhost") {
		re := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(host) + `)(?:$|[^A-Za-z0-9_-])`)
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			add(LeakHostname, text[m[2]:m[3]])
		}
	}
	for _, p := range idPatterns {
		for _, m := range p.re.FindAllString(text, -1) {
			add(p.kind, m)
		}
	}
	// The whole text first, as the policy reads it (a multi-line JSON string or a run of assignments is judged over several lines),
	// then each line alone for the other lines.
	if line, secret := publishpolicy.RawSecretLine(text); secret {
		add(LeakSecret, fmt.Sprintf("line %d", line))
	}
	// A fenced block is judged as its own text: a JSON block in a body is not valid JSON as part of the whole.
	for _, m := range fencePattern.FindAllStringSubmatchIndex(text, -1) {
		first := strings.Count(text[:m[2]], "\n")
		if line, secret := publishpolicy.RawSecretLine(text[m[2]:m[3]]); secret {
			add(LeakSecret, fmt.Sprintf("line %d", first+line))
		}
	}
	for i, line := range strings.Split(text, "\n") {
		if _, secret := publishpolicy.RawSecretLine(line); secret {
			add(LeakSecret, fmt.Sprintf("line %d", i+1))
		}
	}
	return out
}
