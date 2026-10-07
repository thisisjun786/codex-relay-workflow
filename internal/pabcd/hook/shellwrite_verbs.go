package hook

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// This file ports the per-verb part of shell-write-destinations.ts at CXC v0.2.40 (3c1459ac): basename, normalizeVerb,
// stripPrefixes, verbDestinations (:266-311), tee, sed -i, cp/mv, perl/ruby -i (:313-404) and the python and node one-line
// writes (:505-549). POSIX only: the PowerShell and .NET detection (:298, :305-308, :406-504, :551-559) is not ported. The
// oracle's answer comes first and unchanged; a write it cannot see (a wrapper option, a bundled flag, a command after a
// newline, a shell started with -c) is appended after it as a recorded intentionally changed case. Nothing is initialized at
// package level: lookups are functions and regexps compile where they are used.
//
// CRW-900 adds one more write the oracle cannot see: the destination of a Python copy, rename or link call (shutil.copy and
// its siblings, os.rename and its siblings, and the Path methods whose destination is their argument or their receiver).
// It lives in the hardened program walk and is appended after the oracle's answer like every other addition, so the oracle's
// own reading (scriptWriteDestinations) is unchanged.

// shellVerbDestinations is the verb step of ShellWriteDestinations: the oracle's destinations, then the others. The command
// strings a shell -c or eval runs are read again within a budget of 32 times the segment plus 64 KiB, so the work stays linear.
func shellVerbDestinations(segment string) []string {
	budget := 32*len(segment) + 65536
	return shellVerbSegment(segment, &budget)
}

func shellVerbSegment(segment string, budget *int) []string {
	return shellVerbAppendNew(shellVerbOracle(shellTokenize(segment)), shellVerbHardened(segment, budget))
}

// shellVerbAppendNew appends the destinations of more that out does not hold, keeping out's own order and duplicates.
func shellVerbAppendNew(out, more []string) []string {
	seen := make(map[string]struct{}, len(out)+len(more))
	for _, dest := range out {
		seen[dest] = struct{}{}
	}
	for _, dest := range more {
		if _, found := seen[dest]; !found {
			seen[dest] = struct{}{}
			out = append(out, dest)
		}
	}
	return out
}

// shellVerbNested is ShellWriteDestinations for the command string a shell -c or eval runs, one level deeper: the same
// segments, redirects and verbs. With the budget spent it reads redirects and the oracle's verbs only.
func shellVerbNested(command string, budget *int) []string {
	*budget -= len(command)
	out := []string{}
	for _, segment := range splitShellSegments(stripHeredocBodies(utf16.Encode([]rune(command)))) {
		out = append(out, shellStrings(redirectDestinations(segment))...)
		if *budget < 0 {
			out = append(out, shellVerbOracle(shellTokenize(shellString(segment)))...)
		} else {
			out = append(out, shellVerbSegment(shellString(segment), budget)...)
		}
	}
	return shellVerbAppendNew(out, literalRedirectDestinations(command))
}

// shellVerbNestedBoth reads a command string as the token holds it and again with its shell escapes removed (the token does not
// say which quotes held it, and each reading can hide what the other shows).
func shellVerbNestedBoth(command string, budget *int) []string {
	out := shellVerbNested(command, budget)
	if un := shellVerbUnescape(command); un != command {
		out = shellVerbAppendNew(out, shellVerbNested(un, budget))
	}
	return out
}

// shellVerbOracle is verbDestinations (:293-311) without the PowerShell and .NET branches.
func shellVerbOracle(tokens []string) []string {
	rest := shellVerbStripPrefixes(tokens)
	for len(rest) > 0 && rest[0] == "&" {
		rest = rest[1:]
	}
	return shellVerbRun(rest, false, nil)
}

// shellVerbRun reads the destinations of the command rest; hard selects the additions to the oracle's reading.
func shellVerbRun(rest []string, hard bool, budget *int) []string {
	if len(rest) == 0 || rest[0] == "" {
		return []string{}
	}
	verb, args := shellVerbName(rest[0]), rest[1:]
	switch {
	case verb == "tee":
		return shellVerbTee(args)
	case verb == "sed" && hard:
		return shellVerbSedWrites(args)
	case verb == "sed":
		return shellVerbSed(args)
	case verb == "cp" || verb == "mv":
		if hard {
			return shellVerbCpMvWrites(args)
		}
		return shellVerbCpMv(args)
	case verb == "perl" || verb == "ruby":
		return shellVerbInterp(args, hard)
	case verb == "python" || verb == "python3" || verb == "py" || verb == "node" || verb == "nodejs" || hard && shellVerbVersioned(verb):
		return shellVerbPythonNode(verb, args, hard)
	case hard && shellVerbIsShell(verb):
		if script, ok := shellVerbShellScript(args); ok {
			return shellVerbNestedBoth(script, budget)
		}
	case hard && verb == "eval":
		for { // eval builtin eval X runs X: a chain is peeled here, not read level by level
			next := shellVerbSkipWrappers(args)
			if len(next) == 0 || shellVerbName(next[0]) != "eval" {
				break
			}
			args = next[1:]
		}
		return shellVerbNestedBoth(strings.Join(args, " "), budget)
	}
	return []string{}
}

// shellVerbShellScript is the command string of a shell run with -c: the first operand after the leading options, when one of
// them (an option bundle such as -c, -lc, -cx) set c and none left the shell reading without running (-n, -o noexec; +n and
// +o noexec turn them off). Options come before the first operand, which may be a script file whose own arguments follow.
func shellVerbShellScript(args []string) (string, bool) {
	runC, noExec := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return shellVerbOperand(args, i+1, runC && !noExec)
		case a == "" || a[0] != '-' && a[0] != '+':
			return shellVerbOperand(args, i, runC && !noExec)
		case a == "--rcfile" || a == "--init-file":
			i++
		case len(a) > 1 && a[1] == '-':
		default:
			on := a[0] == '-'
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'c':
					runC = on
				case 'n':
					noExec = on
				case 'o', 'O': // each takes the next word as its value, wherever the letter stands in the bundle
					if a[j] == 'o' && i+1 < len(args) && args[i+1] == "noexec" {
						noExec = on
					}
					i++
				}
			}
		}
	}
	return "", false
}

func shellVerbOperand(args []string, i int, runs bool) (string, bool) {
	if runs && i < len(args) {
		return args[i], true
	}
	return "", false
}

// shellVerbBasename is basename (:266): what follows the last slash once every backslash is a slash.
func shellVerbBasename(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return p[strings.LastIndexByte(p, '/')+1:]
}

// shellVerbNormalize is normalizeVerb (:272): a trailing .exe, .cmd or .bat goes and case folds (ASCII only in the suffix, as
// the oracle's /i). toLowerCase maps U+0130 to i and a combining dot; the other Go differences (final sigma) cannot match a verb.
func shellVerbNormalize(verb string) string {
	if n := len(verb); n >= 4 && verb[n-4] == '.' {
		switch strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				return r + 32
			}
			return r
		}, verb[n-3:]) {
		case "exe", "cmd", "bat":
			verb = verb[:n-4]
		}
	}
	return strings.ToLower(strings.ReplaceAll(verb, "\u0130", "i\u0307"))
}

func shellVerbName(token string) string { return shellVerbNormalize(shellVerbBasename(token)) }

// shellVerbAssignment is /^[A-Za-z_][A-Za-z0-9_]*=/.
func shellVerbAssignment(token string) bool {
	name, _, found := strings.Cut(token, "=")
	if !found || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// shellVerbStripPrefixes is stripPrefixes (:276): sudo, command and builtin, and env with the NAME=value words after it.
func shellVerbStripPrefixes(tokens []string) []string {
	rest := tokens
	for len(rest) > 0 {
		head := ""
		if rest[0] != "" {
			head = shellVerbBasename(rest[0])
		}
		switch head {
		case "sudo", "command", "builtin":
			rest = rest[1:]
		case "env":
			rest = rest[1:]
			for len(rest) > 0 && shellVerbAssignment(rest[0]) {
				rest = rest[1:]
			}
		default:
			return rest
		}
	}
	return rest
}

// shellVerbTee is teeDestinations (:313): every operand.
func shellVerbTee(args []string) []string {
	out := []string{}
	for i, a := range args {
		if a == "--" {
			return append(out, args[i+1:]...)
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
		}
	}
	return out
}

// shellVerbSed is sedInPlaceDestinations (:327): the files of an in-place edit, the script being the first operand
// unless -e or -f gave it.
func shellVerbSed(args []string) []string {
	inPlace, sawExpression := false, false
	positional := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case a == "-i" || a == "--in-place" || strings.HasPrefix(a, "--in-place=") || strings.HasPrefix(a, "-i") && len(a) > 2:
			inPlace = true
			if a == "-i" && i+1 < len(args) && (args[i+1] == "" || strings.HasPrefix(args[i+1], ".")) {
				i++
			}
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			sawExpression = true
			i++
		case !strings.HasPrefix(a, "-"):
			positional = append(positional, a)
		}
	}
	switch {
	case !inPlace:
		return []string{}
	case sawExpression || len(positional) == 0:
		return positional
	}
	return positional[1:]
}

// shellVerbCpMv is cpMvDestinations (:354): the target directory, else the last of two or more operands.
func shellVerbCpMv(args []string) []string {
	targetDir := ""
	positional := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || a == "--target-directory":
			i++
			targetDir = ""
			if i < len(args) {
				targetDir = args[i]
			}
		case strings.HasPrefix(a, "--target-directory="):
			targetDir = a[len("--target-directory="):]
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case !strings.HasPrefix(a, "-"):
			positional = append(positional, a)
		}
	}
	if targetDir != "" {
		return []string{targetDir}
	}
	if len(positional) >= 2 {
		return []string{positional[len(positional)-1]}
	}
	return []string{}
}

// shellVerbBundleHas is /^-[a-zA-Z]*c/ for a letter c, with digits allowed in the bundle when digits: c occurs in the
// leading run of option letters. shellVerbBundleEnds is /^-[a-zA-Z]*c$/: the whole argument is such a bundle ending in c.
func shellVerbBundleHas(a string, c byte, digits bool) bool {
	for j := 1; a != "" && a[0] == '-' && j < len(a); j++ {
		if a[j] == c {
			return true
		}
		if !shellVerbLetter(a[j], digits) {
			return false
		}
	}
	return false
}

func shellVerbBundleEnds(a string, c byte, digits bool) bool {
	if len(a) < 2 || a[0] != '-' || a[len(a)-1] != c {
		return false
	}
	for j := 1; j < len(a); j++ {
		if !shellVerbLetter(a[j], digits) {
			return false
		}
	}
	return true
}

func shellVerbLetter(b byte, digits bool) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || digits && b >= '0' && b <= '9'
}

// shellVerbInterp is interpInPlaceDestinations (:379): the operands of a perl or ruby run with an -i option; -e and bundles
// ending in e take the next argument as program text. digits lets a bundle such as -0pi count.
func shellVerbInterp(args []string, digits bool) []string {
	inPlace := false
	positional := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case shellVerbBundleHas(a, 'i', digits):
			inPlace = true
			if strings.HasSuffix(a, "e") && !strings.HasPrefix(a, "-i") {
				i++
			}
		case a == "-e" || shellVerbBundleEnds(a, 'e', false):
			i++
		case !strings.HasPrefix(a, "-"):
			positional = append(positional, a)
		}
	}
	if inPlace {
		return positional
	}
	return []string{}
}

// shellVerbPythonNode is pythonNodeWriteDestinations (:505): the one-line program of python -c or node -e, scanned for
// writes. hard adds node -p, --print and -pe, python options bundled with -c (-Ic) and the shell-unescaped program.
func shellVerbPythonNode(verb string, args []string, hard bool) []string {
	isNode := verb == "node" || verb == "nodejs"
	isPy := verb == "python" || verb == "python3" || verb == "py" || hard && shellVerbVersioned(verb)
	script := ""
	for i := 0; i < len(args); i++ {
		a, next := args[i], ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch {
		case isPy && (a == "-c" || a == "--command"), isNode && (a == "-e" || a == "--eval"):
			script = next
		case isPy && strings.HasPrefix(a, "-c") && len(a) > 2:
			script = a[2:]
		case isNode && strings.HasPrefix(a, "--eval="):
			script = a[len("--eval="):]
		case isNode && strings.HasPrefix(a, "-e") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			script = a[2:]
		case hard && isNode && (a == "-p" || a == "--print" || a == "-pe"):
			script = next
		case hard && isPy && shellVerbBundleEnds(a, 'c', false) && !strings.ContainsAny(a, "mWXQ"): // -Ic, -uc
			script = next
		default:
			continue
		}
		break
	}
	if script == "" {
		return []string{}
	}
	out := shellVerbScriptWritesIn(script, hard, isPy)
	if un := shellVerbUnescape(script); hard && un != script {
		out = append(out, shellVerbScriptWritesIn(un, true, isPy)...)
	}
	return out
}

// shellVerbUnescape removes the backslashes a double-quoted shell word keeps in the token (the lexer reads the quotes off and
// leaves the escapes), so that a command string quoted into another command reads as the shell reads it.
func shellVerbUnescape(s string) string {
	return strings.NewReplacer("\\\"", "\"", "\\\\", "\\", "\\$", "$", "\\\x60", "\x60").Replace(s)
}

// shellVerbHardened reads each command of the segment again the way the shell does: the segment is cut at newlines and at a
// lone & outside quotes, a leading keyword, brace, NAME=value word or wrapper command (with its options) is skipped, and the
// verb is read with the getopt-style readers below. Its destinations are appended after the oracle's.
func shellVerbHardened(segment string, budget *int) []string {
	out := []string{}
	for _, command := range shellVerbSubsegments(segment) {
		out = append(out, shellVerbRun(shellVerbSkipWrappers(shellTokenize(command)), true, budget)...)
	}
	return out
}

// shellVerbSubsegments cuts at an unquoted newline, ; or |, and at a & that is not part of >&, <& or &>. A backslash keeps the
// character after it (a continued line, an escaped quote or &), which the oracle's own cut at ; | and && does not.
func shellVerbSubsegments(segment string) []string {
	s := utf16.Encode([]rune(segment))
	commands, start := []string{}, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			i = skipQuoted(s, i) - 1
		case c == '\\':
			i++
		case c == '\n' || c == ';' || c == '|' && shellAt(s, i-1) != '>' || c == '&' && shellAt(s, i-1) != '>' && shellAt(s, i-1) != '<' && shellAt(s, i+1) != '>':
			commands = append(commands, shellString(s[start:i]))
			start = i + 1
		}
	}
	return append(commands, shellString(s[start:]))
}

// shellVerbSkipWrappers drops what stands in front of the verb: parentheses and braces, shell keywords, NAME=value words, and
// wrapper commands with their options (sudo -u root, env -i, nohup, time -p, timeout 5, nice -n 5).
func shellVerbSkipWrappers(tokens []string) []string {
	rest := tokens
	for len(rest) > 0 {
		word := strings.TrimLeft(rest[0], "({")
		head := shellVerbName(word)
		opts, wrapper := shellVerbWrapper(head)
		switch {
		case word == "" || word == "&" || word == "!" || shellVerbAssignment(word) || shellVerbKeyword(head):
			rest = rest[1:]
		case wrapper:
			var runs bool
			if rest, runs = shellVerbSkipOptions(head, rest[1:], opts, head == "timeout"); !runs {
				return nil
			}
		case word != rest[0]:
			rest[0] = word
			return rest
		default:
			return rest
		}
	}
	return rest
}

// shellVerbSkipOptions skips the options of a wrapper command; opts lists its short options that take the next word as their
// value, and duration skips timeout's duration operand. It reports false when an option makes the wrapper run nothing.
func shellVerbSkipOptions(head string, rest []string, opts string, duration bool) ([]string, bool) {
	for len(rest) > 0 {
		a := rest[0]
		if a == "--" {
			rest = rest[1:]
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		rest = rest[1:]
		if a[1] == '-' {
			name, _, inline := strings.Cut(a[2:], "=")
			if shellVerbNoExec(head, name, 0) {
				return nil, false
			}
			if !inline && shellVerbLongValue(name) && len(rest) > 0 {
				rest = rest[1:]
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			if shellVerbNoExec(head, "", a[j]) {
				return nil, false
			}
			if strings.IndexByte(opts, a[j]) >= 0 {
				if j == len(a)-1 && len(rest) > 0 {
					rest = rest[1:]
				}
				break
			}
		}
	}
	if duration && len(rest) > 0 {
		rest = rest[1:]
	}
	return rest, true
}

// shellVerbNoExec is an option under which the wrapper looks something up or lists and runs no command: --help, --version,
// command -v and -V, sudo -l, -v, -V and -K.
func shellVerbNoExec(head, long string, short byte) bool {
	switch {
	case long == "help" || long == "version":
		return true
	case head == "command":
		return short == 'v' || short == 'V'
	case head == "sudo":
		return long == "list" || long == "validate" || long == "remove-timestamp" || strings.IndexByte("lvVK", short) >= 0 && short != 0
	}
	return false
}

// shellVerbWrapper lists wrapper commands with the short options that take the next word (name:letters).
func shellVerbWrapper(head string) (string, bool) {
	for _, entry := range strings.Fields("sudo:ughpCrtTDRU doas:uC env:uCS time:fo nice:n ionice:cnpt timeout:sk stdbuf:ioe exec:a nohup: setsid: command: builtin:") {
		if name, opts, _ := strings.Cut(entry, ":"); name == head {
			return opts, true
		}
	}
	return "", false
}

func shellVerbLongValue(name string) bool {
	return slices.Contains(strings.Fields("user group host prompt chdir chroot role type close-from other-user unset split-string "+
		"signal kill-after adjustment class classdata input output error"), name)
}

func shellVerbKeyword(head string) bool {
	return slices.Contains(strings.Fields("then else elif do if while until"), head)
}

func shellVerbIsShell(verb string) bool {
	return slices.Contains(strings.Fields("sh bash zsh dash ksh ash mksh fish"), verb)
}

// shellVerbVersioned is a python interpreter name carrying a version: python2, python3.11.
func shellVerbVersioned(verb string) bool {
	version, found := strings.CutPrefix(verb, "python")
	return found && version != "" && strings.Trim(version, "0123456789.") == ""
}

// shellVerbSedWrites reads sed's options as getopt does: a bundle such as -ni or -Ei holds the in-place flag, -e and -f
// (attached or not) and --expression and --file give the script, so then every operand is a file.
func shellVerbSedWrites(args []string) []string {
	inPlace, script := false, false
	files := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			switch name {
			case "in-place":
				inPlace = true
			case "expression", "file":
				script = true
				if !inline {
					i++
				}
			case "line-length":
				if !inline {
					i++
				}
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'i' || c == 'I' {
					inPlace = true
					if j == len(a)-1 && i+1 < len(args) && (args[i+1] == "" || strings.HasPrefix(args[i+1], ".")) {
						i++
					}
					break
				}
				if c == 'e' || c == 'f' || c == 'l' {
					script = script || c != 'l'
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		default:
			files = append(files, a)
		}
	}
	if inPlace && !script && len(files) > 0 {
		files = files[1:]
	}
	if !inPlace {
		return []string{}
	}
	return files
}

// shellVerbCpMvWrites reads cp and mv options as getopt does: -t and -S inside a bundle (-rt DIR) or with the value attached
// (-tDIR) take their value, and --suffix does not leave its value among the operands.
func shellVerbCpMvWrites(args []string) []string {
	dir := ""
	files := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			if name != "target-directory" && name != "suffix" {
				continue
			}
			if !inline && i+1 < len(args) {
				i++
				value = args[i]
			}
			if name == "target-directory" {
				dir = value
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				if a[j] != 't' && a[j] != 'S' {
					continue
				}
				value := a[j+1:]
				if value == "" && i+1 < len(args) {
					i++
					value = args[i]
				}
				if a[j] == 't' {
					dir = value
				}
				break
			}
		default:
			files = append(files, a)
		}
	}
	if dir != "" {
		return []string{dir}
	}
	if len(files) >= 2 {
		return files[len(files)-1:]
	}
	return []string{}
}

// shellVerbScriptWrites is scriptWriteDestinations (:535): the path of open(path, "w"), Path(path).write_text(...) and
// writeFile(path...) calls, pattern by pattern in that order. The oracle's backreferences (the closing quote repeats the opening
// one) become one alternative per quote; each alternation picks its branch by the quote present, so it never competes with
// lazy matching and each pattern matches what the backtracking original does. hard adds template literal paths, the open() and
// Path reader and the decoded value of a JavaScript literal, after the oracle's raw text.
func shellVerbScriptWrites(script string, hard bool) []string {
	return shellVerbScriptWritesIn(script, hard, true)
}

// shellVerbScriptWritesIn is shellVerbScriptWrites with the program's language: python selects the Python readers, whose
// triple-quoted region rule belongs to Python source alone. A JavaScript program is not Python source, so three quotes inside a
// template literal must not open a triple-quoted region and swallow the rest of the program, which would lose the destination
// of a later write; a Node program keeps the single-quote walk the reader always had.
func shellVerbScriptWritesIn(script string, hard, python bool) []string {
	s, dot, pre := lintSpace, lintDot, "[rRuUbBfF]*"
	quoted := func(body string) string { return "(?:'(" + body + ")'|\"(" + body + ")\")" }
	callQuote, callGroups := quoted(dot+"*?"), 2
	if hard {
		callQuote, callGroups = "(?:'("+dot+"*?)'|\"("+dot+"*?)\"|\x60("+dot+"*?)\x60)", 3
	}
	out := []string{}
	for _, p := range []struct {
		pattern string
		groups  int
	}{
		{"\\bopen" + s + "*\\(" + s + "*" + pre + quoted(dot+"*?") + s + "*," + s + "*" + pre + quoted("[wax][^'\"]*"), 2},
		{"\\bPath" + s + "*\\(" + s + "*" + pre + quoted(dot+"*?") + s + "*\\)" + s + "*\\.write_(?:text|bytes)" + s + "*\\(", 2},
		{"\\b(?:writeFileSync|writeFile|appendFileSync|appendFile|createWriteStream)" + s + "*\\(" + s + "*" + callQuote, callGroups},
	} {
		for _, m := range regexp.MustCompile(p.pattern).FindAllStringSubmatch(script, -1) {
			if path := strings.Join(m[1:1+p.groups], ""); path != "" {
				out = append(out, path)
			}
		}
	}
	if hard {
		out = append(out, shellWriteEscapeJSWrites(script, out)...)
		out = append(out, shellVerbOpenWritesIn(script, python)...)
	}
	return out
}

// shellVerbOpenWrites reads each open(...) call of a program as Python does, in one pass over the text: the path is the first
// argument or file=, the mode the second or mode=, in either order and with other keywords between. A call whose mode writes,
// appends, creates or updates (a w, a, x or + in it) names its path; only string literals count. One frame is kept per open
// bracket and arguments are spans of the text, so unclosed and nested calls cost no more than their own characters. A
// Path(...).write_text or .write_bytes call names the join of its arguments (shellWriteEscapePath).
func shellVerbOpenWrites(script string) []string {
	return shellVerbOpenWritesIn(script, true)
}

// shellVerbOpenWritesIn is shellVerbOpenWrites with the program's language: python enables the triple-quoted region rule, so a
// Node program is scanned exactly as it was before that rule existed (shellVerbScriptWritesIn).
func shellVerbOpenWritesIn(script string, python bool) []string {
	return shellWriteFStringOpenWritesRunes(shellVerbWithoutComments(script, python), python)
}

// shellWriteExecMaxDepth bounds the program nesting the exec walk reads: a program nested deeper is unreadable, so the memory
// gate fails closed rather than reading a program it cannot finish (CRW-754, criterion c1).
const shellWriteExecMaxDepth = 32

// shellWriteExecUnreadableWhat is the what the walk reports for a program it cannot read (the deny reason names it as
// "(a program the gate cannot read: " + what + ")"; CRW-754, criterion c1).
const shellWriteExecUnreadableWhat = "a Python program passed to exec, eval or compile"

// shellWriteExecCallee reports whether the bracket c at i opens a call to exec, eval or compile (shellWriteExecCalleeExpr).
func shellWriteExecCallee(rs []rune, i int, c rune) bool {
	if c != '(' {
		return false
	}
	for i > 0 && shellVerbSpaceRune(rs[i-1]) {
		i--
	}
	return shellWriteExecCalleeExpr(rs, i)
}

// shellWriteExecCalleeExpr reports whether the expression that ends just before rs[end] is exec, eval or compile, called
// directly or through builtins. or __builtins. Parentheses around the whole callee expression do not change it, so
// (exec)(...) and (builtins.exec)(...) count (CRW-754 review). A dot before the name makes it an attribute of whatever
// precedes the module name, so only those two module names count; any other attribute chain is a different callee and is left
// to the dynamic routes this issue records as out of scope. Any other character (a newline, a semicolon, a comma, an opening
// bracket, the start of the program) leaves a plain call, and a def or async def header binds a name instead.
func shellWriteExecCalleeExpr(rs []rune, end int) bool {
	for end > 0 && shellVerbSpaceRune(rs[end-1]) {
		end--
	}
	if end == 0 {
		return false
	}
	if rs[end-1] == ')' {
		close := end - 1
		open := shellWriteExecMatchParen(rs, close)
		// A group that is the whole expression is stripped and its content read again; one that follows a value is a call
		// whose result is being called, which is not this callee.
		if open < 0 || open > 0 && shellWriteExecValueRune(rs[open-1]) {
			return false
		}
		return shellWriteExecCalleeExpr(rs, close)
	}
	for _, name := range []string{"exec", "eval", "compile"} {
		n := len(name)
		if end < n || string(rs[end-n:end]) != name || shellWriteExecIdentRune(rs, end-n-1) {
			continue
		}
		j := end - n - 1
		for j >= 0 && shellVerbSpaceRune(rs[j]) {
			j-- // legal spacing around the attribute operator: runner . exec(src)
		}
		if j >= 0 && rs[j] == '.' {
			for _, module := range []string{"builtins", "__builtins__"} {
				m := len(module)
				if j < m || string(rs[j-m:j]) != module || shellWriteExecIdentRune(rs, j-m-1) || j-m-1 >= 0 && rs[j-m-1] == '.' {
					continue
				}
				return true
			}
			continue
		}
		if shellWriteExecDefHeader(rs, j) {
			continue // a def or async def header binds a name; the parenthesis opens its parameters, not a call
		}
		return true // a plain call: nothing, or a character that is no identifier, stands before the name
	}
	return false
}

// shellWriteExecMatchParen is the index of the ( that matches the ) at close, or -1 when the program has none.
func shellWriteExecMatchParen(rs []rune, close int) int {
	depth := 0
	for j := close; j >= 0; j-- {
		switch rs[j] {
		case ')':
			depth++
		case '(':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// shellWriteExecIdentRune reports whether rs[j] can stand inside an identifier, so a name ending at j is not a word of its own.
func shellWriteExecIdentRune(rs []rune, j int) bool {
	return j >= 0 && (rs[j] >= 128 || rs[j] == '_' || shellVerbLetter(byte(rs[j]), true))
}

// shellWriteExecValueRune reports whether r can end a value, so a group that follows it is a call or an index of that value
// rather than the whole callee expression.
func shellWriteExecValueRune(r rune) bool {
	return r >= 128 || r == '_' || shellVerbLetter(byte(r), true) || r == '.' || r == ')' || r == ']' || r == '}'
}

// shellWriteExecDefHeader reports whether the name that ends just before rs[j] is the one a def or async def header binds, so
// the parenthesis that follows opens a parameter list and runs nothing (CRW-754 review: a definition is no call).
func shellWriteExecDefHeader(rs []rune, j int) bool {
	k := j
	for k >= 0 && (rs[k] == ' ' || rs[k] == '\t') {
		k--
	}
	if k < 2 || string(rs[k-2:k+1]) != "def" {
		return false
	}
	b := func(p int) bool {
		return p >= 0 && (rs[p] >= 128 || rs[p] == '_' || shellVerbLetter(byte(rs[p]), true))
	}
	return !b(k - 3)
}

// shellWriteFStringOpenWritesRunes is shellVerbOpenWritesIn over an already comment-stripped program: the destinations the
// walk reads (shellWriteExecScan), without its fail-closed reason.
func shellWriteFStringOpenWritesRunes(rs []rune, python bool) []string {
	dests, _ := shellWriteExecScan(rs, python, 0)
	return dests
}

// shellWriteExecScan is the program walk: the writes of each open() and Path(...).write_text/bytes call, of a string literal
// whose prefix holds an f read as Python reads it - each replacement field's expression is read as program text by this same
// walk, so an open() call inside a field names its path (CRW-741) - and of the program text a string literal passed to exec,
// eval or compile names (CRW-754). what is the fail-closed reason when the walk cannot read a program it was given: an
// f-string replacement field it cannot finish (shellWriteFStringUnreadableWhat), or a program passed to exec, eval or compile
// whose first argument is no string literal, or one nested deeper than shellWriteExecMaxDepth
// (shellWriteExecUnreadableWhat). The walk keeps going after recording what, so a destination it did read is still named.
// Every other literal keeps the one-region rule of shellWriteTripleScanRegion.
func shellWriteExecScan(rs []rune, python bool, depth int) ([]string, string) {
	return shellWriteExecScanIn(rs, python, depth, shellWriteCopyImports{})
}

// shellWriteExecScanIn is shellWriteExecScan with the bindings the enclosing program made: the program text of an exec,
// eval or compile call and an f-string replacement field are read by this same walk and run in the scope that holds
// them, so a copy, rename or link call inside one resolves the enclosing program's imports too (CRW-900 review).
func shellWriteExecScanIn(rs []rune, python bool, depth int, outer shellWriteCopyImports) (dests []string, what string) {
	if depth > shellWriteExecMaxDepth {
		return []string{}, shellWriteExecUnreadableWhat
	}
	type frame struct {
		kind  byte // 'o' for open(, 'p' for Path(, 'e' for exec/eval/compile(, 'c', 'r' or 'l' for a copy, rename or link call, else 0
		start int
		args  [][2]int
		recv  [][2]int // the receiver arguments of Path(b).symlink_to(a) or .hardlink_to(a), whose destination is b
	}
	// pending is the method call a Path(...) receiver just named: the bracket of Path(a).rename(b) and its siblings, read
	// when the walk reaches it, with the receiver's own arguments kept for the two methods whose destination is the receiver.
	type pendingCall struct {
		at   int
		kind byte
		recv [][2]int
	}
	var stack []frame
	var pending pendingCall
	var binds shellWriteCopyImports
	if python {
		binds = shellWriteCopyImportsOf(rs, outer)
	}
	dests = []string{}
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			if python && shellWriteFStringPrefix(rs, i) {
				end, fields, bad := shellWriteFStringRegion(rs, i, 0)
				for _, f := range fields {
					// The expression is read as program text: its comments are cut first, so a quote or brace in one
					// is no syntax (a Python 3.12+ multi-line field allows a comment).
					more, inner := shellWriteExecScanIn(shellVerbWithoutComments(string(rs[f[0]:f[1]]), python), python, depth, binds)
					dests = append(dests, more...)
					if inner != "" && what == "" {
						what = inner
					}
				}
				if bad && what == "" {
					what = shellWriteFStringUnreadableWhat
				}
				i = end - 1
				continue
			}
			i = shellWriteTripleScanRegion(rs, i, python) - 1
		case c == '(' || c == '[' || c == '{':
			kind, recv := byte(0), [][2]int(nil)
			switch {
			case pending.kind != 0 && pending.at == i:
				kind, recv, pending = pending.kind, pending.recv, pendingCall{}
			default:
				kind = shellVerbCallKind(rs, i, c)
				if kind == 0 && python && shellWriteExecCallee(rs, i, c) {
					kind = 'e'
				}
				if kind == 0 && python && c == '(' {
					kind = shellWriteCopyModuleKind(rs, i, binds)
				}
			}
			stack = append(stack, frame{kind: kind, start: i + 1, recv: recv})
		case c == ',' && len(stack) > 0:
			if top := &stack[len(stack)-1]; top.kind != 0 {
				top.args = append(top.args, [2]int{top.start, i})
				top.start = i + 1
			}
		case (c == ')' || c == ']' || c == '}') && len(stack) > 0:
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			spans := append(top.args, [2]int{top.start, i})
			switch {
			case top.kind == 'o' && c == ')':
				dests = append(dests, shellVerbOpenCall(rs, spans)...)
			case top.kind == 'p' && c == ')':
				if shellVerbWriteMethod(rs, i+1) {
					dests = append(dests, shellWriteEscapePath(rs, spans)...)
				} else if python {
					if at, kind, ok := shellWritePathMethodCall(rs, i+1); ok {
						pending = pendingCall{at: at, kind: kind, recv: spans}
					}
				}
			case top.kind == 'c' && c == ')':
				dests = append(dests, shellWriteCopyDest(rs, spans, 1, "dst")...)
			case top.kind == 'n' && c == ')':
				dests = append(dests, shellWriteCopyDest(rs, spans, 1, "new")...)
			case top.kind == 'r' && c == ')':
				dests = append(dests, shellWriteCopyDest(rs, spans, 0, "target")...)
			case top.kind == 'l' && c == ')':
				dests = append(dests, shellWriteEscapePath(rs, top.recv)...)
			case top.kind == 'e' && c == ')':
				more, inner := shellWriteExecProgram(rs, spans, depth, binds)
				dests = append(dests, more...)
				if inner != "" && what == "" {
					what = inner
				}
			}
		}
	}
	return dests, what
}

// shellWriteExecProgram reads the first argument of an exec, eval or compile call as program text when it is a string literal
// Python can decode, and reports the fail-closed reason when it is not (CRW-754). The literal's value, as Python decodes it,
// is read by the same walk one level deeper. An f-string with a replacement field has no value here, so it is unreadable too;
// the writes inside its fields were already named by the walk above (CRW-741). outer is the enclosing program's bindings,
// which the program text inherits (CRW-900 review).
func shellWriteExecProgram(rs []rune, spans [][2]int, depth int, outer shellWriteCopyImports) ([]string, string) {
	for _, span := range spans {
		arg := rs[span[0]:span[1]]
		if shellVerbBlank(arg) {
			continue // the empty argument after a trailing comma
		}
		if shellWriteEscapeField(arg) {
			return nil, shellWriteExecUnreadableWhat
		}
		program, ok := shellVerbLiteral(arg)
		if !ok {
			return nil, shellWriteExecUnreadableWhat
		}
		if shellWriteExecBytesLiteral(arg) && shellWriteExecCodingDecl(program) {
			// A bytes literal carrying a source-encoding declaration is decoded by Python under that codec, which changes
			// the program text this reader sees (CRW-754 review). The codec is not modelled here, so fail closed. A bytes
			// literal with no declaration decodes as UTF-8, which is what this reader already reads.
			return nil, shellWriteExecUnreadableWhat
		}
		return shellWriteExecScanIn(shellVerbWithoutComments(program, true), true, depth+1, outer)
	}
	return nil, shellWriteExecUnreadableWhat
}

// shellWriteExecBytesLiteral reports whether arg is a bytes string literal (a b or B in its prefix), whose bytes Python
// decodes under its own source-encoding rules rather than as the text this reader reads (CRW-754 review).
func shellWriteExecBytesLiteral(arg []rune) bool {
	i := 0
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	bytes := false
	for ; i < len(arg) && strings.ContainsRune("rRuUbBfF", arg[i]); i++ {
		bytes = bytes || arg[i] == 'b' || arg[i] == 'B'
	}
	return bytes && i < len(arg) && (arg[i] == '\'' || arg[i] == '"')
}

// shellWriteExecCodingDecl reports whether program holds a source-encoding declaration (PEP 263) in its first two lines, so
// the bytes it came from are decoded under a codec this reader does not model (CRW-754 review). A str literal is not decoded
// that way, but the same text there is no declaration either, so the check costs nothing on one.
func shellWriteExecCodingDecl(program string) bool {
	for line, rest := 0, program; line < 2 && rest != ""; line++ {
		text, next, found := strings.Cut(rest, "\n")
		if at := strings.IndexByte(text, '#'); at >= 0 && shellWriteExecCodingLine(text[at+1:]) {
			return true
		}
		if !found {
			break
		}
		rest = next
	}
	return false
}

// shellWriteExecCodingLine reports whether a comment body names a source encoding, as PEP 263's coding[:=] does.
func shellWriteExecCodingLine(comment string) bool {
	at := strings.Index(comment, "coding")
	if at < 0 {
		return false
	}
	rest := strings.TrimLeft(comment[at+len("coding"):], " \t\f")
	if rest == "" || rest[0] != ':' && rest[0] != '=' {
		return false
	}
	rest = strings.TrimLeft(rest[1:], " \t\f")
	return rest != "" && (shellVerbLetter(rest[0], true) || rest[0] == '-' || rest[0] == '_' || rest[0] == '.')
}

// shellVerbWithoutComments is the program with its # comments (outside string literals) cut off at the end of the line, so a
// quote in a comment opens no string and a comment inside a call is no argument. A comment ends at a \n or at a lone \r,
// because Python's source decoding reads a lone CR as a line break too (CRW-754 review); ending it only at \n let the code
// after a \r stay hidden inside the comment while Python ran it.
func shellVerbWithoutComments(script string, python bool) []rune {
	rs, out := []rune(script), []rune{}
	for i := 0; i < len(rs); {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			end := shellWriteTripleScanRegion(rs, i, python)
			out = append(out, rs[i:end]...)
			i = end
		case c == '#':
			i++
			for i < len(rs) && rs[i] != '\n' && rs[i] != '\r' {
				i++
			}
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

// shellVerbCallKind says whether the bracket c at i opens the arguments of open( ('o') or Path( ('p'): the word before it,
// blanks allowed between, as in \bopen\s*\(.
func shellVerbCallKind(rs []rune, i int, c rune) byte {
	for i > 0 && shellVerbSpaceRune(rs[i-1]) {
		i--
	}
	for _, word := range []string{"open", "Path"} {
		if n := len(word); c == '(' && i >= n && string(rs[i-n:i]) == word && (i == n || rs[i-n-1] >= 128 || !shellVerbLetter(byte(rs[i-n-1]), true) && rs[i-n-1] != '_') {
			return word[0] | 0x20 // 'o' or 'p'
		}
	}
	return 0
}

// shellVerbWriteMethod reports whether .write_text( or .write_bytes( follows at j (blanks allowed), as after Path(...).
func shellVerbWriteMethod(rs []rune, j int) bool {
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j >= len(rs) || rs[j] != '.' {
		return false
	}
	for j++; j < len(rs) && shellVerbSpaceRune(rs[j]); {
		j++
	}
	for _, name := range []string{"write_text", "write_bytes"} {
		if end := j + len(name); end <= len(rs) && string(rs[j:end]) == name {
			for ; end < len(rs) && shellVerbSpaceRune(rs[end]); end++ {
			}
			return end < len(rs) && rs[end] == '('
		}
	}
	return false
}

// shellWritePathMethodCall reads the method call that follows a Path(...) receiver at j (CRW-900): its bracket index and the
// frame kind this reader gives it - 'r' for .rename( and .replace(, whose destination is their own argument, and 'l' for
// .symlink_to( and .hardlink_to(, whose destination is the receiver. Blanks are allowed around the dot and before the
// bracket. Every other method, and the end of the program, is no such call.
func shellWritePathMethodCall(rs []rune, j int) (int, byte, bool) {
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j >= len(rs) || rs[j] != '.' {
		return 0, 0, false
	}
	for j++; j < len(rs) && shellVerbSpaceRune(rs[j]); {
		j++
	}
	start := j
	for j < len(rs) && shellWriteCopyIdentRune(rs[j]) {
		j++
	}
	var kind byte
	switch string(rs[start:j]) {
	case "rename", "replace":
		kind = 'r'
	case "symlink_to", "hardlink_to":
		kind = 'l'
	default:
		return 0, 0, false
	}
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j >= len(rs) || rs[j] != '(' {
		return 0, 0, false
	}
	return j, kind, true
}

// shellWriteCopyDest is the destination argument of a copy, rename or link call: the positional argument at pos, or the
// keyword named key, whichever the call gives. A call that gives neither names nothing, and a destination that is no string
// literal names nothing either, exactly as the non-literal argument of open() names nothing (CRW-900, criterion c1).
func shellWriteCopyDest(rs []rune, spans [][2]int, pos int, key string) []string {
	positional, arg := 0, []rune(nil)
	for _, span := range spans {
		value := rs[span[0]:span[1]]
		if shellVerbBlank(value) {
			continue // the empty argument after a trailing comma
		}
		if name, keyword, ok := shellVerbKeywordArg(value); ok {
			if name == key {
				arg = keyword
			}
			continue
		}
		if positional == pos {
			arg = value
		}
		positional++
	}
	return shellWriteCopyLiteral(arg)
}

// shellWriteCopyLiteral is the destination a string literal names, in both readings this port keeps for a path: the earlier
// one, which drops a backslash only before the literal's own quote or another backslash, and the decoded one
// (shellWriteEscapeLiteral). A value that is no literal names nothing.
func shellWriteCopyLiteral(arg []rune) []string {
	if len(arg) == 0 {
		return nil
	}
	names := []string{}
	for _, earlier := range []bool{true, false} {
		if file, ok := shellWriteEscapeLiteral(arg, earlier); ok && file != "" && !slices.Contains(names, file) {
			names = append(names, file)
		}
	}
	return names
}

// shellWriteCopyModuleKind reads the callee of the call whose bracket is at i when it names a copy, rename or link function
// this reader knows ('c'): a name after the shutil. or os. prefix, a bare name a from-import bound (from shutil import copy),
// or an alias an import bound (import shutil as s). Every other callee is no copy call.
func shellWriteCopyModuleKind(rs []rune, i int, binds shellWriteCopyImports) byte {
	j := i - 1
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	end := j
	for j >= 0 && shellWriteCopyIdentRune(rs[j]) {
		j--
	}
	if end < j+1 {
		return 0
	}
	name := string(rs[j+1 : end+1])
	for before := j; before >= 0; before-- {
		if shellVerbSpaceRune(rs[before]) {
			continue
		}
		if rs[before] != '.' {
			break
		}
		before--
		for before >= 0 && shellVerbSpaceRune(rs[before]) {
			before--
		}
		rend := before
		for before >= 0 && shellWriteCopyIdentRune(rs[before]) {
			before--
		}
		if rend < before+1 {
			return 0
		}
		if before >= 0 && rs[before] == '.' {
			return 0 // an attribute chain (a.shutil.copy) is not the module itself
		}
		if kind := shellWriteCopyKind(string(rs[before+1:rend+1]), name, binds); kind != 0 {
			return kind
		}
		return 0
	}
	if modules := binds.from[name]; len(modules) > 0 && !shellWriteExecDefHeader(rs, j) {
		for _, module := range modules {
			if module == "os" && name == "renames" {
				return 'n'
			}
		}
		return 'c'
	}
	return 0
}

// shellWriteCopyKind is the frame kind of a call to a copy, rename or link function, or 0 when the call is no such
// function: 'n' for os.renames, whose destination keyword is new, and 'c' for every other one, whose destination keyword
// is dst. callee is the module the call names or a local name the program bound, and binds says which modules that local
// name stands for (a name may stand for more than one, and any of them naming the function is enough).
func shellWriteCopyKind(callee, name string, binds shellWriteCopyImports) byte {
	modules := []string{callee}
	if callee != "shutil" && callee != "os" {
		modules = binds.alias[callee]
	}
	for _, module := range modules {
		if module == "os" && name == "renames" {
			return 'n'
		}
		if shellWriteCopyFunc(module, name) {
			return 'c'
		}
	}
	return 0
}

// shellWriteCopyFuncs is the functions of a module whose destination this reader names: every one of them writes to its
// second argument, or to the Path receiver or argument the call names (CRW-900).
func shellWriteCopyFuncs(module string) []string {
	switch module {
	case "shutil":
		return []string{"copy", "copy2", "copyfile", "copytree", "move"}
	case "os":
		return []string{"rename", "replace", "renames", "link", "symlink"}
	}
	return nil
}

// shellWriteCopyFunc reports whether a module names such a function.
func shellWriteCopyFunc(module, name string) bool {
	return slices.Contains(shellWriteCopyFuncs(module), name)
}

// shellWriteCopyImports is what a Python program's import statements bind for the copy, rename and link calls this reader
// names: every module a local name was bound to (import shutil as s) and every module a from-import bound a bare name to
// (from shutil import copy). A name keeps every binding the program gave it, so a later rebinding does not erase the
// reading an earlier call needs; nothing else is bound, so an unimported bare name names nothing.
type shellWriteCopyImports struct {
	alias map[string][]string
	from  map[string][]string
}

// shellWriteCopyBind records that a statement bound local to module, once per module.
func shellWriteCopyBind(binds map[string][]string, local, module string) {
	if local == "" || local == "*" || module == "" {
		return
	}
	if !slices.Contains(binds[local], module) {
		binds[local] = append(binds[local], module)
	}
}

// shellWriteCopyImportsMerge is the union of two binding sets: a program read inside another keeps the enclosing
// program's imports beside its own, because an f-string replacement field and a literal passed to exec both run in the
// scope that holds them (CRW-900 review).
func shellWriteCopyImportsMerge(outer, inner shellWriteCopyImports) shellWriteCopyImports {
	out := shellWriteCopyImports{alias: map[string][]string{}, from: map[string][]string{}}
	for _, binds := range []shellWriteCopyImports{outer, inner} {
		for local, modules := range binds.alias {
			for _, module := range modules {
				shellWriteCopyBind(out.alias, local, module)
			}
		}
		for local, modules := range binds.from {
			for _, module := range modules {
				shellWriteCopyBind(out.from, local, module)
			}
		}
	}
	return out
}

// shellWriteCopyImportsOf reads the import statements of a program, starting from the bindings an enclosing program
// already made (empty at the top level): the program read recursively - an f-string replacement field, the text of a
// literal passed to exec, eval or compile - runs in the scope that holds it, so those names are in scope there too
// (CRW-900 review). A string literal is no import text, so a name inside one binds nothing, and a comment was already cut
// from the program the walk reads. A statement ends at a newline or a semicolon outside every bracket; a newline inside
// one is an implicit line join and a backslash-newline an explicit one, so an import whose names are parenthesised over
// several lines binds them all (CRW-900 review: dropping it would leave the destination of the call it binds unnamed,
// which is the fail-open direction).
func shellWriteCopyImportsOf(rs []rune, outer shellWriteCopyImports) shellWriteCopyImports {
	binds := shellWriteCopyImportsMerge(outer, shellWriteCopyImports{alias: map[string][]string{}, from: map[string][]string{}})
	words := []string{}
	flush := func() {
		if len(words) > 0 {
			shellWriteCopyImportStatement(words, &binds)
			words = words[:0]
		}
	}
	depth := 0
	for i := 0; i < len(rs); {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			i = shellWriteTripleScanRegion(rs, i, true)
		case c == '#':
			for i < len(rs) && rs[i] != '\n' && rs[i] != '\r' {
				i++
			}
		case c == '\\' && i+1 < len(rs) && (rs[i+1] == '\n' || rs[i+1] == '\r'):
			i += 2
			if rs[i-1] == '\r' && i < len(rs) && rs[i] == '\n' {
				i++
			}
		case c == '(' || c == '[' || c == '{':
			depth++
			i++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			i++
		case (c == '\n' || c == '\r' || c == ';') && depth == 0:
			flush()
			i++
		case shellWriteCopyIdentRune(c):
			j := i
			for j < len(rs) && shellWriteCopyIdentRune(rs[j]) {
				j++
			}
			words = append(words, string(rs[i:j]))
			i = j
		case c == '*':
			words = append(words, "*")
			i++
		default:
			i++
		}
	}
	flush()
	return binds
}

// shellWriteCopyImportStatement reads one statement's words: import a [as b], import a, b, from m import x [as y], and the
// star form from m import *, which binds every function of m this reader knows.
func shellWriteCopyImportStatement(words []string, binds *shellWriteCopyImports) {
	switch {
	case len(words) >= 2 && words[0] == "import":
		for _, item := range shellWriteCopyImportItems(words[1:]) {
			name := item[0]
			if item[1] != "" {
				name = item[1]
			}
			shellWriteCopyBind(binds.alias, name, item[0])
		}
	case len(words) >= 3 && words[0] == "from" && words[2] == "import":
		module := words[1]
		for _, item := range shellWriteCopyImportItems(words[3:]) {
			if item[0] == "*" {
				for _, known := range shellWriteCopyFuncs(module) {
					shellWriteCopyBind(binds.from, known, module)
				}
				continue
			}
			name := item[0]
			if item[1] != "" {
				name = item[1]
			}
			if name != "" && shellWriteCopyFunc(module, item[0]) {
				shellWriteCopyBind(binds.from, name, module)
			}
		}
	}
}

// shellWriteCopyImportItems splits the name list of an import clause: each item is a name and, after "as", the name it is
// bound to. A star is the name "*".
func shellWriteCopyImportItems(words []string) [][2]string {
	out := [][2]string{}
	for i := 0; i < len(words); {
		item := [2]string{words[i], ""}
		i++
		if i+1 < len(words) && words[i] == "as" {
			item[1] = words[i+1]
			i += 2
		}
		out = append(out, item)
	}
	return out
}

// shellWriteCopyIdentRune is what a Python identifier holds, in the ASCII this reader reads: a letter, a digit or an
// underscore, and any rune outside ASCII, as the identifier tests beside this one read it.
func shellWriteCopyIdentRune(r rune) bool {
	return r >= 128 || r == '_' || shellVerbLetter(byte(r), true)
}

func shellVerbBlank(arg []rune) bool {
	for _, r := range arg {
		if !shellVerbSpaceRune(r) {
			return false
		}
	}
	return true
}

func shellVerbSpaceRune(r rune) bool {
	if r < 128 {
		return r == ' ' || r >= '\t' && r <= '\r'
	}
	return text.Trim(string(r)) == ""
}

// shellVerbOpenCall decides one call from its argument spans: whether its mode opens for writing, and its path. The decoded
// reading (shellVerbLiteral) is added to the earlier one, which kept escapes other than a backslash and the quote as written, and
// neither replaces the other: each reading names its own path when its own mode writes. A mode written with a \N{name} escape
// cannot be decoded (it needs the Unicode name table), so it counts as writing for both readings.
func shellVerbOpenCall(rs []rune, spans [][2]int) []string {
	var path, mode []rune
	positional := 0
	for _, span := range spans {
		arg := rs[span[0]:span[1]]
		if shellVerbBlank(arg) {
			continue // the empty argument after a trailing comma
		}
		if name, value, keyword := shellVerbKeywordArg(arg); keyword {
			switch name {
			case "file":
				path = value
			case "mode":
				mode = value
			}
			continue
		}
		switch positional {
		case 0:
			path = arg
		case 1:
			mode = arg
		}
		positional++
	}
	names := []string{}
	_, decodedOK := shellVerbLiteral(mode)
	named := !decodedOK && strings.Contains(string(mode), "\\N{")
	for _, earlier := range []bool{true, false} {
		if kind, ok := shellWriteEscapeLiteral(mode, earlier); !named && !(ok && strings.ContainsAny(kind, "wax+")) {
			continue
		}
		if file, ok := shellWriteEscapeLiteral(path, earlier); ok && file != "" && !slices.Contains(names, file) {
			names = append(names, file)
		}
	}
	return names
}

// shellVerbKeywordArg splits name=value (not ==) off an argument.
func shellVerbKeywordArg(arg []rune) (string, []rune, bool) {
	i := 0
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	start := i
	for i < len(arg) && arg[i] < 128 && (arg[i] == '_' || shellVerbLetter(byte(arg[i]), true)) {
		i++
	}
	end := i
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	if end == start || i >= len(arg) || arg[i] != '=' || i+1 < len(arg) && arg[i+1] == '=' {
		return "", nil, false
	}
	return string(arg[start:end]), arg[i+1:], true
}

// shellVerbLiteral is the value of a Python string literal argument: a quoted string with an optional r, u, b or f prefix, whose
// escapes are decoded unless it is raw (shellWriteEscapePython). Anything else around the quotes (a concatenation, a name), and a
// literal Python would reject or whose value holds NUL, makes it no literal.
func shellVerbLiteral(arg []rune) (string, bool) { return shellWriteEscapeLiteral(arg, false) }

// shellWriteEscapeLiteral reads a Python string literal; earlier selects the reading before escapes were decoded, which drops a
// backslash only before the literal's own quote or another backslash and keeps every other escape as written. The decoded reading
// also reads a literal that opens with three of its quote as Python reads a triple-quoted one (shellWriteTripleBody) and folds the
// doubled braces of a field-free f-string (shellWriteTripleFold); the earlier reading keeps the single-quote walk it always had.
func shellWriteEscapeLiteral(arg []rune, earlier bool) (string, bool) {
	i, raw, isBytes, isF := 0, false, false, false
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	for ; i < len(arg) && strings.ContainsRune("rRuUbBfF", arg[i]); i++ {
		raw = raw || arg[i] == 'r' || arg[i] == 'R'
		isBytes = isBytes || arg[i] == 'b' || arg[i] == 'B'
		isF = isF || arg[i] == 'f' || arg[i] == 'F'
	}
	if i >= len(arg) || arg[i] != '\'' && arg[i] != '"' {
		return "", false
	}
	quote := arg[i]
	body, ok := shellWriteTripleBody(arg, i, quote, !earlier && i+2 < len(arg) && arg[i+1] == quote && arg[i+2] == quote)
	if !ok {
		return "", false
	}
	if raw {
		return string(shellWriteTripleFold(body, arg, isF && !earlier)), true
	}
	if earlier {
		return shellWriteEscapeUnquote(body, quote), true
	}
	return shellWriteEscapePython(shellWriteTripleFold(body, arg, isF), isBytes)
}

// shellWriteTripleBody is the body of the string literal whose opening quote is arg[i] and whether it closes with spaces alone
// after it: with triple false the body runs to the first unescaped arg[i], with triple true to the first unescaped run of three
// arg[i], and a character after a backslash never closes it. A single-quoted body is a slice of arg, so it is not copied; a
// triple-quoted body is copied only when its physical line breaks need normalising (shellWriteTripleScanNewlines).
func shellWriteTripleBody(arg []rune, i int, quote rune, triple bool) ([]rune, bool) {
	open := i + 1
	if triple {
		open = i + 3
	}
	for k := open; k < len(arg); k++ {
		switch c := arg[k]; {
		case c == '\\':
			k++ // the character after a backslash never closes the literal
		case c == quote && (!triple || k+2 < len(arg) && arg[k+1] == quote && arg[k+2] == quote):
			after := k + 1
			if triple {
				after = k + 3
			}
			for _, rest := range arg[after:] {
				if !shellVerbSpaceRune(rest) {
					return nil, false
				}
			}
			if triple {
				return shellWriteTripleScanNewlines(arg[open:k]), true
			}
			return arg[open:k], true
		}
	}
	return nil, false
}

// shellWriteTripleScanNewlines is a triple-quoted body with the physical line breaks Python's source decoding gives it: a CRLF
// and a lone CR both read as LF before the literal is evaluated, so a destination written with either names the path Python
// writes. A body with no CR is returned as it is; otherwise the copy is one pass.
func shellWriteTripleScanNewlines(body []rune) []rune {
	at := slices.Index(body, '\r')
	if at < 0 {
		return body
	}
	out := append(make([]rune, 0, len(body)), body[:at]...)
	for ; at < len(body); at++ {
		switch {
		case body[at] != '\r':
			out = append(out, body[at])
		case at+1 < len(body) && body[at+1] == '\n':
			out = append(out, '\n')
			at++
		default:
			out = append(out, '\n')
		}
	}
	return out
}

// shellWriteTripleScanRegion is the index just past the string region that opens at the quote rs[i], as Python reads it: a
// triple-quoted literal (the quote repeated three times) runs to the first unescaped run of three of the same quote, and a
// single-quoted region to the first unescaped quote; a character after a backslash closes neither. An unterminated region ends
// at len(rs). The scanners shellVerbOpenWritesIn and shellVerbWithoutComments use it so a triple-quoted literal is one region
// instead of a run of single-quoted ones. python selects the triple-quoted rule; with python false the region is one quote
// wide, which is what a JavaScript program needs (its three quotes are three empty strings, not a Python triple quote).
// A literal whose prefix holds an f is not one region: its body is walked as Python reads it (shellWriteFStringRegion), so a
// replacement field's expression is reached by the scanners (CRW-741).
func shellWriteTripleScanRegion(rs []rune, i int, python bool) int {
	if python && shellWriteFStringPrefix(rs, i) {
		end, _, _ := shellWriteFStringRegion(rs, i, 0)
		return end
	}
	quote := rs[i]
	triple := python && i+2 < len(rs) && rs[i+1] == quote && rs[i+2] == quote
	k := i + 1
	if triple {
		k = i + 3
	}
	for ; k < len(rs); k++ {
		switch c := rs[k]; {
		case c == '\\':
			k++
		case c == quote && (!triple || k+2 < len(rs) && rs[k+1] == quote && rs[k+2] == quote):
			if triple {
				return k + 3
			}
			return k + 1
		}
	}
	return len(rs)
}

// shellWriteFStringMaxDepth bounds the replacement-field nesting the walk reads: a deeper one is unreadable, so the memory
// gate fails closed rather than reading a program it cannot finish (CRW-741, criterion c2).
const shellWriteFStringMaxDepth = 32

// shellWriteFStringUnreadableWhat is the what shellWriteFStringUnreadable reports for a program it cannot read (the deny
// reason names it as "(a program the gate cannot read: " + what + ")"; CRW-741, criterion c2).
const shellWriteFStringUnreadableWhat = "a Python f-string replacement field"

// shellWriteFStringPrefix reports whether the string literal whose opening quote stands at rs[i] carries an f (or F) in its
// prefix (f, F, rf, fr, Rf, fR and the other case mixes), so its body is walked as Python reads it rather than skipped as one
// region. A run of prefix letters that is the tail of a longer name (a variable ending in r, u, b or f) is not a prefix.
func shellWriteFStringPrefix(rs []rune, i int) bool {
	j, f := i, false
	for j > 0 && strings.ContainsRune("rRuUbBfF", rs[j-1]) {
		j--
		if rs[j] == 'f' || rs[j] == 'F' {
			f = true
		}
	}
	if !f {
		return false
	}
	if j == 0 {
		return true
	}
	b := rs[j-1]
	return !(b >= 128 || b == '_' || shellVerbLetter(byte(b), true))
}

// shellWriteFStringRegion reads the f-string literal whose opening quote stands at rs[i] as Python reads it: the literal runs
// to the first closing quote (three of the same quote for a triple-quoted one) outside every replacement field, literal text
// and the doubled {{ and }} are skipped, and each field's expression span is returned so the caller can read it as program
// text. depth counts the enclosing f-strings and fields. bad reports a field the walk cannot read: one not closed before the
// literal or the program ends, an unpaired } outside a field, or nesting deeper than shellWriteFStringMaxDepth. A literal
// without an f in its prefix is not this function's case (shellWriteTripleScanRegion keeps the one-region rule for it).
func shellWriteFStringRegion(rs []rune, i, depth int) (end int, fields [][2]int, bad bool) {
	if depth > shellWriteFStringMaxDepth {
		return len(rs), nil, true
	}
	quote := rs[i]
	triple := i+2 < len(rs) && rs[i+1] == quote && rs[i+2] == quote
	k := i + 1
	if triple {
		k = i + 3
	}
	for k < len(rs) {
		switch c := rs[k]; {
		case c == '\\':
			// A backslash escapes the next character for the string's own escapes, but it never escapes a brace:
			// {{ and }} are the only brace escapes in an f-string, so a brace after a backslash still opens or
			// closes a field (rf'\{x}' holds a field, not a literal brace).
			if k+1 < len(rs) && rs[k+1] != '{' && rs[k+1] != '}' {
				k += 2
			} else {
				k++
			}
		case c == '{':
			if k+1 < len(rs) && rs[k+1] == '{' {
				k += 2
				continue
			}
			var more [][2]int
			k, more, bad = shellWriteFStringField(rs, k+1, depth)
			fields = append(fields, more...)
			if bad {
				return len(rs), fields, true
			}
		case c == '}':
			if k+1 < len(rs) && rs[k+1] == '}' {
				k += 2
				continue
			}
			return len(rs), fields, true
		case c == quote:
			if !triple || k+2 < len(rs) && rs[k+1] == quote && rs[k+2] == quote {
				if triple {
					return k + 3, fields, false
				}
				return k + 1, fields, false
			}
			k++
		default:
			k++
		}
	}
	return len(rs), fields, true
}

// shellWriteFStringField reads one replacement field whose text begins just after its { at rs[from]. Its expression ends at a
// same-depth ! (a conversion; != is not one), : (a format spec) or }, and brackets, string literals (the outer quote included,
// PEP 701) and an inner f-string inside it are followed. It returns the index just past the field's closing }, the expression
// spans to read as program text, and whether the field is unreadable.
func shellWriteFStringField(rs []rune, from, depth int) (next int, fields [][2]int, bad bool) {
	if depth > shellWriteFStringMaxDepth {
		return len(rs), nil, true
	}
	k, bracket := from, 0
	for k < len(rs) {
		switch c := rs[k]; {
		case c == '\\':
			// In a replacement field a backslash is a line continuation (or part of a string literal, read by the
			// quote case below); it never hides a delimiter.
			if k+1 < len(rs) && (rs[k+1] == '\n' || rs[k+1] == '\r') {
				k += 2
			} else {
				k++
			}
		case c == '#':
			// Python 3.12+ allows a comment inside a multi-line replacement field: it runs to the end of the line and
			// is no part of the expression, so a quote or brace inside it is not live syntax.
			for k < len(rs) && rs[k] != '\n' {
				k++
			}
		case c == '\'' || c == '"':
			if shellWriteFStringPrefix(rs, k) {
				end, _, innerBad := shellWriteFStringRegion(rs, k, depth+1)
				if innerBad {
					return len(rs), fields, true
				}
				k = end
				continue
			}
			k = shellWriteTripleScanRegion(rs, k, true)
		case c == '(' || c == '[' || c == '{':
			bracket++
			k++
		case c == ')' || c == ']':
			if bracket > 0 {
				bracket--
			}
			k++
		case c == '}':
			if bracket > 0 {
				bracket--
				k++
				continue
			}
			fields = append(fields, [2]int{from, k})
			return k + 1, fields, false
		case bracket == 0 && c == '!' && (k+1 >= len(rs) || rs[k+1] != '='):
			fields = append(fields, [2]int{from, k})
			return shellWriteFStringSpec(rs, k+2, depth, fields)
		case bracket == 0 && c == ':':
			fields = append(fields, [2]int{from, k})
			return shellWriteFStringSpec(rs, k+1, depth, fields)
		default:
			k++
		}
	}
	return len(rs), fields, true
}

// shellWriteFStringSpec reads a field's format spec from rs[from] to the field's closing }, collecting the expression spans
// of any {...} fields the spec holds; fields already carries the field's own expression.
func shellWriteFStringSpec(rs []rune, from, depth int, fields [][2]int) (next int, out [][2]int, bad bool) {
	k := from
	for k < len(rs) {
		switch c := rs[k]; {
		case c == '\\':
			// In a format spec a backslash is literal text, so a brace after it still opens a nested field.
			k++
		case c == '}':
			return k + 1, fields, false
		case c == '{':
			var more [][2]int
			k, more, bad = shellWriteFStringField(rs, k+1, depth+1)
			fields = append(fields, more...)
			if bad {
				return len(rs), fields, true
			}
		default:
			k++
		}
	}
	return len(rs), fields, true
}

// shellWriteFStringUnreadable reports whether a command holds a Python program with an f-string replacement field the walk
// cannot read (shellWriteFStringRegion). It looks at the same programs ShellWriteDestinations reads as Python - the
// python -c and --command programs of a segment, including the ones a nested shell -c or eval runs - and returns what the
// deny reason names (CRW-741, criterion c2).
func shellWriteFStringUnreadable(command string) (string, bool) {
	budget := 32*len(command) + 65536
	for _, segment := range splitShellSegments(stripHeredocBodies(utf16.Encode([]rune(command)))) {
		if what, ok := shellWriteFStringUnreadableIn(shellString(segment), &budget); ok {
			return what, true
		}
	}
	return "", false
}

// shellWriteFStringUnreadableIn scans one segment's commands for a Python program and reports the first unreadable
// f-string, over both the program and its shell-unescaped reading, as shellVerbPythonNode reads them. A command a nested
// shell -c or eval runs is read again within the shared budget, the way shellVerbNested reads it for the destinations.
func shellWriteFStringUnreadableIn(segment string, budget *int) (string, bool) {
	for _, command := range shellVerbSubsegments(segment) {
		tokens := shellVerbSkipWrappers(shellTokenize(command))
		if script, ok := shellWriteFStringPythonScript(tokens); ok {
			if what, bad := shellWriteFStringUnreadableProgram(script); bad {
				return what, true
			}
			continue
		}
		nested, ok := shellWriteFStringNestedScript(tokens)
		if !ok {
			continue
		}
		if *budget -= len(nested); *budget < 0 {
			continue
		}
		if what, bad := shellWriteFStringUnreadableIn(nested, budget); bad {
			return what, true
		}
	}
	return "", false
}

// shellWriteFStringNestedScript is the command string a shell -c or eval runs, as shellVerbRun reads it, so the fail-closed
// scan reaches a Python program one level down the way the destination walk does.
func shellWriteFStringNestedScript(tokens []string) (string, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	verb, args := shellVerbName(tokens[0]), tokens[1:]
	if shellVerbIsShell(verb) {
		return shellVerbShellScript(args)
	}
	if verb != "eval" {
		return "", false
	}
	for { // eval builtin eval X runs X: a chain is peeled here, not read level by level
		next := shellVerbSkipWrappers(args)
		if len(next) == 0 || shellVerbName(next[0]) != "eval" {
			break
		}
		args = next[1:]
	}
	return strings.Join(args, " "), true
}

// shellWriteFStringUnreadableProgram reports the what of a Python program the reader cannot finish: an f-string replacement
// field it cannot read (CRW-741) in either reading, and a program passed to exec, eval or compile whose first argument is no
// string literal (CRW-754) in every reading the token allows.
func shellWriteFStringUnreadableProgram(script string) (string, bool) {
	if what, bad := shellWriteFStringProgramUnreadable(script); bad {
		return what, true
	}
	un := shellVerbUnescape(script)
	if un != script {
		if what, bad := shellWriteFStringProgramUnreadable(un); bad {
			return what, true
		}
	}
	return shellWriteExecUnreadableProgram(script, un)
}

// shellWriteFStringPythonScript is the program of a python -c/--command command, as shellVerbPythonNode reads it: a bundled
// -c (with no -m, -W or -X) and a versioned interpreter name (python3.11) count too.
func shellWriteFStringPythonScript(tokens []string) (string, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	verb := shellVerbName(tokens[0])
	if verb != "python" && verb != "python3" && verb != "py" && !shellVerbVersioned(verb) {
		return "", false
	}
	args := tokens[1:]
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "--command":
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			return a[2:], true
		case shellVerbBundleEnds(a, 'c', false) && !strings.ContainsAny(a, "mWXQ"):
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// shellWriteFStringProgramUnreadable reports the what of one Python program the reader cannot finish: an f-string
// replacement field it cannot read (CRW-741), or a program passed to exec, eval or compile whose first argument is no
// string literal, or one nested deeper than shellWriteExecMaxDepth (CRW-754). The f-string scan keeps CRW-741's own rule -
// every quote position of the program is examined - and the exec check (shellWriteExecUnreadableProgram) adds the programs a
// literal passed to exec, eval or compile names.
func shellWriteFStringProgramUnreadable(program string) (string, bool) {
	rs := shellVerbWithoutComments(program, true)
	for i := 0; i < len(rs); i++ {
		if c := rs[i]; (c == '\'' || c == '"') && shellWriteFStringPrefix(rs, i) {
			end, _, bad := shellWriteFStringRegion(rs, i, 0)
			if bad {
				return shellWriteFStringUnreadableWhat, true
			}
			i = end - 1
		}
	}
	return "", false
}

// shellWriteExecUnreadableProgram is the exec fail-closed reason of one Python program: the reason only when every reading
// the reader considers - the program as the token holds it (program) and again with its shell escapes removed (unescaped) -
// reports it. A reading that reads the call's first argument as a string literal names the writes inside it, so a reason
// taken from the other reading alone would deny a program this reader can read (CRW-754 review: shellVerbUnescape turns a
// valid literal with an escaped quote into a reading whose quotes no longer pair, so only the unescaped reading looks
// unreadable).
func shellWriteExecUnreadableProgram(program, unescaped string) (string, bool) {
	_, what := shellWriteExecScan(shellVerbWithoutComments(program, true), true, 0)
	if what == "" {
		return "", false
	}
	if unescaped == program {
		return what, true
	}
	if _, other := shellWriteExecScan(shellVerbWithoutComments(unescaped, true), true, 0); other == "" {
		return "", false
	}
	return what, true
}

// shellWriteTripleFold is the body of a field-free f-string literal with its doubled braces folded to single ones, as Python folds
// them before it decodes escapes; any other body is returned unchanged. shellWriteEscapeField decides whether a replacement field
// is present, so the fold never fires on a body the Path join already treats as dynamic.
func shellWriteTripleFold(body, arg []rune, f bool) []rune {
	if !f || shellWriteEscapeField(arg) {
		return body
	}
	i := 0
	for i+1 < len(body) && !(body[i] == body[i+1] && (body[i] == '{' || body[i] == '}')) {
		i++
	}
	if i+1 >= len(body) {
		return body
	}
	out := append([]rune(nil), body[:i]...)
	for ; i < len(body); i++ {
		if i+1 < len(body) && body[i] == body[i+1] && (body[i] == '{' || body[i] == '}') {
			out = append(out, body[i])
			i++
			continue
		}
		out = append(out, body[i])
	}
	return out
}

// shellWriteEscapeUnquote is the earlier reading of a non-raw literal's body.
func shellWriteEscapeUnquote(body []rune, quote rune) string {
	out := make([]rune, 0, len(body))
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			if i++; body[i] != quote && body[i] != '\\' {
				out = append(out, '\\')
			}
		}
		out = append(out, body[i])
	}
	return string(out)
}

// shellWriteEscapePath is what a Path(...) call names: posixpath.join of its string literal arguments (a trailing comma leaves a
// blank). An absolute part discards the parts before it and nothing else is normalized, so Path("/m", "") is "/m/". An argument
// that is no literal, or an f-string with a field, leaves the rest of the path unknown, so the call names the literal prefix, the
// directory the write lands under (Path("/m", name) is "/m"), until an absolute literal part starts the path over; no known prefix
// names nothing. Such a call, and one with a single argument, also keeps the earlier reading, its first argument when that is a
// literal, read as that reading did (shellWriteEscapeLiteral), so the join never names fewer destinations than before; only a call
// of several literals names the join alone. The path grows in one buffer, so the work is linear in the arguments.
func shellWriteEscapePath(rs []rune, spans [][2]int) []string {
	var path []byte
	known, dynamic, head, parts := true, false, "", 0
	for _, span := range spans {
		if shellVerbBlank(rs[span[0]:span[1]]) {
			continue
		}
		part, ok := shellVerbLiteral(rs[span[0]:span[1]])
		if parts++; parts == 1 {
			head, _ = shellWriteEscapeLiteral(rs[span[0]:span[1]], true)
		}
		dynamic = dynamic || !ok || shellWriteEscapeField(rs[span[0]:span[1]])
		switch {
		case !ok:
			known = false
		case strings.HasPrefix(part, "/"):
			path, known = append(path[:0], part...), true
		case !known:
		case len(path) == 0 || path[len(path)-1] == '/':
			path = append(path, part...)
		default:
			path = append(append(path, '/'), part...)
		}
	}
	names := []string{}
	if len(path) > 0 {
		names = append(names, string(path))
	}
	if (dynamic || parts == 1) && head != "" && head != string(path) {
		names = append(names, head)
	}
	return names
}

// shellWriteEscapeField reports whether arg is an f-string literal with a replacement field: a brace that is not doubled.
func shellWriteEscapeField(arg []rune) bool {
	i, f := 0, false
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	for ; i < len(arg) && strings.ContainsRune("rRuUbBfF", arg[i]); i++ {
		f = f || arg[i] == 'f' || arg[i] == 'F'
	}
	for ; f && i < len(arg); i++ {
		if arg[i] == '{' {
			if i+1 == len(arg) || arg[i+1] != '{' {
				return true
			}
			i++
		}
	}
	return false
}

// shellWriteEscapePython decodes the body of a non-raw Python string literal: a line continuation (a CR or CRLF is a newline to
// Python as well), the one-character escapes, octal of one to three digits, \xhh, and in a str literal \uXXXX and \UXXXXXXXX. An
// unknown escape keeps its backslash. It reports false for what Python rejects (a short or oversized \x, \u or \U) and for a \N
// escape, which needs the Unicode name table, and for a NUL, which no path holds. A str value that is a surrogate is the byte
// os.fsencode makes of U+DC80 to U+DCFF (surrogateescape) and no path at all otherwise, since Python cannot encode it. In a b
// literal \u, \U and \N are plain text, an escape is a byte (octal wraps at eight bits) and is written as that byte.
func shellWriteEscapePython(body []rune, isBytes bool) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 == len(body) {
			out.WriteRune(body[i])
			continue
		}
		i++
		c, value := body[i], -1
		switch {
		case c == '\n': // a line continuation
		case c == '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
		case c == '\\' || c == '\'' || c == '"':
			value = int(c)
		case strings.ContainsRune("abfnrtv", c):
			value = int("\a\b\f\n\r\t\v"[strings.IndexRune("abfnrtv", c)])
		case c >= '0' && c <= '7':
			value = int(c - '0')
			for n := 0; n < 2 && i+1 < len(body) && body[i+1] >= '0' && body[i+1] <= '7'; n++ {
				i++
				value = value*8 + int(body[i]-'0')
			}
		case c == 'x' || !isBytes && (c == 'u' || c == 'U'):
			width := []int{2, 4, 8}[strings.IndexRune("xuU", c)]
			var ok bool
			if value, ok = shellWriteEscapeDigits(body, i+1, width); !ok {
				return "", false
			}
			i += width
		case c == 'N' && !isBytes:
			return "", false
		default:
			out.WriteByte('\\')
			out.WriteRune(c)
		}
		if isBytes && value > 0xff {
			value &= 0xff
		}
		switch {
		case value == 0:
			return "", false
		case value >= 0xd800 && value <= 0xdfff && !isBytes:
			if value < 0xdc80 || value > 0xdcff {
				return "", false
			}
			out.WriteByte(byte(value - 0xdc00))
		case value > 0 && isBytes:
			out.WriteByte(byte(value))
		case value > 0:
			out.WriteRune(rune(value))
		}
	}
	return out.String(), true
}

// shellWriteEscapeDigits is the value of the n hexadecimal digits of rs from index from; false when they are not all digits or
// the value is past the Unicode range.
func shellWriteEscapeDigits(rs []rune, from, n int) (int, bool) {
	if n < 1 || from+n > len(rs) {
		return 0, false
	}
	value := 0
	for _, r := range rs[from : from+n] {
		switch {
		case r >= '0' && r <= '9':
			value = value*16 + int(r-'0')
		case r >= 'a' && r <= 'f':
			value = value*16 + int(r-'a') + 10
		case r >= 'A' && r <= 'F':
			value = value*16 + int(r-'A') + 10
		default:
			return 0, false
		}
		if value > 0x10ffff {
			return 0, false
		}
	}
	return value, true
}

// shellWriteEscapeJSWrites reads the first argument of each writeFile*, appendFile* and createWriteStream call as JavaScript reads
// a string or template literal, and names the decoded value unless have holds it already. The oracle's lazy dot stops at an
// escaped quote and never crosses a line terminator, so a path hidden by an escape (or a line continuation) is not in its raw
// text. A quoted literal holds no raw LF or CR; a template does and keeps an escape's backslash pair together.
func shellWriteEscapeJSWrites(script string, have []string) []string {
	quoted := func(q string) string { return q + `((?:[^` + q + `\\\n\r]|\\(?:\r\n|[\s\S]))*)` + q }
	pattern := `\b(?:writeFileSync|writeFile|appendFileSync|appendFile|createWriteStream)` + lintSpace + `*\(` + lintSpace + `*(?:` +
		quoted("'") + "|" + quoted("\"") + `|\x60((?:[^\x60\\]|\\(?:\r\n|[\s\S]))*)\x60)`
	seen := make(map[string]struct{}, len(have))
	for _, path := range have {
		seen[path] = struct{}{}
	}
	out := []string{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(script, -1) {
		decoded, ok := shellWriteEscapeJS(m[1]+m[2]+m[3], m[3] != "")
		if _, found := seen[decoded]; ok && decoded != "" && !found {
			seen[decoded] = struct{}{}
			out = append(out, decoded)
		}
	}
	return out
}

// shellWriteEscapeJS decodes the body of a JavaScript string or template literal: \xhh, \uXXXX, \u{...}, legacy octal (up to
// three digits when the first is 0 to 3, else two; \0 alone is NUL), \b \f \n \r \t \v, a line continuation (LF, CR, CRLF, U+2028,
// U+2029), and any other \c as c. The value is built from UTF-16 units, so a surrogate pair joins and a lone surrogate becomes
// U+FFFD as everywhere in this package. A template reads a raw CR or CRLF as LF and, with an unescaped dollar-brace placeholder,
// is no literal; a malformed \x, \u or a code point past U+10FFFF makes either kind no literal.
func shellWriteEscapeJS(body string, template bool) (string, bool) {
	rs, units := []rune(body), []uint16{}
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '$' && template && i+1 < len(rs) && rs[i+1] == '{':
			return "", false
		case c == '\r' && template:
			units = append(units, '\n')
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
		case c != '\\' || i+1 == len(rs):
			units = utf16.AppendRune(units, c)
		default:
			i++
			switch d := rs[i]; {
			case d == '\n' || d == '\u2028' || d == '\u2029': // a line continuation
			case d == '\r':
				if i+1 < len(rs) && rs[i+1] == '\n' {
					i++
				}
			case strings.ContainsRune("bfnrtv", d):
				units = append(units, uint16("\b\f\n\r\t\v"[strings.IndexRune("bfnrtv", d)]))
			case d == 'x' || d == 'u' && (i+1 == len(rs) || rs[i+1] != '{'):
				width := 2 + 2*strings.IndexRune("xu", d)
				value, ok := shellWriteEscapeDigits(rs, i+1, width)
				if !ok {
					return "", false
				}
				units, i = append(units, uint16(value)), i+width
			case d == 'u':
				end := slices.Index(rs[i:], '}')
				value, ok := shellWriteEscapeDigits(rs, i+2, end-2)
				if !ok {
					return "", false
				}
				if value > 0xffff {
					units = utf16.AppendRune(units, rune(value))
				} else { // a surrogate stays a unit, so \u{d83d}\u{de00} joins as a pair does
					units = append(units, uint16(value))
				}
				i += end
			case d >= '0' && d <= '7':
				value := int(d - '0')
				for more := 2 - value/4; more > 0 && i+1 < len(rs) && rs[i+1] >= '0' && rs[i+1] <= '7'; more-- {
					i++
					value = value*8 + int(rs[i]-'0')
				}
				units = append(units, uint16(value))
			default:
				units = utf16.AppendRune(units, d)
			}
		}
	}
	return shellString(units), true
}
