// CRW-1549: the publication scrub. Everything written to a public forge (issue and milestone text,
// comments, pull request text, commit messages) passes one function: Scrub replaces local paths,
// the host's names and private addresses, thread, call, run and session ids and e-mail addresses,
// and refuses the text entirely when a token or secret shape is in it.

package publishpolicy

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// ErrSecret is what a publisher returns when Scrub blocks the text: a token or secret shape was in
// it. Nothing is published; a public answer cannot be taken back.
var ErrSecret = errors.New("public_text_secret: token or secret pattern; nothing published")

// Context is what is local to the host a text is published from: path roots whose appearance names
// this machine (the user's home, the state and work roots), the host's names and identifiers a
// session would carry. HostContext is the default; a policy adds literals.
type Context struct {
	Paths       []string
	Hostnames   []string
	Identifiers []string
}

// Policy is the per-repository publication setting. Disabled skips the privacy replacements only:
// a token or secret shape still blocks the text. Redact nil is every kind; a non-nil list is exactly
// those kinds (an explicit empty list turns every privacy replacement off). Paths, Hostnames and
// Identifiers are extra literals the repository names.
type Policy struct {
	Disabled    bool     `json:"disabled,omitempty"`
	Redact      []string `json:"redact,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	Hostnames   []string `json:"hostnames,omitempty"`
	Identifiers []string `json:"identifiers,omitempty"`
}

// kinds, in the order a Change lists them and in the order the kinds win an overlap (a path span
// covering an id span is reported as a path).
var kinds = []string{"path", "hostname", "ip", "thread_id", "call_id", "run_id", "session_id", "email"}

var kindSet = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}()

// Validate judges a policy: every redact kind is a known kind, no literal is blank, and every path
// root is absolute ("/" itself, which would cover everything, is refused).
func (p Policy) Validate() error {
	for _, k := range p.Redact {
		if !kindSet[k] {
			return fmt.Errorf("publication: redact kind %q is not one of %s", k, strings.Join(kinds, ", "))
		}
	}
	for _, v := range p.Paths {
		if strings.TrimSpace(v) == "" {
			return errors.New("publication: a paths entry is blank")
		}
		if !isAbsRoot(v) {
			return fmt.Errorf("publication: path %q is not absolute", v)
		}
		if v == "/" {
			return errors.New(`publication: "/" as a path root covers everything`)
		}
	}
	for _, v := range p.Hostnames {
		if strings.TrimSpace(v) == "" {
			return errors.New("publication: a hostnames entry is blank")
		}
	}
	for _, v := range p.Identifiers {
		if strings.TrimSpace(v) == "" {
			return errors.New("publication: an identifiers entry is blank")
		}
	}
	return nil
}

// isAbsRoot is whether v is an absolute path root: /rooted or a Windows drive root.
func isAbsRoot(v string) bool {
	if strings.HasPrefix(v, "/") {
		return true
	}
	return len(v) >= 3 && isDrive(v[0]) && (v[1] == ':') && (v[2] == '/' || v[2] == '\\')
}

func isDrive(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Change counts the spans of one kind the scrub replaced.
type Change struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Result is the scrub of one text: Text is publishable only when Blocked is false (it is still a
// diagnostic for a blocked text — a token is already gone from it, a remaining secret collapses the
// whole text to "[secret]"). Changes are the privacy replacements made, one entry per kind.
type Result struct {
	Text    string   `json:"text"`
	Changes []Change `json:"changes"`
	Blocked bool     `json:"blocked"`
}

// Event is the audit one published field leaves: where it went and the kinds and counts of its
// replacements, never the text or a match.
type Event struct {
	Repo    string   `json:"repo"`
	Surface string   `json:"surface"`
	Field   string   `json:"field"`
	Changes []Change `json:"changes"`
	Blocked bool     `json:"blocked"`
}

// HostContext is the Context of this host: the user's home, temporary directory and configured state and work roots
// (XDG_STATE_HOME, CRW_HOME, CRW_STATE, CRW_WORKTREE_ROOT, each only when absolute and non-empty) as
// path roots, and the machine's name.
func HostContext() Context {
	c := Context{}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		c.Paths = append(c.Paths, home)
	}
	if wd, err := os.Getwd(); err == nil && wd != "/" {
		c.Paths = append(c.Paths, wd)
	}
	if temp, err := tmploc.Dir(); err == nil && temp != "/" {
		c.Paths = append(c.Paths, temp)
	}
	for _, env := range []string{"XDG_STATE_HOME", "CRW_HOME", "CRW_STATE", "CRW_WORKTREE_ROOT"} {
		if v := os.Getenv(env); v != "" && isAbsRoot(v) {
			c.Paths = append(c.Paths, v)
		}
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		c.Hostnames = append(c.Hostnames, host)
	}
	return c
}

// pathLeft and pathRight are the boundaries of a path root, shared by the fixed roots and the
// configured ones (the semantics of the pull request leak check: `docs/state/x.md`, `a/home/b` and
// `https://example.com/home/x` are relative and stay).
const (
	pathLeft   = "(?m)(?:^|[^A-Za-z0-9_.\\-])"
	pathRight  = "(?:/|$|[\\s`'\"<>()\\]>,;:*])"
	winRight   = "(?:[\\\\/]|$|[\\s`'\"<>()\\]>,;:*])"
	tokenBreak = "[^\\s`'\"<>()\\],;:*]*"
)

// pathRest is the rest of a path token after its root.
var pathRest = regexp.MustCompile("^" + tokenBreak)

// unixRoots is the fixed set of private path roots: the user homes of Linux and macOS, the relay's
// state and scratch roots, the temporary directories and the ~ spelling of a home.
var unixRoots = regexp.MustCompile(pathLeft + "(/(?:var/tmp|home|Users|state|scratch|tmp)|~)" + pathRight)

// winRoots is the fixed Windows user root (a supplied drive root is a Policy path).
var winRoots = regexp.MustCompile(pathLeft + `([A-Za-z]:[\\/]Users)` + winRight)

// fencePattern is the inside of a fenced code block.
var fencePattern = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```")

// idPatterns are the identifier shapes, in kind order (a UUID is a thread id).
var idPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"thread_id", regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)},
	{"thread_id", regexp.MustCompile(`\bthread[-_][A-Za-z0-9_-]{8,}\b`)},
	{"call_id", regexp.MustCompile(`\bc_[0-9a-f]{12}\b`)},
	{"call_id", regexp.MustCompile(`\bcall_[A-Za-z0-9]{16,}\b`)},
	{"call_id", regexp.MustCompile(`\b(?:fc|toolu)_[A-Za-z0-9]{16,}\b`)},
	{"call_id", regexp.MustCompile(`(?:crw-flow-op:|crwh1:)[^\s` + "`" + `'"),\]}>:;]+`)},
	{"run_id", regexp.MustCompile(`\brun-[0-9a-f]{12}\b`)},
	{"run_id", regexp.MustCompile(`\b(?:req|rep)_[0-9a-f]{24}\b`)},
	{"session_id", regexp.MustCompile(`\bsession[-_][A-Za-z0-9_-]{8,}\b`)},
}

// Public source filenames and issue references have identities that publication must preserve.
// These spans only exempt the automatic ID heuristics; explicit identifiers and other categories still apply.
const referenceLeft = `(?:^|[^A-Za-z0-9_./\\-])`

var referencePatterns = []*regexp.Regexp{
	// a trailing period is a sentence end (markdown punctuation), not part of the name; a dot
	// followed by another name character is still the middle of a dotted name.
	regexp.MustCompile(referenceLeft + `((?:[A-Za-z0-9_.-]+[/\\])*[A-Za-z0-9_.-]+\.[A-Za-z][A-Za-z0-9]*)(?:$|\.(?:$|[^A-Za-z0-9_])|[^A-Za-z0-9_./\\-])`),
	regexp.MustCompile(referenceLeft + `([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[1-9][0-9]*)(?:$|[^A-Za-z0-9_])`),
}

func referenceSpans(text string) [][2]int {
	var out [][2]int
	for _, re := range referencePatterns {
		eachGroup1(re, text, func(start, end int) { out = append(out, [2]int{start, end}) })
	}
	return out
}

var (
	ipv4Candidate = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Candidate = regexp.MustCompile(`(?:^|[^\w:])([0-9A-Fa-f:.]*:[0-9A-Fa-f:.]+(?:%[A-Za-z0-9_.-]+)?)`)
	emailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// privateKeyHeader is a PEM private-key header; its body lines are not a token pattern, so the
// whole block collapses the text, not just the header.
var privateKeyHeader = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// namedCredential is a JSON/YAML-shaped credential assignment ("token": v, token: v) and
// bearerCredential an HTTP bearer token, both with a real value, not a placeholder.
var (
	namedCredential  = regexp.MustCompile(`(?im)(?:^|[\s{,])(?:["']?)(?:api[_-]?key|access[_-]?token|auth[_-]?token|authorization|client[_-]?secret|password|passwd|private[_-]?key|token|secret)(?:["']?)\s*:\s*[^\s,}]+`)
	bearerCredential = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{16,}={0,2}`)
)

// secretRemains is whether text still holds a credential shape (an assignment whose name marks a
// secret, a run of assignments, a JSON/YAML named credential, a bearer token, a JSON string or
// fenced block holding one, or a private-key header whose body the token pass cannot see) after the
// literal token shapes are out of it.
func secretRemains(original, safe string) bool {
	if privateKeyHeader.MatchString(original) || namedCredential.MatchString(safe) || bearerCredential.MatchString(safe) {
		return true
	}
	if _, found := RawSecretLine(safe); found {
		return true
	}
	for _, m := range fencePattern.FindAllStringSubmatchIndex(safe, -1) {
		if _, found := RawSecretLine(safe[m[2]:m[3]]); found {
			return true
		}
	}
	for _, line := range strings.Split(safe, "\n") {
		if _, found := RawSecretLine(line); found {
			return true
		}
	}
	return false
}

// on is whether kind is one the policy replaces: every kind when Redact is nil, else exactly the
// kinds Redact lists.
func (p Policy) on(kind string) bool {
	if p.Redact == nil {
		return true
	}
	for _, k := range p.Redact {
		if k == kind {
			return true
		}
	}
	return false
}

// aSpan is one matched span of text: its byte range, the priority of its kind and its replacement.
type aSpan struct {
	start, end int
	prio       int
	repl       string
}

// Scrub makes text safe to publish. The secret check reads the original first, so a token inside a
// path cannot hide under a path replacement: token shapes become "[token]" in the working text and
// any credential shape that remains after them blocks the text entirely (Text "[secret]"). The
// privacy spans are gathered over the working text by kind in priority order and merged, so a
// covered span is replaced once, and every identifier becomes a stable pseudonym rather than being
// dropped.
func Scrub(text string, c Context, p Policy) Result {
	out := Result{Text: text, Changes: []Change{}}
	tokenFound := false
	safe := text
	for _, re := range secretPatterns() {
		if re.MatchString(text) {
			tokenFound = true
		}
		safe = re.ReplaceAllString(safe, "[token]")
	}
	if tokenFound {
		out.Blocked = true
	}
	if secretRemains(text, safe) {
		out.Blocked = true
		out.Text = "[secret]"
		return out
	}
	if p.Disabled {
		out.Text = safe
		return out
	}
	spans := mergeSpans(gatherSpans(safe, c, p))
	out.Text = applySpans(safe, spans)
	count := map[string]int{}
	for _, s := range spans {
		count[kinds[s.prio]]++
	}
	for _, k := range kinds {
		if count[k] > 0 {
			out.Changes = append(out.Changes, Change{Kind: k, Count: count[k]})
		}
	}
	return out
}

// eachGroup1 calls fn once per match of re whose first capture group is a span of text. A match
// consumes its leading boundary but the trailing boundary is not part of the group, so the next
// search resumes at the group's end — two values back to back are both found.
func eachGroup1(re *regexp.Regexp, text string, fn func(start, end int)) {
	for off := 0; off < len(text); {
		m := re.FindStringSubmatchIndex(text[off:])
		if m == nil {
			return
		}
		fn(off+m[2], off+m[3])
		off += m[3]
	}
}

// gatherSpans is every privacy span of text, in kind order paths, hostnames, IPs, ids, e-mail.
func gatherSpans(text string, c Context, p Policy) []aSpan {
	var spans []aSpan
	if p.on("path") {
		for _, re := range []*regexp.Regexp{unixRoots, winRoots} {
			eachGroup1(re, text, func(start, end int) {
				end += len(pathRest.FindString(text[end:]))
				spans = append(spans, aSpan{start, end, 0, "[path]"})
			})
		}
		for _, root := range append(append([]string{}, c.Paths...), p.Paths...) {
			root = strings.TrimRight(strings.TrimSpace(root), "/\\")
			if root == "" {
				continue
			}
			right := pathRight
			if isDriveRoot(root) {
				right = winRight
			}
			re := regexp.MustCompile(pathLeft + "(" + regexp.QuoteMeta(root) + ")" + right)
			eachGroup1(re, text, func(start, end int) {
				end += len(pathRest.FindString(text[end:]))
				spans = append(spans, aSpan{start, end, 0, "[path]"})
			})
		}
	}
	if p.on("hostname") {
		for _, host := range append(append([]string{}, c.Hostnames...), p.Hostnames...) {
			host = strings.TrimSpace(host)
			if host == "" || strings.EqualFold(host, "localhost") {
				continue
			}
			re := regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(host) + `)(?:$|[^A-Za-z0-9_-])`)
			eachGroup1(re, text, func(start, end int) {
				spans = append(spans, aSpan{start, end, 1, "[hostname]"})
			})
		}
	}
	if p.on("ip") {
		for _, m := range ipv4Candidate.FindAllStringIndex(text, -1) {
			if privateIP(text[m[0]:m[1]]) {
				spans = append(spans, aSpan{m[0], m[1], 2, "[ip]"})
			}
		}
		for _, m := range ipv6Candidate.FindAllStringSubmatchIndex(text, -1) {
			// a trailing sentence period is not part of the address: shrink the candidate, not an
			// internal dot of a mapped IPv4
			end := m[3]
			for end > m[2] && text[end-1] == '.' {
				end--
			}
			if privateIP(text[m[2]:end]) {
				spans = append(spans, aSpan{m[2], end, 2, "[ip]"})
			}
		}
	}
	refs := referenceSpans(text)
	for _, ptn := range idPatterns {
		if !p.on(ptn.kind) {
			continue
		}
		for _, m := range ptn.re.FindAllStringIndex(text, -1) {
			protected := false
			for _, ref := range refs {
				if ref[0] <= m[0] && m[1] <= ref[1] {
					protected = true
					break
				}
			}
			if protected {
				continue
			}
			match := text[m[0]:m[1]]
			spans = append(spans, aSpan{m[0], m[1], idPrio(ptn.kind), pseudonym(ptn.kind, match)})
		}
	}
	if p.on("session_id") {
		for _, lit := range append(append([]string{}, c.Identifiers...), p.Identifiers...) {
			lit = strings.TrimSpace(lit)
			if lit == "" {
				continue
			}
			re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(lit) + `)(?:$|[^A-Za-z0-9_-])`)
			eachGroup1(re, text, func(start, end int) {
				spans = append(spans, aSpan{start, end, idPrio("session_id"), pseudonym("session_id", text[start:end])})
			})
		}
	}
	if p.on("email") {
		for _, m := range emailPattern.FindAllStringIndex(text, -1) {
			spans = append(spans, aSpan{m[0], m[1], 7, "[email]"})
		}
	}
	return spans
}

// idPrio is the priority of an id kind inside the ids group (thread_id < call_id < run_id < session_id).
func idPrio(kind string) int {
	switch kind {
	case "thread_id":
		return 3
	case "call_id":
		return 4
	case "run_id":
		return 5
	}
	return 6
}

// isDriveRoot is whether root is a Windows drive root like C:\ or C:/.
func isDriveRoot(root string) bool {
	return len(root) >= 2 && isDrive(root[0]) && root[1] == ':'
}

// privateIP is whether s parses as a private, loopback or link-local address, mapped forms unmapped
// first; anything that is not an address is ignored.
func privateIP(s string) bool {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()
}

// pseudonym is the stable stand-in for an identifier: the kind and the first 12 hex of its digest,
// so one identifier is one pseudonym and different ones do not collapse (a comment header named by
// a session id must still deduplicate against itself).
func pseudonym(kind, match string) string {
	sum := sha256.Sum256([]byte(match))
	return fmt.Sprintf("[%s:%x]", kind, sum[:6])
}

// mergeSpans is the covered regions: spans sorted by start (longest first, then earlier kind first
// on a tie), overlapping ones merged into one region that takes the replacement of the
// earliest-priority kind inside it.
func mergeSpans(spans []aSpan) []aSpan {
	if len(spans) == 0 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		if spans[i].end != spans[j].end {
			return spans[i].end > spans[j].end
		}
		return spans[i].prio < spans[j].prio
	})
	var merged []aSpan
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start < merged[n-1].end {
			top := &merged[n-1]
			if s.end > top.end {
				top.end = s.end
			}
			if s.prio < top.prio {
				top.prio, top.repl = s.prio, s.repl
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// applySpans splices the merged regions' replacements into the text in one pass.
func applySpans(text string, spans []aSpan) string {
	var out strings.Builder
	last := 0
	for _, s := range spans {
		out.WriteString(text[last:s.start])
		out.WriteString(s.repl)
		last = s.end
	}
	out.WriteString(text[last:])
	return out.String()
}
