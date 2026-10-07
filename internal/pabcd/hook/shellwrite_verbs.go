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
	alias    map[string][]string
	from     map[string][]string
	importer map[string]bool // a bare name bound to importlib.import_module, which reaches a module this reader cannot name
	opener   map[string]bool // a bare name bound to the open builtin through another module (from io import open as o)
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
	out := shellWriteCopyImports{alias: map[string][]string{}, from: map[string][]string{}, importer: map[string]bool{}, opener: map[string]bool{}}
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
		for name := range binds.importer {
			out.importer[name] = true
		}
		for name := range binds.opener {
			out.opener[name] = true
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
	binds := shellWriteCopyImportsMerge(outer, shellWriteCopyImports{alias: map[string][]string{}, from: map[string][]string{}, importer: map[string]bool{}, opener: map[string]bool{}})
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
			if module == "importlib" && item[0] == "import_module" {
				if binds.importer == nil {
					binds.importer = map[string]bool{}
				}
				binds.importer[name] = true
			}
			if item[0] == "open" {
				// from io import open as o, from builtins import open: the name is the open builtin through
				// another module, and the reader names no destination for such a value.
				if binds.opener == nil {
					binds.opener = map[string]bool{}
				}
				binds.opener[name] = true
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
	return shellVerbOpenNames(shellVerbOpenArgs(rs, spans))
}

// shellVerbOpenArgs splits an open() call's arguments the way Python binds them: the path is the first positional
// argument or file=, and the mode is the second positional argument or mode=, in either order and with other keywords
// between. The spans are the argument ranges the walk recorded, so an argument that is no literal stays the text it was
// written as.
func shellVerbOpenArgs(rs []rune, spans [][2]int) (path, mode []rune) {
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
	return path, mode
}

// shellVerbOpenNames is the destination an open() call names from its already-split arguments: the decoded reading
// (shellVerbLiteral) is added to the earlier one, which kept escapes other than a backslash and the quote as written, and
// neither replaces the other: each reading names its own path when its own mode writes. A mode written with a \N{name}
// escape cannot be decoded (it needs the Unicode name table), so it counts as writing for both readings.
func shellVerbOpenNames(path, mode []rune) []string {
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

// CRW-951 adds the memory gate's third fail-closed check, beside CRW-741's f-string replacement field and CRW-726's
// unreadable program: a Python program that holds a write whose destination the reader cannot name, while the program
// can point at the protected area, is a write attempt of its own. The reader (ShellWriteDestinations and the hardened
// program walk) names string-literal destinations only, so a program that computes its destination - open(Path(m), "w"),
// open(os.path.join(root, "n.md"), "w"), Path(root).joinpath("n.md").write_text(...), a chained receiver, a write
// function used as a value, getattr(..., "copy"), __import__("shutil") or importlib.import_module - writes into the
// memories root without a grant. The oracle's scriptWriteDestinations names the same literal destinations only, so it
// allows the same programs: this is a security fix (port: fixed). A program with no such write, and one that cannot
// point at the protected area, is allowed exactly as before, and a session that holds a grant is allowed as before. The
// over-blocking this accepts - a program that names the protected area and writes elsewhere through a computed path is
// refused - is recorded in docs/port-cxc/known-defects/CRW-951.md.

// shellWriteUnnamedWhat is the what this check reports; the deny reason names it as
// "(a program the gate cannot read: " + what + ")".
const shellWriteUnnamedWhat = "a write whose destination it cannot name"

// shellWriteUnnamedCommand reports the what of this check for one shell command: the first Python program the command
// runs that holds a write whose destination the reader cannot name while the program can point at the protected area.
// points says whether a string literal of a program can point at the protected area; the caller resolves the protected
// root. The programs read are the -c and --command programs of a python command, including the ones a nested shell -c
// or eval runs and the shell-unescaped reading of each, and the body of a here-document the interpreter reads.
func shellWriteUnnamedCommand(command string, points func(string) bool) (string, bool) {
	for _, program := range shellWriteUnnamedPrograms(command, points) {
		if program.unreadable || shellWriteUnnamedProgram(program.text, points) {
			return shellWriteUnnamedWhat, true
		}
	}
	return "", false
}

// shellWriteUnnamedSource is one Python program text the reader reads, or a program position it cannot read at all: the
// body of a here-document whose unquoted delimiter lets the outer shell rewrite it is not the text the interpreter runs,
// so the reader cannot read it and the check fails closed.
type shellWriteUnnamedSource struct {
	text       string
	unreadable bool
}

// shellWriteUnnamedPrograms is every Python program text a command holds that the reader reads, in the order the command
// names them.
func shellWriteUnnamedPrograms(command string, points func(string) bool) []shellWriteUnnamedSource {
	budget := &shellWriteUnnamedBudget{bytes: 32*len(command) + 65536}
	out := []shellWriteUnnamedSource{}
	for _, segment := range splitShellSegments(stripHeredocBodies(utf16.Encode([]rune(command)))) {
		out = shellWriteUnnamedSegment(shellString(segment), budget, points, out)
	}
	return shellWriteUnnamedHerePrograms(command, budget, points, out)
}

// shellWriteUnnamedBudget bounds the work this check does on one command: the bytes of the nested programs it reads and
// how deep a nested shell program or here-document may stand, so a hostile command cannot make the reader recurse
// without end. bytes is spent as nested text is read; depth counts the nesting the reader is inside.
type shellWriteUnnamedBudget struct {
	bytes int
	depth int
}

// shellWriteUnnamedMaxDepth is the deepest nesting of nested shell programs and here-documents this check reads.
const shellWriteUnnamedMaxDepth = 16

// take spends n bytes of the budget and reports what is left; a negative result means the budget is spent.
func (b *shellWriteUnnamedBudget) take(n int) int {
	b.bytes -= n
	return b.bytes
}

// shellWriteUnnamedSegment collects the programs of one shell segment's commands: the -c or --command program of a
// python command, over both the token as it stands and its shell-unescaped reading as shellVerbPythonNode reads them,
// and the commands a nested shell -c or eval runs, including the here-documents that nested program reads.
func shellWriteUnnamedSegment(segment string, budget *shellWriteUnnamedBudget, points func(string) bool, out []shellWriteUnnamedSource) []shellWriteUnnamedSource {
	for _, command := range shellVerbSubsegments(segment) {
		// A redirection may stand before the verb (2>/dev/null python3 -c ...), so the redirection words go first:
		// the verb and its program are read from what is left.
		tokens := shellWriteUnnamedCommandWords(shellTokenize(command))
		if script, ok := shellWriteFStringPythonScript(tokens); ok {
			out = append(out, shellWriteUnnamedSource{text: script})
			if un := shellVerbUnescape(script); un != script {
				out = append(out, shellWriteUnnamedSource{text: un})
			}
			continue
		}
		nested, ok := shellWriteFStringNestedScript(tokens)
		if !ok {
			continue
		}
		out = shellWriteUnnamedNested(nested, budget, points, out)
	}
	return out
}

// shellWriteUnnamedNested reads one nested shell program: its own commands (a python -c it runs, a shell -c or eval
// inside it) and the here-documents it reads. The budget bounds the nesting and the bytes read, so a command that nests
// shell programs without end cannot make the reader recurse without end.
func shellWriteUnnamedNested(program string, budget *shellWriteUnnamedBudget, points func(string) bool, out []shellWriteUnnamedSource) []shellWriteUnnamedSource {
	if budget.depth >= shellWriteUnnamedMaxDepth || budget.take(len(program)) < 0 {
		return out
	}
	budget.depth++
	out = shellWriteUnnamedSegment(program, budget, points, out)
	out = shellWriteUnnamedHerePrograms(program, budget, points, out)
	if un := shellVerbUnescape(program); un != program {
		// A nested program the outer shell quoted keeps its backslash escapes in the text the tokenizer retained
		// (bash -c "python3 -c \"...\""), so the escaped reading is scanned too: without it the second
		// tokenization cuts the -c operand at the first escaped quote and reads only part of the program.
		out = shellWriteUnnamedSegment(un, budget, points, out)
		out = shellWriteUnnamedHerePrograms(un, budget, points, out)
	}
	budget.depth--
	return out
}

// shellWriteUnnamedHerePrograms appends the Python programs the reader reads out of the here-documents of a command: a
// here-document whose command is a Python interpreter reading its program from standard input runs its body as the
// program, and one whose command is a shell runs its body as a command, so a python -c inside it is read too. A body
// whose closing delimiter line is missing never ends, and the unreadable-program check already refuses it (CRW-726). An
// unquoted delimiter lets the outer shell rewrite the body before the interpreter reads it, so a destination the body
// spells as a literal is not the text the interpreter runs (a $VAR may expand into a protected path): when such a body
// holds a write, the check fails closed on it. A body with an expansion and no write at all is read as the program it
// is, so a read-only program is not refused for naming an unrelated variable.
func shellWriteUnnamedHerePrograms(command string, budget *shellWriteUnnamedBudget, points func(string) bool, out []shellWriteUnnamedSource) []shellWriteUnnamedSource {
	for i := 0; i < len(command); {
		line, after := worktreeDelUnreadableLine(command, i)
		ops := worktreeDelUnreadableHereOperators(line)
		if len(ops) == 0 {
			i = after
			continue
		}
		bodies, next := worktreeDelUnreadableHereBodies(command, after, ops)
		for _, body := range bodies {
			if !body.closed {
				continue
			}
			switch shellWriteUnnamedHereOwner(line, body.op.at) {
			case "python":
				if !body.op.literal && worktreeDelUnreadableHereExpansion(body.body) {
					// The outer shell rewrites this body before the interpreter reads it, so a destination the
					// reader reads here is not the one the interpreter uses. The reader still asks the check's
					// two questions of the body: a write whose destination it cannot name while the body can
					// point at the protected area, or any write at all while the body can point at it (a literal
					// the shell rewrites is no trustworthy destination). A body that writes nothing, or cannot
					// point at the protected area, is read as the program it is.
					_, wrote, pointed := shellWriteUnnamedRead(body.body, points)
					if pointed && wrote {
						out = append(out, shellWriteUnnamedSource{unreadable: true})
						continue
					}
				}
				out = append(out, shellWriteUnnamedSource{text: body.body})
			case "shell":
				out = shellWriteUnnamedNested(body.body, budget, points, out)
			}
		}
		i = next
	}
	return out
}

// shellWriteUnnamedHereOwner says what the command that owns the here-document operator at byte at on line runs:
// "python" when its verb is a Python interpreter that reads its program from that here-document, "shell" when it is a
// shell, and "" for every other command. A redirection may stand in front of the verb (0<<'PY' python3), so the
// redirection words go first, and an interpreter with a script operand or -m runs that instead, its here-document being
// only the program's standard input.
func shellWriteUnnamedHereOwner(line string, at int) string {
	start, end := shellWriteUnnamedOwnerStart(line, at), shellWriteUnnamedOwnerEnd(line, at)
	tokens := shellWriteUnnamedCommandWords(shellTokenize(line[start:end]))
	if len(tokens) == 0 {
		return ""
	}
	if _, hasC := shellWriteFStringPythonScript(tokens); hasC {
		return ""
	}
	verb := shellVerbName(tokens[0])
	if shellWriteUnnamedPython(verb) {
		if !shellWriteUnnamedPythonStdin(tokens[1:]) {
			return ""
		}
		return "python"
	}
	if shellVerbIsShell(verb) {
		return "shell"
	}
	return ""
}

// shellWriteUnnamedOwnerStart is the byte the command that owns the here-document operator at byte at begins at: the
// last unquoted command separator before it, plus one. A separator inside a quote (python3 -W "a;b" <<'PY') belongs to
// the word that holds it and cuts nothing, so the verb is still read.
func shellWriteUnnamedOwnerStart(line string, at int) int {
	r, start := &worktreeDelQuoteReader{}, 0
	for i := 0; i < at && i < len(line); i++ {
		if r.escapes(line, i) {
			i++
			r.pair()
			continue
		}
		if r.state == worktreeDelQuotePlain && shellWriteUnnamedSeparatorAt(line, i) {
			start = i + 1
		}
		r.step(line[i])
	}
	return start
}

// shellWriteUnnamedOwnerEnd is the byte the command that owns the here-document operator at byte at ends at: the next
// unquoted command separator at or after it.
func shellWriteUnnamedOwnerEnd(line string, at int) int {
	r := &worktreeDelQuoteReader{}
	for i := 0; i < at && i < len(line); i++ {
		if r.escapes(line, i) {
			i++
			r.pair()
			continue
		}
		r.step(line[i])
	}
	for i := at; i < len(line); i++ {
		if r.escapes(line, i) {
			i++
			r.pair()
			continue
		}
		if r.state == worktreeDelQuotePlain && shellWriteUnnamedSeparatorAt(line, i) {
			return i
		}
		r.step(line[i])
	}
	return len(line)
}

// shellWriteUnnamedRedirectBare reports whether a redirection word is a bare operator that names its target as the
// next word: its text after the leading digits and & is nothing but < and > (">", "2>", "&>"). A word that carries
// its target (2>/dev/null, >out) is not bare, so the word after it is no target.
func shellWriteUnnamedRedirectBare(word string) bool {
	rest := strings.TrimLeft(word, "0123456789&")
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] != '<' && rest[i] != '>' {
			return false
		}
	}
	return true
}

// shellWriteUnnamedCommandWords drops the here-document operator and the redirections that stand in front of a command,
// so the verb is read from what is left: the operator token (0<<'PY', <<-EOF) and a leading redirection (</dev/null,
// >out, 2>&1) are not the command name.
func shellWriteUnnamedCommandWords(tokens []string) []string {
	for len(tokens) > 0 && (strings.Contains(tokens[0], "<<") || worktreeDelRedirectWord(tokens[0])) {
		word := tokens[0]
		tokens = tokens[1:]
		if strings.Contains(word, "<<") {
			continue // the here-document operator; the body is read by the caller
		}
		// A redirection may name its target as a separate word (> /w/out python3 ...), so the target goes with the
		// operator. Only a bare operator (its text after the digits and & is nothing but < and >) takes the next
		// word as its target; a word that already carries a target (2>/dev/null) does not.
		if shellWriteUnnamedRedirectBare(word) && len(tokens) > 0 && !worktreeDelRedirectWord(tokens[0]) && !strings.Contains(tokens[0], "<<") {
			tokens = tokens[1:]
		}
	}
	tokens = shellVerbSkipWrappers(tokens)
	// A redirection may also stand after the command (python3 <<'PY' 2>/dev/null), so the words that follow the
	// verb are trimmed too: only the verb and its own operands are kept. Only a word shaped like a redirection is
	// trimmed, never a quoted program that merely holds << in its text (bash -c 'python3 <<PY ...').
	for len(tokens) > 1 {
		last := tokens[len(tokens)-1]
		if worktreeDelRedirectWord(last) {
			tokens = tokens[:len(tokens)-1]
			continue
		}
		if len(tokens) > 2 && worktreeDelRedirectWord(tokens[len(tokens)-2]) {
			tokens = tokens[:len(tokens)-2] // the operator and the separate target word it names
			continue
		}
		break
	}
	return tokens
}

// shellWriteUnnamedPythonStdin reports whether a Python interpreter with these arguments reads its program from its
// standard input: no script operand and no -m module (-c is read by shellWriteFStringPythonScript). A lone - operand is
// the standard input itself. An option that takes its value as a separate word (-W, -X) is skipped with it, so the
// value is not mistaken for a script operand.
func shellWriteUnnamedPythonStdin(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return i+1 >= len(args) || args[i+1] == "-"
		case a == "-m" || a == "--module" || strings.HasPrefix(a, "-m") && len(a) > 2:
			return false
		case a == "-W" || a == "-X" || a == "--check-hash-based-pycs":
			i++ // the option's own value is not a script operand
		case a == "-":
			return true
		case a != "" && a[0] == '-':
			continue // an interpreter option, whose own value is not a script operand
		default:
			return false // a script operand: the interpreter runs that, not its standard input
		}
	}
	return true
}

// shellWriteUnnamedSeparatorAt reports whether the byte at i ends the command, read with its neighbours: the & and |
// of a redirection (2>&1, >&2, &>out, <&0) belong to that redirection and cut nothing, while a bare & or | does.
func shellWriteUnnamedSeparatorAt(line string, i int) bool {
	switch c := line[i]; c {
	case ';', '\n':
		return true
	case '&':
		return (i == 0 || line[i-1] != '>' && line[i-1] != '<') && (i+1 >= len(line) || line[i+1] != '>')
	case '|':
		return i == 0 || line[i-1] != '>'
	}
	return false
}

// shellWriteUnnamedPython reports whether a command name is a Python interpreter, as shellWriteFStringPythonScript reads
// it.
func shellWriteUnnamedPython(verb string) bool {
	return verb == "python" || verb == "python3" || verb == "py" || shellVerbVersioned(verb)
}

// shellWriteUnnamedProgram reports whether one Python program holds a write whose destination the reader cannot name and
// can point at the protected area (points). The program text of a string literal passed to exec, eval or compile and the
// replacement fields of an f-string are read by this same walk, because they run as program text in the scope that holds
// them, and each nested program's own imports are read beside the enclosing program's.
func shellWriteUnnamedProgram(program string, points func(string) bool) bool {
	unnamed, _, pointed := shellWriteUnnamedRead(program, points)
	return unnamed && pointed
}

// shellWriteUnnamedRead reads one Python program for this check and answers its three questions: whether it holds a
// write whose destination the reader cannot name (unnamed), whether it holds a write of any kind (wrote, which the
// unquoted here-document rule needs because the outer shell may rewrite a literal destination), and whether it can
// point at the protected area (pointed, from a decoded literal or the name CODEX_HOME).
func shellWriteUnnamedRead(program string, points func(string) bool) (unnamed, wrote, pointed bool) {
	rs := shellVerbWithoutComments(program, true)
	w := &shellWriteUnnamedWalk{}
	w.read(rs, 0, shellWriteCopyImports{})
	if w.codexHome {
		return w.unnamed, w.wrote, true
	}
	for _, literal := range w.literals {
		if literal == "CODEX_HOME" || points != nil && points(literal) {
			return w.unnamed, w.wrote, true
		}
	}
	return w.unnamed, w.wrote, false
}

// shellWriteUnnamedWalk walks one Python program for this check. unnamed is true once it finds a write whose destination
// the reader names nothing for; codexHome is true once the program names CODEX_HOME; literals are the decoded values of
// the string literals it holds, in the order they stand.
type shellWriteUnnamedWalk struct {
	unnamed   bool
	wrote     bool // the walk saw a write at all, named or not, which the here-document rule needs
	codexHome bool
	literals  []string
	assigned  map[string]bool // every name the program binds as an assignment target
}

// shellWriteUnnamedFrame is one bracket the walk opened: the call kind it holds, the argument spans read so far, and
// whether the receiver the call hangs off named a destination of its own, which only a Path(<literal>) call does.
type shellWriteUnnamedFrame struct {
	kind       byte
	name       string // the method name the pending call gave, for the methods whose argument count decides the reading
	start      int
	args       [][2]int
	recvNamed  bool
	recvPath   bool // the receiver is a Path(...) call, so an open() mode is its first argument, not its second
	recvModule bool // the receiver is a module the program imported, so the call is module.open(path, mode)
	defValue   bool // a def header's parameter list is reading a default value, where a name is an expression
}

// shellWriteUnnamedCall is the method call the expression whose bracket just closed names.
type shellWriteUnnamedCall struct {
	at        int
	name      string
	recvNamed bool
}

// read walks one program text with the bindings the enclosing program made (outer, empty at the top level). The program
// read recursively - a replacement field of an f-string, the text of a literal passed to exec, eval or compile - runs
// in the scope that holds it, so it keeps the enclosing program's imports beside its own. A frame is one bracket; the
// kinds are 'o' open(, 'p' Path(, 'd' a def or async def header's parameter list, 'e' exec/eval/compile(, 'g' getattr(,
// 'i' __import__(, 'c' and 'n' a copy or rename call of shutil or os, and the write methods a closing receiver or a
// method call names - 'w' write_text/write_bytes, 't' touch/mkdir, 'q' open(, 'r' rename and 'l' the links. pending is
// the method call the expression whose bracket just closed names.
func (w *shellWriteUnnamedWalk) read(rs []rune, depth int, outer shellWriteCopyImports) {
	if depth > shellWriteExecMaxDepth {
		return
	}
	w.bindLoopTargets(rs)
	binds := shellWriteCopyImportsOf(rs, outer)
	var stack []shellWriteUnnamedFrame
	var pending shellWriteUnnamedCall
	importStmt, firstWord := false, true
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			if shellWriteFStringPrefix(rs, i) {
				end, fields, _ := shellWriteFStringRegion(rs, i, 0)
				// The prefix runes before the quote belong to the literal (rf"...", fr"..."), so the reader
				// decodes a raw f-string as raw: without them it would decode escapes Python preserves and could
				// invent a protected segment the program does not hold.
				w.literal(shellWriteUnnamedLiteralSpan(rs, i, end))
				for _, f := range fields {
					w.read(shellVerbWithoutComments(string(rs[f[0]:f[1]]), true), depth+1, binds)
				}
				i = end - 1
				continue
			}
			end := shellWriteTripleScanRegion(rs, i, true)
			w.literal(shellWriteUnnamedLiteralSpan(rs, i, end))
			i = end - 1
		case c == '\n' || c == '\r' || c == ';':
			if len(stack) == 0 {
				importStmt, firstWord = false, true
			}
		case c == '(' || c == '[' || c == '{':
			kind, recvNamed, recvPath, recvModule, name := byte(0), false, false, false, ""
			switch {
			case pending.name != "" && pending.at == i:
				kind, recvNamed, name = shellWriteUnnamedMethodKind(pending.name), pending.recvNamed, pending.name
				pending = shellWriteUnnamedCall{}
				// recvPath says the receiver is a Path(<literal>) call, so the method's own argument is its mode.
				// A receiver the reader cannot place ((io).open(...), a variable) is not one, so its argument is
				// read in both positions instead.
				recvPath = recvNamed
			case c == '(' && shellWriteUnnamedDefHeader(rs, i):
				kind = 'd' // a def or async def header binds a name; its parenthesis runs nothing
			default:
				kind = shellVerbCallKind(rs, i, c)
				if kind != 0 && shellWriteUnnamedDotted(rs, i) {
					// A method named open (p.open("w")) is not the builtin: its destination is the receiver,
					// which this reader names for no open() method at all. A method named Path (x.Path(...)) is
					// no pathlib.Path and names no destination. A method of any other name is read below.
					kind = shellWriteUnnamedDottedKindWith(rs, i, binds, w.assigned)
					recvModule = kind == 'q' && shellWriteUnnamedDottedModule(rs, i, binds, w.assigned)
				}
				if kind == 0 && c == '(' && shellWriteExecCallee(rs, i, c) {
					kind = 'e'
				}
				if kind == 0 && c == '(' {
					kind = shellWriteCopyModuleKind(rs, i, binds)
				}
				if kind == 0 && c == '(' {
					kind = shellWriteUnnamedFromOpen(rs, i, binds)
				}
				if kind == 0 && c == '(' {
					kind = shellWriteUnnamedSpecial(rs, i)
				}
				if kind == 0 && c == '(' {
					// A write method of an expression that is no Path(<literal>) call: the destination is the
					// receiver, which the reader cannot name.
					kind = shellWriteUnnamedMethodAt(rs, i, binds, w.assigned)
				}
			}
			stack = append(stack, shellWriteUnnamedFrame{kind: kind, name: name, start: i + 1, recvNamed: recvNamed, recvPath: recvPath, recvModule: recvModule})
		case c == ',' && len(stack) > 0:
			if top := &stack[len(stack)-1]; top.kind != 0 {
				top.args = append(top.args, [2]int{top.start, i})
				top.start = i + 1
				top.defValue = false // the next parameter name binds again
			}
		case c == '=' && len(stack) > 0 && stack[len(stack)-1].kind == 'd':
			// A default value in a def header's parameter list (def put(m, f=open)) is an expression, not a
			// binding: the name it holds is read as the value it is.
			stack[len(stack)-1].defValue = true
		case (c == ')' || c == ']' || c == '}') && len(stack) > 0:
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			spans := append(top.args, [2]int{top.start, i})
			w.close(rs, top, spans, depth, binds)
			if c == ')' {
				pending = shellWriteUnnamedReceiver(rs, i, top, spans)
			}
		case shellWriteCopyIdentRune(c):
			j := i
			for j < len(rs) && shellWriteCopyIdentRune(rs[j]) {
				j++
			}
			inDef := false
			if len(stack) > 0 {
				if top := stack[len(stack)-1]; top.kind == 'd' && !top.defValue {
					inDef = true // a parameter name binds a name; a name in a default value does not
				}
			}
			w.identifier(rs, i, j, importStmt, inDef, binds)
			if firstWord {
				firstWord = false
				word := string(rs[i:j])
				importStmt = word == "import" || word == "from"
			}
			i = j - 1
		}
	}
}

// close decides one call from its argument spans and the bracket that closed it.
func (w *shellWriteUnnamedWalk) close(rs []rune, f shellWriteUnnamedFrame, spans [][2]int, depth int, binds shellWriteCopyImports) {
	switch f.kind {
	case 'o':
		if shellWriteUnnamedUnpacked(rs, spans) {
			// A * or ** argument may supply the mode or the path, so neither is a value this reader can read.
			w.wrote = true
			w.unnamed = true
			return
		}
		path, mode := shellVerbOpenArgs(rs, spans)
		if shellWriteEscapeField(path) {
			// An interpolated path (open(f"{root}/n.md", "w")) is a destination this reader cannot read. The mode
			// still decides whether the call writes: an interpolated path read with the default or a read mode is
			// no write at all.
			if shellWriteUnnamedWrites(mode) {
				w.wrote = true
				w.unnamed = true
			}
			return
		}
		if shellWriteUnnamedWrites(mode) {
			w.wrote = true
			if len(shellVerbOpenCall(rs, spans)) == 0 {
				w.unnamed = true
			}
		}
	case 'c', 'n':
		w.wrote = true
		key := "dst"
		if f.kind == 'n' {
			key = "new"
		}
		if len(shellWriteUnnamedDest(rs, spans, 1, key)) == 0 {
			w.unnamed = true
		}
	case 'C', 'N':
		// A copy, rename or link call whose receiver the reader cannot name as a bare module ((shutil).copy(...)):
		// the ordinary destination reader names no destination for this shape, so the call fails closed.
		w.wrote = true
		w.unnamed = true
	case 'O':
		// A call through a name bound to the open builtin through another module (from io import open as o): the
		// ordinary destination reader names an open() destination only for the open spelling, so the call is a write
		// whose destination it cannot name whenever its mode writes. mode= names the mode in either form.
		if shellWriteUnnamedUnpacked(rs, spans) || shellWriteUnnamedWrites(shellWriteUnnamedArg(rs, spans, 1, "mode")) {
			w.wrote = true
			w.unnamed = true
		}
	case 'w', 't':
		// write_text and write_bytes write to their receiver, which this reader names only for a Path(<literal>)
		// call. touch and mkdir write to theirs too, and the reader names no destination for those two at all, so
		// they are a write whose destination it cannot name whatever the receiver is.
		w.wrote = true
		if f.kind == 't' || !f.recvNamed {
			w.unnamed = true
		}
	case 'q':
		// A method named open writes to its receiver, which this reader names for no open() method at all, so the
		// destination is unnamed; the mode decides whether it writes. Path(...).open(mode) gives the mode as its
		// only argument, and module.open(path, mode) as its second; mode= names it in either form. A variable
		// receiver may hold either, so the call is read in the form its arguments fit: a second argument that is no
		// number is the mode of module.open(path, mode), and otherwise a lone mode-like first argument is the mode
		// of Path.open. A number in a mode's place is the buffering count of Path.open, which is no mode at all, so
		// a read-only open with buffering is not refused; an argument the reader can read as neither a mode nor a
		// path still fails closed.
		if shellWriteUnnamedUnpacked(rs, spans) {
			w.wrote = true
			w.unnamed = true // a * or ** argument may supply the mode this reader cannot read
			break
		}
		first, second := shellWriteUnnamedArg(rs, spans, 0, "mode"), shellWriteUnnamedArg(rs, spans, 1, "mode")
		writes := false
		switch {
		case f.recvPath:
			writes = shellWriteUnnamedWrites(first)
		case f.recvModule:
			// A module's own open(path, mode) defaults to reading, so a missing mode is no write, and a number in
			// the mode's place is that module's own numeric argument, which is no mode either.
			writes = !shellVerbBlank(second) && !shellWriteUnnamedNumber(second) && shellWriteUnnamedWrites(second)
		case shellWriteUnnamedModeLike(first):
			// A mode-like first argument is a Path's own open(mode, buffering...): the mode is the first argument,
			// and whatever follows it is a buffering count, never a mode.
			writes = shellWriteUnnamedWrites(first)
		case !shellVerbBlank(second) && !shellWriteUnnamedNumber(second):
			writes = shellWriteUnnamedWrites(second) || !shellWriteUnnamedLiteral(second)
		case !shellVerbBlank(first) && shellWriteUnnamedCallArg(first):
			// A call-shaped lone argument is a path only for a module receiver, which the cases above read. Otherwise
			// it is the mode of a Path's open, and the mode a call passes is its first argument: str("w") and
			// "".join(["w"]) write, str("r") reads, and a first argument the reader cannot read as a literal may hold
			// a write mode, so it writes.
			writes = shellWriteUnnamedCallMode(first)
		case !shellVerbBlank(first) && !shellWriteUnnamedLiteral(first) && !shellWriteUnnamedNumber(first):
			// A lone argument the reader can read as neither a mode nor a path leaves the call's own form unknown,
			// so it fails closed.
			writes = true
		}
		if writes {
			w.wrote = true
			w.unnamed = true
		}
	case 'r':
		if f.name == "replace" && shellWriteUnnamedArgCount(rs, spans) != 1 {
			break // two or more arguments: a string's own replace, which writes no file
		}
		w.wrote = true
		if !f.recvNamed || len(shellWriteUnnamedDest(rs, spans, 0, "target")) == 0 {
			w.unnamed = true
		}
	case 'l':
		w.wrote = true
		if !f.recvNamed {
			w.unnamed = true
		}
	case 'g':
		// getattr(obj, name) reaches a write function dynamically: the reader names the destination only when it can
		// read the name, so an argument that is no literal is a write whose destination it cannot name.
		name, ok := shellVerbLiteral(shellWriteUnnamedArg(rs, spans, 1, "name"))
		if !ok || shellWriteUnnamedFunc(name) {
			w.wrote = true
			w.unnamed = true
		}
	case 'i':
		// __import__(name) reaches a module this reader cannot name when the name is not a literal it knows
		// (__import__("sh" + "util")), and the two modules that carry a write function when it is.
		name, ok := shellVerbLiteral(shellWriteUnnamedArg(rs, spans, 0, "name"))
		if !ok || name == "shutil" || name == "os" {
			w.wrote = true
			w.unnamed = true
		}
	case 'e':
		for _, span := range spans {
			arg := rs[span[0]:span[1]]
			if shellVerbBlank(arg) {
				continue
			}
			if shellWriteEscapeField(arg) {
				// An interpolated exec argument (exec(f"...")) is a program this reader cannot read.
				w.wrote = true
				w.unnamed = true
				break
			}
			program, ok := shellVerbLiteral(arg)
			if !ok {
				break // a program the reader cannot read is CRW-754's case, not this one
			}
			w.read(shellVerbWithoutComments(program, true), depth+1, binds)
			break
		}
	}
}

// bindLoopTargets marks every name a for clause binds (for io in ...): a comprehension reads its element expression
// before its own for clause stands, so the pre-pass binds those names before the walk reaches their use. A name bound
// there is no module any more, so an import's meaning for it no longer holds. String literals are skipped, so a for
// that only stands inside a string binds nothing.
func (w *shellWriteUnnamedWalk) bindLoopTargets(rs []rune) {
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\'' || rs[i] == '"' {
			i = shellWriteTripleScanRegion(rs, i, true) - 1
			continue
		}
		if !shellWriteCopyIdentRune(rs[i]) {
			continue
		}
		j := i
		for j < len(rs) && shellWriteCopyIdentRune(rs[j]) {
			j++
		}
		if string(rs[i:j]) != "for" {
			i = j - 1
			continue
		}
		k := j
		for k < len(rs) && shellVerbSpaceRune(rs[k]) {
			k++
		}
		// The target is a bare name (for io in ...) or a tuple of names ((io,) in ...); a comprehension reads its
		// element before its own for clause stands, so every name the target lists is bound before the walk.
		names, after := shellWriteUnnamedTargetNames(rs, k)
		if len(names) == 0 {
			continue
		}
		m := after
		for m < len(rs) && shellVerbSpaceRune(rs[m]) {
			m++
		}
		if m+2 > len(rs) || string(rs[m:m+2]) != "in" || m+2 < len(rs) && shellWriteCopyIdentRune(rs[m+2]) {
			continue
		}
		if w.assigned == nil {
			w.assigned = map[string]bool{}
		}
		for _, name := range names {
			w.assigned[name] = true
		}
		i = j - 1
	}
}

// shellWriteUnnamedTargetNames is every name a for clause's target binds, read from k: a bare name, or the names a
// tuple lists, with a star, nested parentheses or brackets and a trailing comma. Targets are comma-separated elements
// (for x, io in ...), so the list goes on past the first name. It returns the names and the offset just after the last
// element, before the in keyword.
func shellWriteUnnamedTargetNames(rs []rune, k int) ([]string, int) {
	names := []string{}
	for {
		for k < len(rs) && (rs[k] == '*' || shellVerbSpaceRune(rs[k])) {
			k++
		}
		if k < len(rs) && (rs[k] == '(' || rs[k] == '[') {
			inner, after := shellWriteUnnamedBracketNames(rs, k)
			names, k = append(names, inner...), after
		} else {
			start := k
			for k < len(rs) && shellWriteCopyIdentRune(rs[k]) {
				k++
			}
			if k == start || string(rs[start:k]) == "in" {
				return names, start
			}
			names = append(names, string(rs[start:k]))
		}
		m := k
		for m < len(rs) && shellVerbSpaceRune(rs[m]) {
			m++
		}
		if m >= len(rs) || rs[m] != ',' {
			return names, k
		}
		k = m + 1
	}
}

// shellWriteUnnamedBracketNames is the names a parenthesised or bracketed target lists, from the bracket at k, and the
// offset just after its closing bracket. A bracket left open returns the offset it stopped at.
func shellWriteUnnamedBracketNames(rs []rune, k int) ([]string, int) {
	names, after := shellWriteUnnamedTargetNames(rs, k+1)
	for after < len(rs) && shellVerbSpaceRune(rs[after]) {
		after++
	}
	if after < len(rs) && (rs[after] == ')' || rs[after] == ']') {
		return names, after + 1
	}
	return names, after
}

// literal records the decoded value of one string literal the program holds.
func (w *shellWriteUnnamedWalk) literal(arg []rune) {
	if value, ok := shellVerbLiteral(arg); ok {
		w.literals = append(w.literals, value)
	}
}

// shellWriteUnnamedLiteralSpan is the whole string literal whose opening quote stands at rs[i] and whose body ends at
// end: the quote and its body, with the prefix runes that stand before it (r, b, u, f) when they are the literal's own.
// The walk reaches the quote after its identifier branch consumed those prefix runes, so a raw literal is read as a
// raw literal: without the prefix a body such as \N{foo} would decode as an unsupported escape and be dropped, hiding
// a protected-area literal the program really holds.
func shellWriteUnnamedLiteralSpan(rs []rune, i, end int) []rune {
	start := i
	for start > 0 && strings.ContainsRune("rRuUbBfF", rs[start-1]) {
		start--
	}
	// The whole prefix run belongs to the literal only when no identifier rune stands before it; otherwise the run
	// is the tail of a name (myrb"..." is the name myrb followed by a literal, not an rb literal).
	if start > 0 && (rs[start-1] >= 128 || rs[start-1] == '_' || shellVerbLetter(byte(rs[start-1]), true)) {
		return rs[i:end]
	}
	return rs[start:end]
}

// identifier reads one identifier: the program naming CODEX_HOME can point at the protected area, and a write function
// the program does not call - the open builtin, a copy, rename or link function of shutil or os, a from-imported bare
// name - is a write whose destination the reader cannot name. A name inside an import statement binds a name and calls
// nothing; a name a def header's parameter list binds is no write value either; and a name an assignment binds is the
// target, not the builtin (open = print binds open, it does not read it).
func (w *shellWriteUnnamedWalk) identifier(rs []rune, i, j int, importStmt, inDefParam bool, binds shellWriteCopyImports) {
	word := string(rs[i:j])
	if word == "CODEX_HOME" {
		w.codexHome = true
	}
	if importStmt {
		return
	}
	if inDefParam {
		// A def header's parameter binds the name in the function's scope, so an import's meaning for it no longer
		// holds there (def f(io): io.write_text(...) is a Path's method, not the io module's).
		if w.assigned == nil {
			w.assigned = map[string]bool{}
		}
		w.assigned[word] = true
		return
	}
	if shellWriteUnnamedTarget(rs, j) || shellWriteUnnamedBinds(rs, i, j) {
		if w.assigned == nil {
			w.assigned = map[string]bool{}
		}
		w.assigned[word] = true // the name is bound here, so an import's meaning for it no longer holds
		return                  // an assignment or keyword-argument target binds the name; it is no write value
	}
	if word == "__import__" && !shellWriteUnnamedCalled(rs, j) {
		// The __import__ builtin taken as a value (imp = __import__) reaches a module this reader cannot name.
		w.unnamed = true
		return
	}
	if mod, ok := shellWriteUnnamedAttribute(rs, i); ok {
		if word == "import_module" && (mod == "importlib" || slices.Contains(binds.alias[mod], "importlib")) {
			w.unnamed = true
			return
		}
		if shellWriteCopyKind(mod, word, binds) != 0 && !shellWriteUnnamedCalled(rs, j) {
			w.unnamed = true
			return
		}
		if !shellWriteUnnamedCalled(rs, j) && (shellWriteCopyFunc("shutil", word) || shellWriteCopyFunc("os", word)) {
			// A copy, rename or link function taken off a receiver the reader cannot name as a bare module
			// (f = (shutil).copy, f = s.copy where s holds shutil) is a write function value whose destination it
			// cannot name. A receiver the reader does place is left to the branch above.
			w.unnamed = true
			return
		}
		if word == "open" && !shellWriteUnnamedCalled(rs, j) {
			// A name named open taken off a receiver as a value - f = builtins.open, f = io.open - is the open
			// builtin through another name, and the reader names no destination for such a value. A call
			// (io.open(path, mode)) is left to its own frame.
			w.unnamed = true
			return
		}
		if shellWriteUnnamedModuleReceiver(mod, binds, w.assigned) {
			return // a module this reader reads its own way is left to the readers above
		}
	}
	if word == "import_module" {
		w.unnamed = true
		return
	}
	if binds.importer[word] {
		// A name bound to importlib.import_module reaches a module this reader cannot name, so a call through it
		// (load("shutil").copy(...)) is a write whose destination it cannot name.
		w.unnamed = true
		return
	}
	if shellWriteUnnamedCalled(rs, j) {
		return // a call: its own frame reads it
	}
	if shellWriteUnnamedDotted(rs, i) && shellWriteUnnamedValueMethod(word) {
		// A write method taken off its receiver as a value - f = p.write_text, f = Path(...).write_text - writes to
		// that receiver through whatever name it is bound to, and this reader names no destination for such a value.
		w.unnamed = true
		return
	}
	if word != "open" && len(binds.from[word]) == 0 && !binds.opener[word] {
		return
	}
	w.unnamed = true
}

// shellWriteUnnamedValueMethod reports whether a method name is one of the write methods this check reads whose value
// names no destination: the Path write methods, rename, replace and the two links. A method named replace taken as a
// value names no destination either; the argument-count rule that separates a Path.replace call from a string's own
// replace cannot apply to a value, and a Path write that reaches the protected area is the reading to fail on.
func shellWriteUnnamedValueMethod(name string) bool {
	switch name {
	case "write_text", "write_bytes", "touch", "mkdir", "open", "rename", "replace", "symlink_to", "hardlink_to":
		return true
	}
	return false
}

// shellWriteUnnamedDefHeader reports whether the name that ends just before the bracket at rs[i] is the name a def or
// async def header binds, so the bracket opens a parameter list and runs nothing (shellWriteExecDefHeader).
func shellWriteUnnamedDefHeader(rs []rune, i int) bool {
	k := i
	for k > 0 && shellVerbSpaceRune(rs[k-1]) {
		k--
	}
	end := k
	for k > 0 && shellWriteCopyIdentRune(rs[k-1]) {
		k--
	}
	return k != end && shellWriteExecDefHeader(rs, k-1)
}

// shellWriteUnnamedDotted reports whether the identifier just before the bracket at rs[i] hangs off a dot, so the call
// is a method on a receiver rather than a plain call of the name.
func shellWriteUnnamedDotted(rs []rune, i int) bool {
	j := i
	for j > 0 && shellVerbSpaceRune(rs[j-1]) {
		j--
	}
	for j > 0 && shellWriteCopyIdentRune(rs[j-1]) {
		j--
	}
	for j > 0 && shellVerbSpaceRune(rs[j-1]) {
		j--
	}
	return j > 0 && rs[j-1] == '.'
}

// shellWriteUnnamedDottedKindWith is shellWriteUnnamedDottedKind with the program's import bindings: a method named
// Path on a module the program imported (pathlib.Path(...)) is pathlib.Path and names a destination, while a method
// named Path on anything else is no pathlib.Path and names none.
func shellWriteUnnamedDottedKindWith(rs []rune, i int, binds shellWriteCopyImports, assigned map[string]bool) byte {
	j := i
	for j > 0 && shellVerbSpaceRune(rs[j-1]) {
		j--
	}
	end := j
	for j > 0 && shellWriteCopyIdentRune(rs[j-1]) {
		j--
	}
	name := string(rs[j:end])
	if name == "open" {
		return 'q'
	}
	if name == "Path" && shellWriteUnnamedDottedModule(rs, i, binds, assigned) {
		return 'p'
	}
	return 0
}

// shellWriteUnnamedDottedModule reports whether a dotted call's receiver names a module the program imported, so the
// call is module.open(path, mode) rather than Path.open(mode): the reader names the name before the dot and asks the
// program's own import bindings (a name bound by an import statement holds a module, not a Path).
func shellWriteUnnamedDottedModule(rs []rune, i int, binds shellWriteCopyImports, assigned map[string]bool) bool {
	j := i - 1
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	for j >= 0 && shellWriteCopyIdentRune(rs[j]) {
		j-- // the method name itself (open)
	}
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	if j < 0 || rs[j] != '.' {
		return false
	}
	j--
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	end := j + 1
	for j >= 0 && shellWriteCopyIdentRune(rs[j]) {
		j--
	}
	if end == j+1 {
		return false
	}
	return shellWriteUnnamedModuleReceiver(string(rs[j+1:end]), binds, assigned)
}

// shellWriteUnnamedBinds reports whether the name that ends at j is bound by a form the assignment test does not read:
// an annotated assignment (io: Path = ...), a loop or comprehension target (for io in ...), or a lambda parameter
// (lambda io: ...). Each binds the name in its scope, so an import's meaning for it no longer holds there.
func shellWriteUnnamedBinds(rs []rune, i, j int) bool {
	if shellWriteUnnamedLambdaParam(rs, i) {
		return true // a lambda's parameter binds the name in the lambda's body
	}
	if shellWriteUnnamedAsTarget(rs, i) {
		return true // a with or except clause's as target binds the name (with open(p) as io)
	}
	k := j
	for k < len(rs) && shellVerbSpaceRune(rs[k]) {
		k++
	}
	if k >= len(rs) || rs[k] != ':' {
		return false
	}
	// An annotated assignment (io: Path = ...) binds the name, and only when an = follows the annotation in the same
	// statement: a dict key, a slice or a lambda header carries a colon but binds nothing here. A loop or
	// comprehension target is read by the pre-pass, and a lambda's own parameter by the = after it, so neither is
	// decided here.
	for m := k + 1; m < len(rs) && rs[m] != '\n' && rs[m] != '\r' && rs[m] != ';'; m++ {
		if rs[m] == '=' && (m+1 >= len(rs) || rs[m+1] != '=') {
			return true
		}
		if rs[m] == ':' {
			return false // a second colon: a slice or a nested annotation, not this name's assignment
		}
	}
	return false
}

// shellWriteUnnamedLambdaParam reports whether the name that starts at rs[i] is a lambda's parameter: it stands after
// the lambda keyword of its own clause, before that clause's colon, and no other colon or bracket of the clause's own
// level stands between them. A lambda parameter binds the name in the lambda's body, so an import's meaning for it no
// longer holds there (lambda io: io.write_text(...) is a Path's method, not the io module's).
func shellWriteUnnamedLambdaParam(rs []rune, i int) bool {
	depth := 0
	for b := i - 1; b >= 0 && rs[b] != '\n' && rs[b] != '\r' && rs[b] != ';'; b-- {
		switch c := rs[b]; {
		case c == ')' || c == ']' || c == '}':
			depth++
		case c == '(' || c == '[' || c == '{':
			if depth > 0 {
				depth--
			}
		case depth == 0 && c == '=':
			// A default value follows the parameter's =, so this name is the value, not a parameter
			// (lambda g=open: ...): the name before the = is the parameter.
			return false
		case depth == 0 && c == ':':
			return false // the clause's colon: this name stands after it, in the lambda's body
		case depth == 0 && shellWriteCopyIdentRune(c):
			e := b
			for b >= 0 && shellWriteCopyIdentRune(rs[b]) {
				b--
			}
			if string(rs[b+1:e+1]) == "lambda" {
				return true
			}
			b++
		}
	}
	return false
}

// shellWriteUnnamedTarget reports whether an assignment binds the name that ends at j: the next rune that is not a blank
// is a single = (an == comparison reads the name instead).
func shellWriteUnnamedTarget(rs []rune, j int) bool {
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j < len(rs) && rs[j] == '=' && (j+1 >= len(rs) || rs[j+1] != '=') {
		return true
	}
	// A tuple target binds every name it lists (io, = (Path(...),)), so a comma that leads to an assignment
	// anywhere before the end of the statement counts too.
	for k := j; k < len(rs) && rs[k] != '\n' && rs[k] != '\r' && rs[k] != ';'; k++ {
		if rs[k] == '=' && (k+1 >= len(rs) || rs[k+1] != '=') {
			return true
		}
		if shellWriteCopyIdentRune(rs[k]) || strings.ContainsRune("()[]*, \t", rs[k]) {
			continue // the other names of the tuple, and its brackets, commas and star
		}
		break // something other than a tuple element: the name is no target
	}
	return false
}

// shellWriteUnnamedAsTarget reports whether the name that starts at rs[i] stands after the as keyword of a with or except
// clause, which binds it.
func shellWriteUnnamedAsTarget(rs []rune, i int) bool {
	k := i
	for k > 0 && shellVerbSpaceRune(rs[k-1]) {
		k--
	}
	return k >= 2 && string(rs[k-2:k]) == "as" && (k == 2 || !shellWriteCopyIdentRune(rs[k-3]))
}

// shellWriteUnnamedUnpacked reports whether any argument of a call is a * or ** unpacking, which may supply the mode or
// the path this reader cannot read.
func shellWriteUnnamedUnpacked(rs []rune, spans [][2]int) bool {
	for _, span := range spans {
		for _, r := range rs[span[0]:span[1]] {
			if shellVerbSpaceRune(r) {
				continue
			}
			if r == '*' {
				return true
			}
			break
		}
	}
	return false
}

// shellWriteUnnamedAttribute is the name the attribute whose identifier starts at rs[i] hangs off, and whether a
// receiver expression stands before the dot at all. The name is the nearest preceding name: for p.parent.write_text it
// is parent, for a.shutil.copy it is shutil. The name is empty when the receiver is not a bare name (a call, a
// subscript or a bracketed expression), and the second result is false when no dot stands before the identifier.
func shellWriteUnnamedAttribute(rs []rune, i int) (string, bool) {
	j := i - 1
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	if j < 0 || rs[j] != '.' {
		return "", false
	}
	j--
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	if j >= 0 && (rs[j] == ')' || rs[j] == ']' || rs[j] == '}') {
		return "", true // a call, a subscript or a bracketed expression: a receiver, but not a bare name
	}
	end := j + 1
	for j >= 0 && shellWriteCopyIdentRune(rs[j]) {
		j--
	}
	if end == j+1 {
		return "", false
	}
	if j >= 0 && rs[j] == '.' {
		// The name hangs off a longer attribute chain (p.parent.write_text, a.shutil.copy): it is an attribute of
		// something else, not a bare module name, so a module this reader reads its own way does not claim it.
		return "", true
	}
	return string(rs[j+1 : end]), true
}

// shellWriteUnnamedCalled reports whether a call follows the name that ends at j: the next rune that is not a blank is an
// opening bracket.
func shellWriteUnnamedCalled(rs []rune, j int) bool {
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	return j < len(rs) && rs[j] == '('
}

// shellWriteUnnamedReceiver is the method call the expression whose bracket closes at close names, when the method is one
// of the Path methods this check reads. recvNamed says whether the reader names the receiver itself, which it does only
// for a Path(...) call whose arguments the reader reads as a path.
func shellWriteUnnamedReceiver(rs []rune, close int, recv shellWriteUnnamedFrame, spans [][2]int) shellWriteUnnamedCall {
	j := close + 1
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j >= len(rs) || rs[j] != '.' {
		return shellWriteUnnamedCall{}
	}
	for j++; j < len(rs) && shellVerbSpaceRune(rs[j]); {
		j++
	}
	start := j
	for j < len(rs) && shellWriteCopyIdentRune(rs[j]) {
		j++
	}
	if j == start {
		return shellWriteUnnamedCall{}
	}
	name := string(rs[start:j])
	if !shellWriteUnnamedMethod(name) {
		return shellWriteUnnamedCall{}
	}
	for j < len(rs) && shellVerbSpaceRune(rs[j]) {
		j++
	}
	if j >= len(rs) || rs[j] != '(' {
		return shellWriteUnnamedCall{}
	}
	return shellWriteUnnamedCall{at: j, name: name, recvNamed: recv.kind == 'p' && shellWriteUnnamedPathNamed(rs, spans)}
}

// shellWriteUnnamedPathNamed reports whether the reader names the whole destination of a Path(...) call: every argument
// it gives is a string literal, and none of them holds a replacement field. One argument that is not a literal leaves
// the destination unknown, however many literal parts stand beside it, so the call names its directory and no path.
func shellWriteUnnamedPathNamed(rs []rune, spans [][2]int) bool {
	parts := 0
	for _, span := range spans {
		arg := rs[span[0]:span[1]]
		if shellVerbBlank(arg) {
			continue
		}
		if _, ok := shellVerbLiteral(arg); !ok || shellWriteEscapeField(arg) {
			return false
		}
		parts++
	}
	return parts > 0
}

// shellWriteUnnamedMethodAt reads the call whose bracket is at i when the method that stands before it is one of the
// write methods this check reads and its receiver is not a Path(...) call (read by shellWriteUnnamedReceiver): the
// destination of such a call is its receiver or its argument, which the reader cannot name. A name a module the reader
// reads its own way owns (shutil.copy, os.rename, importlib.import_module) is left to shellWriteCopyModuleKind and
// shellWriteUnnamedSpecial. A method named replace counts only with one argument, because str.replace needs two and
// Path.replace takes one (CRW-900's rename), so an ordinary string's own replace is never refused.
func shellWriteUnnamedMethodAt(rs []rune, i int, binds shellWriteCopyImports, assigned map[string]bool) byte {
	start := i - 1
	for start >= 0 && shellVerbSpaceRune(rs[start]) {
		start--
	}
	end := start + 1
	for start >= 0 && shellWriteCopyIdentRune(rs[start]) {
		start--
	}
	if end == start+1 {
		return 0
	}
	name := string(rs[start+1 : end])
	receiver, ok := shellWriteUnnamedAttribute(rs, start+1)
	if !ok {
		return 0 // no dot before the name: a plain call, not a method
	}
	if shellWriteUnnamedModuleReceiver(receiver, binds, assigned) {
		return 0
	}
	if name == "replace" {
		if shellWriteUnnamedArgs(rs, i) != 1 {
			return 0 // two or more arguments: a str's own replace, which writes no file
		}
	}
	// A method the reader reads as one of the Path write methods (rename, replace, symlink_to, ...) is read by its own
	// rule: its destination is the receiver or its one argument.
	if shellWriteUnnamedMethod(name) {
		return shellWriteUnnamedMethodKind(name)
	}
	// A copy, rename or link method on a receiver this reader does not know to be something else - a variable
	// (s = shutil; s.copy(...)), a bracketed or parenthesized expression ((shutil).copy(...)) - is a call the
	// ordinary destination reader never names a destination for, so the capital kind marks it unnamed whatever its
	// arguments say. Every one of these functions takes two arguments, so a call with fewer is a same-named method of
	// something else (a dict's or list's own copy() takes none) and is no filesystem write; a * or ** argument may
	// supply both.
	if shellWriteCopyFunc("shutil", name) || shellWriteCopyFunc("os", name) {
		if shellWriteUnnamedArgs(rs, i) < 2 && !shellWriteUnnamedUnpackedCall(rs, i) {
			return 0 // too few arguments: a same-named method of a dict or a list, which writes no file
		}
		if name == "renames" {
			return 'N'
		}
		return 'C'
	}
	return 0
}

// shellWriteUnnamedUnpackedCall reports whether the call whose opening bracket is at rs[i] passes a * or ** argument,
// which may supply more arguments than the reader can count (s.copy(*args) supplies both of copy's).
func shellWriteUnnamedUnpackedCall(rs []rune, i int) bool {
	depth, start := 0, i+1
	for j := i; j < len(rs); j++ {
		switch c := rs[j]; {
		case c == '\'' || c == '"':
			j = shellWriteTripleScanRegion(rs, j, true) - 1
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth == 0 {
				return shellWriteUnnamedStarArg(rs[start:j])
			}
		case c == ',' && depth == 1:
			if shellWriteUnnamedStarArg(rs[start:j]) {
				return true
			}
			start = j + 1
		}
	}
	return false
}

// shellWriteUnnamedStarArg reports whether one argument span opens with a * or ** unpacking.
func shellWriteUnnamedStarArg(arg []rune) bool {
	for _, r := range arg {
		if shellVerbSpaceRune(r) {
			continue
		}
		return r == '*'
	}
	return false
}

// shellWriteUnnamedArgs counts the arguments of the call whose opening bracket is at rs[i]: the top-level commas that
// separate them, plus one when the call is not empty. A comma inside a string literal or a nested bracket separates
// nothing, and a trailing comma before the closing bracket adds no argument (Python binds one argument for
// p.replace("x",)).
func shellWriteUnnamedArgs(rs []rune, i int) int {
	depth, args, lastComma := 0, 0, -1
	for j := i; j < len(rs); j++ {
		switch c := rs[j]; {
		case c == '\'' || c == '"':
			j = shellWriteTripleScanRegion(rs, j, true) - 1
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth--; depth == 0 {
				if lastComma >= 0 && shellVerbBlank(rs[lastComma+1:j]) {
					return args // a trailing comma binds no argument of its own
				}
				if args == 0 && shellVerbBlank(rs[i+1:j]) {
					return 0
				}
				return args + 1
			}
		case c == ',':
			if depth == 1 {
				args++
				lastComma = j
			}
		}
	}
	return -1
}

// shellWriteUnnamedArgCount counts the arguments the walk read for a call whose bracket has closed: the argument spans
// that hold anything at all.
func shellWriteUnnamedArgCount(rs []rune, spans [][2]int) int {
	count := 0
	for _, span := range spans {
		if !shellVerbBlank(rs[span[0]:span[1]]) {
			count++
		}
	}
	return count
}

// shellWriteUnnamedModuleReceiver reports whether the name a method hangs off is a module: shutil, os or importlib, an
// alias an import bound to one of them, or any other name the program bound with an import statement. The last case
// matters for a dotted open: a name the program imported holds a module, so X.open(path, mode) is the module form,
// whose mode is its second argument, not a Path's own open(mode). A name the program later assigned is no module any
// more (io = Path(...) rebinds it), so an assigned name is read as the Path or variable it now holds.
func shellWriteUnnamedModuleReceiver(receiver string, binds shellWriteCopyImports, assigned map[string]bool) bool {
	if assigned[receiver] {
		return false
	}
	switch receiver {
	case "shutil", "os", "importlib":
		return true
	}
	for _, module := range binds.alias[receiver] {
		switch module {
		case "shutil", "os", "importlib":
			return true
		}
	}
	return len(binds.alias[receiver]) > 0
}

// shellWriteUnnamedMethod reports whether a method name is one of the Path methods this check reads: the write methods
// whose destination is the receiver, and rename, replace and the two links, whose destination is their argument or the
// receiver (CRW-900).
func shellWriteUnnamedMethod(name string) bool {
	switch name {
	case "write_text", "write_bytes", "touch", "mkdir", "open", "rename", "replace", "symlink_to", "hardlink_to":
		return true
	}
	return false
}

// shellWriteUnnamedMethodKind is the frame kind of such a method.
func shellWriteUnnamedMethodKind(name string) byte {
	switch name {
	case "write_text", "write_bytes":
		return 'w'
	case "touch", "mkdir":
		return 't'
	case "open":
		return 'q'
	case "rename", "replace":
		return 'r'
	}
	return 'l'
}

// shellWriteUnnamedFromOpen is the frame kind of a call to a name the program bound to the open builtin through another
// module (from io import open as o). The ordinary destination reader names an open() destination only for the open
// spelling, so a call through the alias is a write whose destination this reader cannot name whenever its mode writes.
func shellWriteUnnamedFromOpen(rs []rune, i int, binds shellWriteCopyImports) byte {
	word, ok := shellWriteUnnamedCallee(rs, i)
	if !ok || !binds.opener[word] {
		return 0
	}
	return 'O'
}

// shellWriteUnnamedSpecial reads the callee of the call whose bracket is at i when it is one of the dynamic routes this
// check names: getattr ('g') and __import__ ('i'). Any other callee is no such call.
func shellWriteUnnamedSpecial(rs []rune, i int) byte {
	word, ok := shellWriteUnnamedCallee(rs, i)
	if !ok {
		return 0
	}
	switch word {
	case "getattr":
		return 'g'
	case "__import__":
		return 'i'
	}
	return 0
}

// shellWriteUnnamedCallee is the bare identifier that stands just before the bracket at i: an attribute chain (a.getattr)
// is not the builtin.
func shellWriteUnnamedCallee(rs []rune, i int) (string, bool) {
	j := i - 1
	for j >= 0 && shellVerbSpaceRune(rs[j]) {
		j--
	}
	end := j + 1
	for j >= 0 && shellWriteCopyIdentRune(rs[j]) {
		j--
	}
	if end == j+1 || j >= 0 && rs[j] == '.' {
		return "", false
	}
	return string(rs[j+1 : end]), true
}

// shellWriteUnnamedArg is the argument a call gives for key, or its pos-th positional argument.
func shellWriteUnnamedArg(rs []rune, spans [][2]int, pos int, key string) []rune {
	positional, arg := 0, []rune(nil)
	for _, span := range spans {
		value := rs[span[0]:span[1]]
		if shellVerbBlank(value) {
			continue
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
	return arg
}

// shellWriteUnnamedWrites reports whether an open() call's mode opens for writing: a literal mode holding w, a, x or +,
// or a mode the reader cannot read as a literal (a name, a \N{...} escape). An absent mode reads, which is what Python's
// default mode does.
func shellWriteUnnamedWrites(mode []rune) bool {
	if shellVerbBlank(mode) {
		return false
	}
	if shellWriteEscapeField(mode) {
		return true // an f-string mode holds a replacement field, so its value is not the text this reader sees
	}
	literals := 0
	for _, earlier := range []bool{true, false} {
		kind, ok := shellWriteEscapeLiteral(mode, earlier)
		if !ok {
			continue
		}
		literals++
		if strings.ContainsAny(kind, "wax+") {
			return true
		}
	}
	return literals == 0
}

// shellWriteUnnamedDest is the destination a copy, rename or link call names, or nil when the argument holds an
// f-string replacement field: an interpolated destination is a value this reader cannot read, so the call names no
// destination of its own.
func shellWriteUnnamedDest(rs []rune, spans [][2]int, pos int, key string) []string {
	if shellWriteEscapeField(shellWriteUnnamedArg(rs, spans, pos, key)) {
		return nil
	}
	return shellWriteCopyDest(rs, spans, pos, key)
}

// shellWriteUnnamedModeLike reports whether an argument is shaped like an open() mode rather than a path: a decoded
// string literal whose characters are all letters Python accepts in a mode (r, w, a, x, b, t, u) or +. It separates
// Path(...).open("w") from module.open("memories/a.tar") when the receiver alone does not say which form the call is.
func shellWriteUnnamedModeLike(arg []rune) bool {
	value, ok := shellVerbLiteral(arg)
	if !ok || value == "" {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("rwxabtu+", r|0x20) {
			return false
		}
	}
	return true
}

// shellWriteUnnamedLiteral reports whether an argument is a string literal this reader can read.
func shellWriteUnnamedLiteral(arg []rune) bool {
	_, ok := shellVerbLiteral(arg)
	return ok
}

// shellWriteUnnamedCallArg reports whether an argument is a call: a name or an attribute chain that ends in a call
// bracket (os.path.join(root, "n.md"), Path(...)). Such an argument is a value the program computes, which is what a
// module's own open() takes as its path, so it is not a mode this reader cannot read.
func shellWriteUnnamedCallArg(arg []rune) bool {
	for i := len(arg) - 1; i >= 0; i-- {
		switch c := arg[i]; {
		case c == ')' || c == ']':
			return true
		case shellVerbSpaceRune(c):
			continue
		default:
			return false
		}
	}
	return false
}

// shellWriteUnnamedCallMode reports whether a call-shaped argument of an open-like method opens for writing. A call that
// makes a mode is read by the mode it passes: str(...), and a join on a string literal, write when the first argument
// they pass is a write mode (str("w") writes, str("r") reads, and an argument the reader cannot read as a literal may
// hold a write mode). A call that makes a path (os.path.join, Path(...), joinpath and the like) is the path a module's
// open reads by default, so it does not write. Any other call computes a value this reader cannot read, so it writes.
func shellWriteUnnamedCallMode(arg []rune) bool {
	end := len(arg)
	for end > 0 && shellVerbSpaceRune(arg[end-1]) {
		end--
	}
	lp := 0
	for lp < end && arg[lp] != '(' {
		lp++
	}
	if end == 0 || lp >= end || arg[end-1] != ')' {
		return true
	}
	callee := strings.TrimSpace(string(arg[:lp]))
	name := callee[strings.LastIndexByte(callee, '.')+1:]
	if callee == "str" || strings.HasPrefix(callee, "'") || strings.HasPrefix(callee, "\"") {
		return shellWriteUnnamedCallInner(arg[lp+1 : end-1])
	}
	switch name {
	case "join", "joinpath", "abspath", "normpath", "realpath", "resolve", "absolute", "expanduser", "fspath", "Path", "PurePath":
		return false
	}
	return true
}

// shellWriteUnnamedCallInner reports whether the first argument of a call inside a call-shaped open argument is a write
// mode: the text up to the first top-level comma, or the whole inside when there is none.
func shellWriteUnnamedCallInner(inner []rune) bool {
	depth, quote := 0, rune(0)
	for i, c := range inner {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			return shellWriteUnnamedWrites(inner[:i])
		}
	}
	return shellWriteUnnamedWrites(inner)
}

// shellWriteUnnamedNumber reports whether an argument is a number literal: an optional sign, digits and one dot. It
// separates Path.open's buffering count (p.open("r", -1)) from module.open's mode, which is never a bare number.
func shellWriteUnnamedNumber(arg []rune) bool {
	i, digits := 0, 0
	for i < len(arg) && shellVerbSpaceRune(arg[i]) {
		i++
	}
	if i < len(arg) && (arg[i] == '-' || arg[i] == '+') {
		i++
	}
	for ; i < len(arg) && !shellVerbSpaceRune(arg[i]); i++ {
		if arg[i] == '.' {
			continue
		}
		if arg[i] < '0' || arg[i] > '9' {
			return false
		}
		digits++
	}
	return digits > 0
}

// shellWriteUnnamedFunc reports whether a name is a write function of this reader: open, a Path write method, or a copy,
// rename or link function of shutil or os.
func shellWriteUnnamedFunc(name string) bool {
	switch name {
	case "open", "write_text", "write_bytes", "touch", "mkdir", "rename", "replace", "symlink_to", "hardlink_to":
		return true
	}
	return shellWriteCopyFunc("shutil", name) || shellWriteCopyFunc("os", name)
}
