package shellir

import (
	"regexp"
	"strings"
)

// Feed says where the operands of a program come from when find, xargs or an outer wrapper runs it. The operands arrive at
// run time, so a consumer that judges a removal reads this record instead of the program's own words: find names its start
// points and whether a test guards the action; xargs names what its standard input carries, or says the reader cannot read it.
type Feed struct {
	// Wrapper is "find" or "xargs".
	Wrapper string
	// Starts are the start points of the find that runs the program; Guarded says every path to the action passes a test
	// (-name, -type, -newer ...), so the action reaches the matches and not the start point itself.
	Starts  []Word
	Guarded bool
	// Names are the words the producer of xargs's standard input prints (echo, printf, a here-string or here-document body,
	// the start points of a find); a Name that is not Known cannot be read. Unread, when not empty, says the input cannot be
	// read at all (a file, an unknown producer, a program that rewrites a known list).
	Names  []Word
	Unread string
	// Outer is the feed of the wrapper that ran this wrapper (xargs started by find -exec).
	Outer *Feed
}

// pipeSource is what the left side of a pipe prints, as far as the reader proves it. names are the words it prints when they
// are named in the text; unknown says it prints something the reader cannot show but must not trust (a program that rewrites a
// known list); neither set is a program whose output the text does not name (ls, git ls-files, a file read).
type pipeSource struct {
	names   []Word
	unknown string
}

func (p *pipeSource) named() bool { return p != nil && (p.unknown != "" || len(p.names) > 0) }

// FindAction is one action of a find expression: -delete, or -exec, -execdir, -ok, -okdir with the command it runs.
type FindAction struct {
	Name    string
	Command []Word
	// Guarded: a test stands before the action on every path that reaches it.
	Guarded bool
}

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

// FindScan reads the operands of find: its start points and its actions in the order they stand, each with whether a test guards
// it. The expression grammar is read as find reads it: -o and , start an alternative whose tests are its own, a parenthesised
// group guards an action after it only when every alternative of the group holds a test, and the operand of a primary
// (-name -delete) is not an action. A find -exec without a terminator or a program is an error, as the reader refuses it.
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
	type frame struct{ cur, all bool }
	stack := []frame{{all: true}}
	for i < n {
		v := args[i].Value
		top := &stack[len(stack)-1]
		switch {
		case v == "(":
			stack = append(stack, frame{all: true})
			i++
		case v == ")":
			if len(stack) > 1 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if f.all && f.cur {
					stack[len(stack)-1].cur = true
				}
			}
			i++
		case v == "-o" || v == "-or" || v == ",":
			top.all = top.all && top.cur
			top.cur = false
			i++
		case v == "-delete":
			actions = append(actions, FindAction{Name: v, Guarded: top.cur})
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
			actions = append(actions, FindAction{Name: v, Command: args[i+1 : j], Guarded: top.cur})
			i = j + 1
		default:
			if findTests[v] || findNewerXY.MatchString(v) {
				top.cur = true
			}
			k := findArgs[v]
			if findNewerXY.MatchString(v) {
				k = 1
			}
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

// escapeRun is a backslash sequence a printf format or an echo -e string turns into a separator or a control byte.
var escapeRun = regexp.MustCompile(`\\(?:[0-7]{1,3}|[abefnrtv]|x[0-9a-fA-F]{1,2})`)

// FeedNames splits the words a producer prints into the names xargs reads from them: blanks and the escapes printf and echo -e
// turn into separators split them, and quoting around a name is dropped.
func FeedNames(word string) []string {
	word = escapeRun.ReplaceAllString(word, " ")
	var out []string
	for _, f := range strings.Fields(word) {
		f = strings.Trim(f, `'"`)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// transparentWrapper names the wrappers that hand their standard input and their operands on unchanged.
func transparentWrapper(name string) bool {
	switch name {
	case "env", "command", "builtin", "exec", "nohup", "nice", "ionice", "timeout", "time", "stdbuf", "setsid", "sudo", "doas":
		return true
	}
	return false
}

// passThroughFilter names the programs whose output is lines of their own input: a known list stays that list.
func passThroughFilter(name string) bool {
	switch name {
	case "cat", "tee", "sort", "uniq", "head", "tail", "tac", "grep", "egrep", "fgrep":
		return true
	}
	return false
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
		return &pipeSource{names: []Word{body}}
	}
	return &pipeSource{unknown: "standard input from a descriptor the reader cannot trace"}
}

// producerSource is what the left side of a pipe prints. execs are the programs the left statement showed, outside any
// substitution. A simple echo or printf prints its words; a filter that passes lines on prints its own input; a find prints its
// start points; any other single program prints names the text does not give; a program that rewrites a known list, a compound
// command or a chain that runs further programs is not traced.
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
	switch {
	case p.Kind != KindCommand:
		return &pipeSource{unknown: "a script feeds the pipe"}
	case p.Name == "echo" || p.Name == "printf":
		return &pipeSource{names: p.Args}
	case p.Name == "find":
		starts, _, err := FindScan(p.Args)
		if err != nil {
			return &pipeSource{unknown: "a find the reader cannot read feeds the pipe"}
		}
		return &pipeSource{names: starts}
	}
	in := stdinSource(p.Ctx, p.Redirs)
	if passThroughFilter(p.Name) {
		if in == nil {
			return &pipeSource{}
		}
		return in
	}
	if in.named() {
		return &pipeSource{unknown: "a program rewrites the list that feeds the pipe"}
	}
	return &pipeSource{}
}

// xargsFeed is the feed of the program xargs runs: what its standard input carries.
func xargsFeed(ctx Context, redirs []Redir, argFile bool) *Feed {
	f := &Feed{Wrapper: "xargs", Outer: ctx.Feed}
	if argFile {
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
		if src != nil {
			f.Names, f.Unread = src.names, src.unknown
		}
	}
	return f
}
