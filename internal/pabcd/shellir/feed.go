package shellir

import (
	"path"
	"regexp"
	"strings"
)

// Feed says where the operands of a program come from when find, xargs or an outer wrapper runs it. The operands arrive at
// run time, so a consumer that judges a removal reads this record instead of the program's own words: find names its start
// points and the tests that stand before the action; xargs names what its standard input carries, or says the reader cannot
// read it.
type Feed struct {
	// Wrapper is "find" or "xargs".
	Wrapper string
	// Starts are the start points of the find that runs the program; a start that is not Known is read at run time
	// (-files0-from). GuardedFor says whether the tests on every path that reaches the action leave a start point itself out.
	Starts []Word
	chains []findChain
	// Items are the operands xargs builds from the names its standard input carries (Named); Named false with no Unread is a
	// program whose output the text does not name (ls, git ls-files). Unread, when not empty, says the input cannot be read at all
	// (a file, an unknown producer, a program that rewrites a known list). Replace is the -I string; Replaced says the command
	// the feed belongs to is the one the reader made from the template by putting Items[0] in place of it.
	Items    []string
	Named    bool
	Unread   string
	Replace  string
	Replaced bool
	// Carried: the feed belongs to a wrapper outside the shell text this program stands in, so the program receives its operands
	// only through the shell's positional parameters.
	Carried bool
	// Outer is the feed of the wrapper that ran this wrapper (xargs started by find -exec).
	Outer *Feed
}

// GuardedFor says whether a test stands before the action on every path that reaches it and leaves the start point out.
func (f *Feed) GuardedFor(start string) bool { return chainsGuard(f.chains, start) }

func (f *Feed) asCarried() *Feed {
	if f == nil {
		return nil
	}
	c := *f
	c.Carried = true
	c.Outer = f.Outer.asCarried()
	return &c
}

// pipeSource is what the left side of a pipe prints, as far as the reader proves it. outs are the exact texts it may print (more
// than one when the shell decides how a program reads its words); sub says the output is some of the lines of those texts, in any
// order; unknown says it prints something the reader cannot show but must not trust. Neither set is a program whose output the
// text does not name (ls, git ls-files, a file read).
type pipeSource struct {
	outs    []string
	sub     bool
	unknown string
}

func (p *pipeSource) named() bool { return p != nil && (p.unknown != "" || len(p.outs) > 0) }

// FindAction is one action of a find expression: -delete, or -exec, -execdir, -ok, -okdir with the command it runs.
type FindAction struct {
	Name    string
	Command []Word
	chains  []findChain
}

// GuardedFor says whether a test stands before the action on every path that reaches it and leaves the start point out.
func (a FindAction) GuardedFor(start string) bool { return chainsGuard(a.chains, start) }

// findTest is a primary that tests a file, with its operand and whether a ! stands before it.
type findTest struct {
	name, operand string
	neg           bool
}

// findChain is the tests of one path through a find expression to an action.
type findChain []findTest

// findArgs is the number of operands a find primary takes; findTests are the primaries that test a file (an action is not a
// test, and neither is an option such as -maxdepth).
var findArgs = map[string]int{
	"-name": 1, "-iname": 1, "-path": 1, "-ipath": 1, "-wholename": 1, "-iwholename": 1, "-regex": 1, "-iregex": 1,
	"-lname": 1, "-ilname": 1, "-type": 1, "-xtype": 1, "-newer": 1, "-anewer": 1, "-cnewer": 1, "-mtime": 1, "-atime": 1,
	"-ctime": 1, "-mmin": 1, "-amin": 1, "-cmin": 1, "-size": 1, "-user": 1, "-group": 1, "-uid": 1, "-gid": 1, "-perm": 1,
	"-links": 1, "-inum": 1, "-samefile": 1, "-fstype": 1, "-used": 1, "-context": 1, "-maxdepth": 1, "-mindepth": 1,
	"-regextype": 1, "-printf": 1, "-fprint": 1, "-fprint0": 1, "-fls": 1, "-fprintf": 2, "-files0-from": 1,
	"-empty": 0, "-nouser": 0, "-nogroup": 0, "-readable": 0, "-writable": 0, "-executable": 0,
}

var findTests = map[string]bool{
	"-name": true, "-iname": true, "-path": true, "-ipath": true, "-wholename": true, "-iwholename": true, "-regex": true,
	"-iregex": true, "-lname": true, "-ilname": true, "-type": true, "-xtype": true, "-newer": true, "-anewer": true,
	"-cnewer": true, "-mtime": true, "-atime": true, "-ctime": true, "-mmin": true, "-amin": true, "-cmin": true,
	"-size": true, "-user": true, "-group": true, "-uid": true, "-gid": true, "-perm": true, "-links": true, "-inum": true,
	"-samefile": true, "-fstype": true, "-used": true, "-context": true, "-empty": true, "-nouser": true, "-nogroup": true,
	"-readable": true, "-writable": true, "-executable": true,
}

var findNewerXY = regexp.MustCompile(`^-newer[acBmt][acBmt]$`)

func findIsAction(v string) bool {
	switch v {
	case "-delete", "-exec", "-execdir", "-ok", "-okdir":
		return true
	}
	return false
}

// findStartsUnknown is the start point of a find that reads its start points from a file at run time (-files0-from): they can be
// any path, the slot and its ancestors among them.
var findStartsUnknown = Word{Reason: "find -files0-from reads its start points at run time"}

func cloneChains(c []findChain) []findChain {
	out := make([]findChain, len(c))
	for i, ch := range c {
		out[i] = append(findChain(nil), ch...)
	}
	return out
}

// FindScan reads the operands of find: its start points and its actions in the order they stand, each with the tests that stand
// before it on every path that reaches it. The expression grammar is read as find reads it: -o and , start an alternative whose
// tests are its own, a parenthesised group adds the tests of each of its alternatives to the paths that reach what follows (and
// what stands in the group is reached through the tests before it), a ! before a group drops the tests of the group, and the
// operand of a primary (-name -delete) is not an action. A find -exec without a terminator or a program is an error, as the
// reader refuses it.
func FindScan(args []Word) (starts []Word, actions []FindAction, err error) {
	for _, a := range args {
		if !a.Known {
			return nil, nil, unreadablef("find argument is not known (%s)", a.Reason)
		}
	}
	n := len(args)
	i := 0
head:
	for i < n {
		switch v := args[i].Value; {
		case v == "-H" || v == "-L" || v == "-P":
			i++
		case v == "-D":
			i += 2
		case len(v) > 2 && strings.HasPrefix(v, "-O") && isDigits(v[2:]):
			i++
		default:
			break head
		}
	}
	for i < n {
		v := args[i].Value
		if v == "-f" && i+1 < n {
			starts = append(starts, args[i+1])
			i += 2
			continue
		}
		if (strings.HasPrefix(v, "-") && len(v) > 1) || v == "(" || v == ")" || v == "!" || v == "," {
			break
		}
		starts = append(starts, args[i])
		i++
	}
	// A frame is a parenthesised group (the first is the whole expression): base are the paths that reach its first primary,
	// paths the paths that reach the point being read, closed the paths of the alternatives it has finished.
	type frame struct {
		base, paths, closed []findChain
		groupNeg            bool
	}
	stack := []frame{{base: []findChain{nil}, paths: []findChain{nil}}}
	neg := false
	for i < n {
		v := args[i].Value
		top := &stack[len(stack)-1]
		switch {
		case v == "!" || v == "-not":
			neg = !neg
			i++
		case v == "(":
			stack = append(stack, frame{base: cloneChains(top.paths), paths: cloneChains(top.paths), groupNeg: neg})
			neg = false
			i++
		case v == ")":
			if len(stack) > 1 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				res := append(cloneChains(f.closed), cloneChains(f.paths)...)
				if f.groupNeg {
					res = cloneChains(f.base)
				}
				stack[len(stack)-1].paths = res
			}
			neg = false
			i++
		case v == "-o" || v == "-or" || v == ",":
			top.closed = append(top.closed, cloneChains(top.paths)...)
			top.paths = cloneChains(top.base)
			neg = false
			i++
		case v == "-delete":
			actions = append(actions, FindAction{Name: v, chains: cloneChains(top.paths)})
			neg = false
			i++
		case v == "-exec" || v == "-execdir" || v == "-ok" || v == "-okdir":
			j := i + 1
			for j < n && args[j].Value != ";" && args[j].Value != "+" {
				j++
			}
			if j >= n {
				return nil, nil, unreadablef("find %s without a terminator", v)
			}
			if j == i+1 {
				return nil, nil, unreadablef("find %s without a program", v)
			}
			actions = append(actions, FindAction{Name: v, Command: args[i+1 : j], chains: cloneChains(top.paths)})
			neg = false
			i = j + 1
		default:
			k := findArgs[v]
			if findNewerXY.MatchString(v) {
				k = 1
			}
			operand := ""
			if k > 0 && i+1 < n && !findIsAction(args[i+1].Value) {
				operand = args[i+1].Value
			}
			if v == "-files0-from" {
				starts = append(starts, findStartsUnknown)
			}
			if findTests[v] || findNewerXY.MatchString(v) {
				t := findTest{name: v, operand: operand, neg: neg}
				for c := range top.paths {
					top.paths[c] = append(top.paths[c], t)
				}
			}
			neg = false
			i++
			// The operand of a primary is skipped, unless it is itself an action: reading that as an action only refuses more.
			for ; k > 0 && i < n && !findIsAction(args[i].Value); k-- {
				i++
			}
		}
	}
	return starts, actions, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// chainsGuard says whether every path that reaches an action holds a test that leaves the start point out. A path with no test
// does not, and neither does a test that matches the start point (-name '*', -path '*', -regex '.*', a pattern that fits the
// start point's own name).
func chainsGuard(chains []findChain, start string) bool {
	if len(chains) == 0 {
		return false
	}
	for _, ch := range chains {
		ok := false
		for _, t := range ch {
			if t.excludes(start) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// excludes says whether the test is false for the start point. A test the reader cannot evaluate against a directory's name (-newer,
// -mtime, -size, -empty, -type, a regular expression with groups) is taken as one that selects some files and leaves the start
// point out, as is a negated test;
// the name and path patterns and the regular expression are evaluated against the start point as find prints it, so -name '*' or
// a pattern that fits the start point's own name does not leave it out.
func (t findTest) excludes(start string) bool {
	if t.neg {
		// A negated test keeps what it names out of the action (find . ! -name keep): it is read as one that selects.
		return true
	}
	var matches, known bool
	switch t.name {
	case "-name", "-iname":
		p, s := t.operand, findBase(start)
		if t.name == "-iname" {
			p, s = strings.ToLower(p), strings.ToLower(s)
		}
		matches, known = globMatch(p, s)
	case "-path", "-ipath", "-wholename", "-iwholename":
		p, s := t.operand, start
		if t.name == "-ipath" || t.name == "-iwholename" {
			p, s = strings.ToLower(p), strings.ToLower(s)
		}
		matches, known = globMatch(p, s)
	case "-regex", "-iregex":
		matches, known = regexMatch(t.operand, start, t.name == "-iregex")
	default:
		return true
	}
	if !known {
		return true
	}
	return !matches
}

// findBase is the name find matches -name against for a start point: its last component as written.
func findBase(start string) string {
	s := strings.TrimRight(start, "/")
	if s == "" {
		return "/"
	}
	return s[strings.LastIndexByte(s, '/')+1:]
}

// globMatch matches a find pattern (fnmatch without FNM_PATHNAME or FNM_PERIOD: * and ? match / and a leading dot too).
func globMatch(pattern, s string) (matches, known bool) {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			} else {
				b.WriteString(`\\`)
			}
		case '[':
			end, class, ok := globBracket(pattern, i)
			if !ok {
				b.WriteString(`\[`)
				break
			}
			if class == "" {
				return false, false
			}
			b.WriteString(class)
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false, false
	}
	return re.MatchString(s), true
}

// posixClasses are the character classes a find pattern bracket may name ([[:alpha:]]); Go's regular expressions spell them the same way.
var posixClasses = map[string]bool{
	"alpha": true, "digit": true, "alnum": true, "upper": true, "lower": true, "space": true, "punct": true, "print": true,
	"graph": true, "cntrl": true, "xdigit": true, "blank": true,
}

// globBracket reads the bracket expression that starts at pattern[i] ('['): end is the index of its closing ']' and class its
// regular expression. A POSIX class inside it ([:alpha:]) does not end it at its own ']'. ok is false when no ']' closes the bracket
// (the '[' is then a plain character); class is "" for a bracket the reader does not evaluate (a collating symbol or an
// equivalence class, an unknown class name).
func globBracket(pattern string, i int) (end int, class string, ok bool) {
	j := i + 1
	neg := ""
	if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
		neg = "^"
		j++
	}
	var body strings.Builder
	first := true
	for j < len(pattern) {
		c := pattern[j]
		switch {
		case c == ']' && !first:
			return j, "[" + neg + body.String() + "]", true
		case c == '[' && j+1 < len(pattern) && (pattern[j+1] == ':' || pattern[j+1] == '.' || pattern[j+1] == '='):
			delim := pattern[j+1]
			k := strings.Index(pattern[j+2:], string(delim)+"]")
			if k < 0 {
				body.WriteString(`\[`)
				j++
				break
			}
			name := pattern[j+2 : j+2+k]
			j += k + 4
			if delim != ':' || !posixClasses[name] {
				// A collating symbol, an equivalence class or an unknown class: find may or may not match it, so the bracket is not read.
				return j - 1, "", true
			}
			body.WriteString("[:" + name + ":]")
		case c == '\\':
			if j+1 < len(pattern) {
				body.WriteString(regexp.QuoteMeta(pattern[j+1 : j+2]))
				j += 2
			} else {
				body.WriteString(`\\`)
				j++
			}
		case c == '[':
			body.WriteString(`\[`)
			j++
		default:
			body.WriteByte(c)
			j++
		}
		first = false
	}
	return 0, "", false
}

// regexMatch matches a find -regex pattern (a regular expression that must match the whole path). Only the part both dialects
// share is evaluated; a pattern with grouping, alternation or repetition operators is not (known is false), unless it is made of
// nothing else, which fits every path.
func regexMatch(pattern, s string, fold bool) (matches, known bool) {
	if strings.Trim(pattern, ".*()^$|+?\\") == "" {
		// Dots, stars, groups and anchors only (.*, (.*), ^.*$, .+): the pattern fits every path.
		return true, true
	}
	if strings.ContainsAny(pattern, "(){}|+?") {
		return false, false
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '\\' {
			if i+1 >= len(pattern) || isAlnum(pattern[i+1]) {
				return false, false
			}
			i++
		}
	}
	p := "^(?s:" + strings.TrimSuffix(strings.TrimPrefix(pattern, "^"), "$") + ")$"
	if fold {
		p = "(?i)" + p
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return false, false
	}
	return re.MatchString(s), true
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// transparentWrapper names the wrappers that hand their standard input and their operands on unchanged.
func transparentWrapper(name string) bool {
	switch name {
	case "env", "command", "builtin", "exec", "nohup", "nice", "ionice", "timeout", "time", "stdbuf", "setsid", "sudo", "doas":
		return true
	}
	return false
}

// filterSpec is the option grammar of a program that passes the lines of its standard input on: flags are options without a value,
// valued the ones with one. identity says the output is the input; otherwise it is some of the input's lines, in any order. A
// program or an option outside the grammar (head -c, grep -o, sort -z) changes the text and makes the list unknown.
type filterSpec struct {
	flags, valued string
	numeric       bool // head -5
	identity      bool
}

var filterSpecs = map[string]filterSpec{
	"cat":   {flags: "AbeEnstTuv", identity: true},
	"tee":   {flags: "aip", identity: true},
	"sort":  {flags: "rnufbdsiVhgM"},
	"uniq":  {flags: "cdiuD", valued: "fsw"},
	"head":  {flags: "qv", valued: "n", numeric: true},
	"tail":  {flags: "qv", valued: "n", numeric: true},
	"tac":   {},
	"grep":  {flags: "iEFGPvxwsqhHnbIaUy", valued: "emfABC"},
	"egrep": {flags: "iEFGPvxwsqhHnbIaUy", valued: "emfABC"},
	"fgrep": {flags: "iEFGPvxwsqhHnbIaUy", valued: "emfABC"},
}

// filterPasses says whether a program with these options passes the lines of its input on. A file operand makes it read that file
// instead of standard input (cat roots.list, sort file, grep pattern file), so the lines it prints are not the producer's.
func filterPasses(name string, args []Word) (spec filterSpec, ok bool) {
	spec, ok = filterSpecs[name]
	if !ok {
		return spec, false
	}
	var operands []string
	patternGiven := false
	for i := 0; i < len(args); i++ {
		if !args[i].Known {
			return spec, false
		}
		v := args[i].Value
		if v == "--" {
			for _, a := range args[i+1:] {
				if !a.Known {
					return spec, false
				}
				operands = append(operands, a.Value)
			}
			break
		}
		if len(v) < 2 || v[0] != '-' {
			operands = append(operands, v)
			continue
		}
		if strings.HasPrefix(v, "--") {
			return spec, false
		}
		if spec.numeric && isDigits(v[1:]) {
			continue
		}
		for k := 1; k < len(v); k++ {
			c := v[k]
			switch {
			case strings.IndexByte(spec.valued, c) >= 0:
				if c == 'e' || c == 'f' {
					patternGiven = true
				}
				if k == len(v)-1 {
					i++
				}
				k = len(v)
			case strings.IndexByte(spec.flags, c) >= 0:
			default:
				return spec, false
			}
		}
	}
	if name == "tee" {
		// The operands of tee are files it writes; its output is its input.
		return spec, true
	}
	if strings.HasSuffix(name, "grep") && !patternGiven && len(operands) > 0 {
		// The first operand of grep is the pattern.
		operands = operands[1:]
	}
	for _, o := range operands {
		if o != "-" {
			return spec, false
		}
	}
	return spec, true
}

// stdinBody is the here-document or here-string body a command reads, from its own redirections.
func stdinBody(redirs []Redir) (Word, bool) {
	for i := len(redirs) - 1; i >= 0; i-- {
		r := redirs[i]
		if r.Fd != "" && r.Fd != "0" {
			continue
		}
		if r.Op == "<<" || r.Op == "<<-" || r.Op == "<<<" {
			return r.Target, true
		}
		if r.Op == "<" || r.Op == "<>" || r.Op == "<&" {
			return Word{}, false
		}
	}
	return Word{}, false
}

// stdinSource is what a command reads on standard input, from the standard input kind and the source the context carries.
func stdinSource(ctx Context, redirs []Redir) *pipeSource {
	switch ctx.Stdin {
	case StdinNone, StdinFile:
		return nil
	case StdinPipe:
		if ctx.pipeSrc == nil {
			return &pipeSource{unknown: "a pipe the reader cannot trace"}
		}
		return ctx.pipeSrc
	case StdinHeredoc, StdinHerestring:
		body, ok := stdinBody(redirs)
		if !ok {
			return &pipeSource{unknown: "standard input from an outer redirection"}
		}
		if !body.Known {
			return &pipeSource{unknown: "a here-document the reader cannot read"}
		}
		return &pipeSource{outs: []string{body.Value}}
	}
	return &pipeSource{unknown: "standard input from a descriptor the reader cannot trace"}
}

// pathPrinters are the programs whose output is a path they compute from the place they run or from their operands.
func pathPrinter(p Exec, name string) bool {
	switch name {
	case "pwd", "dirname", "basename", "realpath", "readlink", "namei", "stat", "du", "file":
		return true
	case "git":
		for i := 0; i < len(p.Args); i++ {
			v := p.Args[i].Value
			switch {
			case v == "-C" || v == "-c":
				i++
			case strings.HasPrefix(v, "-"):
			default:
				return v == "rev-parse" || v == "worktree"
			}
		}
	}
	return false
}

// producerSource is what the left side of a pipe prints. execs are the programs the left statement showed, outside any
// substitution. A simple echo or printf prints what it evaluates to; a filter that passes lines on prints some of its own input; a
// find prints its start points; ls -d prints its operands; any other single program prints names the text does not give; a program
// that rewrites a known list, a compound command or a chain that runs further programs is not traced.
func producerSource(execs []Exec) *pipeSource {
	if len(execs) == 0 {
		return &pipeSource{}
	}
	i := 0
	for i < len(execs)-1 && transparentWrapper(execs[i].Name) {
		i++
	}
	if len(execs)-i != 1 {
		return &pipeSource{unknown: "a program that runs further programs feeds the pipe"}
	}
	p := execs[i]
	if p.Kind != KindCommand {
		return &pipeSource{unknown: "a script feeds the pipe"}
	}
	name := path.Base(p.Name)
	switch {
	case name == "echo":
		outs, why := echoOutputs(p.Args)
		if why != "" {
			return &pipeSource{unknown: why}
		}
		return &pipeSource{outs: outs}
	case name == "printf":
		out, why := printfOutput(p.Args)
		if why != "" {
			return &pipeSource{unknown: why}
		}
		return &pipeSource{outs: []string{out}}
	case name == "find":
		return findSource(p)
	case name == "pwd" && p.Dir.Known:
		return &pipeSource{outs: []string{p.Dir.Path + "\n"}}
	case pathPrinter(p, name):
		return &pipeSource{unknown: "a program that prints a path feeds the pipe"}
	case name == "yes" && !hasUnknown(p.Args):
		var words []string
		for _, a := range p.Args {
			words = append(words, a.Value)
		}
		return &pipeSource{outs: []string{strings.Join(words, " ") + "\n"}, sub: true}
	case programEmitter(name) || p.Inline != nil:
		return &pipeSource{unknown: "a program that prints what its own text says feeds the pipe"}
	case name == "ls" && lsDirectory(p.Args):
		var lines []string
		for _, a := range p.Args {
			if !a.Known {
				return &pipeSource{unknown: "ls -d prints a word the reader does not know"}
			}
			if !strings.HasPrefix(a.Value, "-") {
				lines = append(lines, a.Value)
			}
		}
		return &pipeSource{outs: []string{strings.Join(lines, "\n") + "\n"}}
	}
	in := stdinSource(p.Ctx, p.Redirs)
	if spec, ok := filterPasses(name, p.Args); ok {
		if in == nil {
			return &pipeSource{}
		}
		if in.unknown != "" || spec.identity {
			return in
		}
		c := *in
		c.sub = true
		return &c
	}
	if in.named() {
		return &pipeSource{unknown: "a program rewrites the list that feeds the pipe"}
	}
	return &pipeSource{}
}

func hasUnknown(args []Word) bool {
	for _, a := range args {
		if !a.Known {
			return true
		}
	}
	return false
}

// programEmitter names the programs that print what a program text or a command line of their own says: a script they run, a
// command they start.
func programEmitter(name string) bool {
	switch name {
	case "awk", "gawk", "mawk", "sed", "perl", "python", "python2", "python3", "ruby", "node", "nodejs", "lua", "php", "tclsh",
		"bash", "sh", "dash", "zsh", "ksh", "xargs", "parallel", "eval", "source", ".":
		return true
	}
	return false
}

func lsDirectory(args []Word) bool {
	for _, a := range args {
		if a.Known && (a.Value == "--directory" || len(a.Value) > 1 && a.Value[0] == '-' && a.Value[1] != '-' && strings.Contains(a.Value, "d")) {
			return true
		}
	}
	return false
}

// findSource is what a find prints: the start points it names, one per line (-print, -print0 or nothing). A find that prints
// another way (-printf, -ls, -exec) or reads its starts from a file prints names the reader cannot show.
func findSource(p Exec) *pipeSource {
	starts, actions, err := FindScan(p.Args)
	if err != nil {
		return &pipeSource{unknown: "a find the reader cannot read feeds the pipe"}
	}
	if len(actions) > 0 {
		return &pipeSource{unknown: "a find that runs or deletes feeds the pipe"}
	}
	for _, a := range p.Args {
		switch a.Value {
		case "-printf", "-ls", "-fprint", "-fprint0", "-fprintf", "-fls":
			return &pipeSource{unknown: "a find that formats its output feeds the pipe"}
		}
	}
	var lines []string
	for _, s := range starts {
		if !s.Known {
			return &pipeSource{unknown: "a find that reads its start points from a file feeds the pipe"}
		}
		lines = append(lines, s.Value)
	}
	if len(lines) == 0 {
		lines = []string{"."}
	}
	return &pipeSource{outs: []string{strings.Join(lines, "\n") + "\n"}}
}

// xargsFeed is the feed of the program xargs runs: what its standard input carries.
func xargsFeed(ctx Context, redirs []Redir, o xargsOpts) *Feed {
	f := &Feed{Wrapper: "xargs", Outer: ctx.Feed, Replace: o.Replace}
	if o.ArgFile {
		f.Unread = "xargs -a reads its operands from a file"
		return f
	}
	switch ctx.Stdin {
	case StdinNone:
	case StdinFile:
		if ctx.stdinFile != "/dev/null" {
			f.Unread = "xargs reads its operands from a file"
		}
	default:
		src := stdinSource(ctx, redirs)
		switch {
		case src == nil:
		case src.unknown != "":
			f.Unread = src.unknown
		case len(src.outs) > 0:
			items, why := xargsItems(src, o)
			if why != "" {
				f.Unread = why
			} else {
				f.Items, f.Named = items, true
			}
		}
	}
	return f
}

// xargsPrograms sets the feed of each program xargs runs. With -I and names the reader proves, the template is read once for each
// name, with the replace string in its words replaced by the name (the text of a shell the template runs included), so every
// program the consumer judges is the one that runs; when the names are not known, the template stands and the feed says the
// replace string is unresolved.
func xargsPrograms(u *unwrapped, ctx Context, redirs []Redir) error {
	base := xargsFeed(ctx, redirs, u.xopts)
	if u.xopts.Replace != "" && len(u.inner) == 1 {
		switch {
		case base.Named && base.Unread == "":
			if len(base.Items) > 64 {
				base.Items, base.Named, base.Unread = nil, false, "the names xargs reads are too many to judge"
				break
			}
			inners := make([][]Word, 0, len(base.Items))
			feeds := make([]*Feed, 0, len(base.Items))
			for _, item := range base.Items {
				f := *base
				f.Items, f.Replaced = []string{item}, true
				inners = append(inners, replaceWords(u.inner[0], u.xopts.Replace, item))
				feeds = append(feeds, &f)
			}
			u.inner, u.feeds = inners, feeds
			return nil
		case strings.Contains(u.inner[0][0].Value, u.xopts.Replace) && u.inner[0][0].Known:
			return unreadablef("xargs -I names the program from the input")
		}
	}
	u.feeds = make([]*Feed, len(u.inner))
	for i := range u.feeds {
		u.feeds[i] = base
	}
	return nil
}

func replaceWords(words []Word, repl, item string) []Word {
	out := make([]Word, len(words))
	for i, w := range words {
		if w.Known {
			w.Value = strings.ReplaceAll(w.Value, repl, item)
		}
		out[i] = w
	}
	return out
}
