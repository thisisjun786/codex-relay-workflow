package hook

import "strings"

// A Node or Python program that holds a file API may write through a name the destination reader cannot attribute: a
// destructured or aliased write function, a bracket or computed name, getattr, or a rebinding. Such a write has an unknown
// destination (the memory gate asks for a grant). A write called as a dot call on a file API (fs.writeFileSync(p, x),
// shutil.copy(a, b)) keeps its literal destination, which the verb readers read.

// shellIRFileAPIs names the file APIs of Node and Python programs: their writes are the program's own file effects.
func shellIRFileAPIs(tok string) bool {
	switch tok {
	case "fs", "promises", "pathlib", "shutil", "os", "io", "open", "Path":
		return true
	}
	return false
}

// shellIRWriteNames names the file-writing calls of the two languages (the verb readers read their destinations).
func shellIRWriteNames(tok string) bool {
	switch tok {
	case "writeFile", "writeFileSync", "appendFile", "appendFileSync", "createWriteStream", "write_text", "write_bytes",
		"copy", "copy2", "copyfile", "copyFile", "copytree", "move", "rename", "renames", "renameSync", "replace", "symlink", "link":
		return true
	}
	return false
}

// shellIRPyPathCreateName names the pathlib methods that create a file or a directory at their receiver (CRW-951): Path.touch and
// Path.mkdir. They are read like write_text: a Path(...) call receiver names its destination, any other receiver is unknown.
func shellIRPyPathCreateName(tok string) bool { return tok == "touch" || tok == "mkdir" }

// shellIRPyRunsText names the calls that run a string as a program or a command (exec, eval, compile, a subprocess or an os.system
// call, runpy, timeit, an interactive console): a program that names one may run any string literal it holds.
func shellIRPyRunsText(tok string) bool {
	switch tok {
	case "exec", "eval", "compile", "system", "subprocess", "run", "call", "check_call", "check_output", "getoutput",
		"getstatusoutput", "runpy", "run_path", "run_module", "run_code", "interact", "timeit", "startfile", "Popen":
		return true
	}
	return strings.HasPrefix(tok, "exec") || strings.HasPrefix(tok, "spawn") || strings.HasPrefix(tok, "popen") || strings.HasPrefix(tok, "posix_spawn")
}

// shellIRPyDataMask marks (by byte offset of src) the text of a Python program that is only data: a # comment and the body of a
// string literal without an f in its prefix. An f-string stays code, since its replacement fields are program text, and so
// does every string of a program that names a call which runs text (shellIRPyRunsText): the mask is nil then, and every offset
// is read as code. It is the same string and comment reading as shellVerbWithoutComments and shellWriteTripleScanRegion.
func shellIRPyDataMask(src string, spans [][2]int) []bool {
	for _, sp := range spans {
		if shellIRPyRunsText(src[sp[0]:sp[1]]) {
			return nil
		}
	}
	rs := []rune(src)
	offs := make([]int, len(rs)+1)
	for i, n := 0, 0; i <= len(rs); i++ {
		offs[i] = n
		if i < len(rs) {
			n += len(string(rs[i]))
		}
	}
	mask := make([]bool, len(src)+1)
	mark := func(from, to int) {
		for k := offs[from]; k < offs[to]; k++ {
			mask[k] = true
		}
	}
	for i := 0; i < len(rs); {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			end := shellWriteTripleScanRegion(rs, i, true)
			if !shellWriteFStringPrefix(rs, i) {
				mark(i, end)
			}
			i = end
		case c == '#':
			end := i + 1
			for end < len(rs) && rs[end] != '\n' && rs[end] != '\r' {
				end++
			}
			mark(i, end)
			i = end
		default:
			i++
		}
	}
	return mask
}

// shellIRStructuralWriteUnknown reports a program that holds a file API and a write the reader cannot attribute to a literal
// destination: a bare or unattached write name, a run-time name (getattr, __import__, eval, ...), or a computed subscript.
func shellIRStructuralWriteUnknown(src string, python bool) bool {
	spans := shellIRTokenSpans(src)
	if !python {
		// A Node program that evaluates text (eval, new Function, vm) runs code the program does not show, with the file APIs
		// among it: the destination is unknown whether or not the program names a file API itself.
		for _, sp := range spans {
			switch src[sp[0]:sp[1]] {
			case "eval", "Function", "vm":
				return true
			}
		}
	}
	api := false
	for _, sp := range spans {
		if shellIRFileAPIs(src[sp[0]:sp[1]]) {
			api = true
			break
		}
	}
	if !api {
		return false
	}
	var data []bool // Python source offsets that are comment or plain string text; nil when the program may run a string
	if python {
		data = shellIRPyDataMask(src, spans)
	}
	for _, sp := range spans {
		tok := src[sp[0]:sp[1]]
		if shellIRRunTimeName(tok) {
			return true
		}
		if python && shellIRPyPathCreateName(tok) {
			// touch and mkdir create a file or a directory only as methods (Path(...).touch()); a bare name is no such call,
			// and the text p.touch() in a comment or a string the program never runs is no call either.
			if data != nil && data[sp[0]] {
				continue
			}
			if shellIRPrevNonSpace(src, sp[0]) == '.' && (shellIRNextNonSpace(src, sp[1]) != '(' || shellIRPyUnattributedCall(src, sp, tok)) {
				return true
			}
			continue
		}
		if !shellIRWriteNames(tok) {
			continue
		}
		if shellIRPrevNonSpace(src, sp[0]) != '.' {
			return true
		}
		if shellIRNextNonSpace(src, sp[1]) != '(' {
			return true
		}
		if python && shellIRPyUnattributedCall(src, sp, tok) {
			return true
		}
	}
	if python && shellIRPyModeOpenOnModule(src, spans) {
		return true
	}
	return worktreeDelComputedSubscript(src)
}

// shellIRTokenSpans returns the byte spans of the identifier-like tokens of src.
func shellIRTokenSpans(src string) [][2]int {
	var out [][2]int
	start := -1
	for i, r := range src {
		if worktreeDelIdentRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(src)})
	}
	return out
}

// shellIRPrevNonSpace is the last non-blank byte before offset i, or 0 when there is none.
func shellIRPrevNonSpace(src string, i int) byte {
	for j := i - 1; j >= 0; j-- {
		if c := src[j]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c
		}
	}
	return 0
}

// shellIRNextNonSpace is the first non-blank byte at or after offset i, or 0 when there is none.
func shellIRNextNonSpace(src string, i int) byte {
	rest := strings.TrimLeft(src[i:], " \t\r\n")
	if rest == "" {
		return 0
	}
	return rest[0]
}

// shellIRRunTimeName names a call a Python or Node program reaches only at run time: a name read from a string or an
// attribute name built at run time. Builtins that run a literal program (exec of a string) are read by the layer.
func shellIRRunTimeName(tok string) bool {
	switch tok {
	case "getattr", "setattr", "delattr", "__import__", "importlib", "__getattribute__", "__class__", "__subclasses__",
		"__globals__", "constructor", "Function", "eval", "vm":
		return true
	}
	return false
}

// A Python write called as a dot call is attributed to a literal destination only when its receiver is what the verb readers
// read: the module the function belongs to (shutil for a copy, os for a rename), or a Path(...) call. Any other receiver is a name
// the program may have bound to a module or a path in a way the text does not show (s = shutil; io = Path(...); a loop or
// comprehension target; a parameter), a chain (Path(root).joinpath(name)), a subscript or a parenthesised expression: the
// destination is unknown (CRW-951, CRW-998, criterion c1e).

// shellIRPyUnattributedCall reports a dot call of a Python write function whose receiver the verb readers do not read. sp is the
// span of the function name in src.
func shellIRPyUnattributedCall(src string, sp [2]int, name string) bool {
	args, closed := shellIRPyCallArgs(src, sp[1])
	switch name {
	case "write_text", "write_bytes":
		return !shellIRPyPathCallReceiver(src, sp[0])
	case "copy", "copy2", "copyfile", "copytree", "move":
		if closed && len(args) == 0 {
			return false // dict.copy() and list.copy() take no argument
		}
		recv := shellIRPyReceiverIdent(src, sp[0])
		return recv != "shutil" && !shellIRPyImportAlias(src, recv, "shutil")
	case "touch":
		return !shellIRPyPathCallReceiver(src, sp[0])
	case "mkdir":
		// os.mkdir(path) is a different call that this reader has never judged (CRW-951 scope: the pathlib methods only).
		recv := shellIRPyReceiverIdent(src, sp[0])
		return !shellIRPyPathCallReceiver(src, sp[0]) && recv != "os" && !shellIRPyImportAlias(src, recv, "os")
	case "rename", "renames", "symlink", "link":
		recv := shellIRPyReceiverIdent(src, sp[0])
		return !shellIRPyPathCallReceiver(src, sp[0]) && recv != "os" && !shellIRPyImportAlias(src, recv, "os")
	case "replace":
		if recv := shellIRPyReceiverIdent(src, sp[0]); recv == "os" || shellIRPyImportAlias(src, recv, "os") || shellIRPyPathCallReceiver(src, sp[0]) {
			return false
		}
		// str.replace takes two or more arguments; Path.replace takes one (an argument unpacking may carry more).
		return !closed || len(args) <= 1 || shellIRPyHasUnpacking(args)
	}
	return false
}

// shellIRPyReceiverIdent is the identifier that ends the receiver of the dot call whose name starts at name, when the receiver is
// a bare identifier (not a call, a subscript or a parenthesised expression); otherwise "".
func shellIRPyReceiverIdent(src string, name int) string {
	i := name - 1
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	if i < 0 || src[i] != '.' {
		return ""
	}
	i--
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	end := i + 1
	for i >= 0 && worktreeDelIdentRune(rune(src[i])) {
		i--
	}
	if i+1 == end {
		return ""
	}
	// A longer chain (a.b.copy) is not a bare module identifier.
	j := i
	for j >= 0 && (src[j] == ' ' || src[j] == '\t') {
		j--
	}
	if j >= 0 && src[j] == '.' {
		return ""
	}
	return src[i+1 : end]
}

// shellIRPyPathCallReceiver reports a receiver that is a call of Path: Path(...) or pathlib.Path(...), directly before the dot.
func shellIRPyPathCallReceiver(src string, name int) bool {
	i := name - 1
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	if i < 0 || src[i] != '.' {
		return false
	}
	i--
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	if i < 0 || src[i] != ')' {
		return false
	}
	open := shellIRPyMatchOpen(src, i)
	if open < 0 {
		return false
	}
	k := open - 1
	for k >= 0 && (src[k] == ' ' || src[k] == '\t') {
		k--
	}
	end := k + 1
	for k >= 0 && worktreeDelIdentRune(rune(src[k])) {
		k--
	}
	return src[k+1:end] == "Path"
}

// shellIRPyMatchOpen is the index of the bracket that opens the one closed at close, scanning back over nested brackets and
// quoted strings; -1 when there is none.
func shellIRPyMatchOpen(src string, close int) int {
	depth := 0
	for i := close; i >= 0; i-- {
		switch src[i] {
		case ')', ']', '}':
			depth++
		case '(', '[', '{':
			depth--
			if depth == 0 {
				return i
			}
		case '\'', '"':
			q := src[i]
			j := i - 1
			for j >= 0 && src[j] != q {
				j--
			}
			if j < 0 {
				return -1
			}
			i = j
		}
	}
	return -1
}

// shellIRPyCallArgs splits the arguments of the call whose name ends at nameEnd: the top-level pieces between the parentheses.
// closed is false when the call is not closed or no parenthesis follows.
func shellIRPyCallArgs(src string, nameEnd int) (args []string, closed bool) {
	i := nameEnd
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	if i >= len(src) || src[i] != '(' {
		return nil, false
	}
	depth, start := 0, i+1
	for ; i < len(src); i++ {
		switch src[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				if piece := strings.TrimSpace(src[start:i]); piece != "" {
					args = append(args, piece)
				}
				return args, true
			}
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(src[start:i]))
				start = i + 1
			}
		case '\'', '"':
			q := src[i]
			j := i + 1
			for j < len(src) && src[j] != q {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		}
	}
	return nil, false
}

// shellIRPyHasUnpacking reports a * or ** argument.
func shellIRPyHasUnpacking(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "*") {
			return true
		}
	}
	return false
}

// shellIRPyModeOpenOnModule reports an open call on a module name whose first argument is a mode string: io.open("w") is the
// module function opening a file named w only if the name is the module; a program may have bound io to a Path (a parameter, a
// loop target), whose open takes the mode first. The destination is then the receiver's, which the text does not show.
func shellIRPyModeOpenOnModule(src string, spans [][2]int) bool {
	for _, sp := range spans {
		if src[sp[0]:sp[1]] != "open" {
			continue
		}
		recv := shellIRPyReceiverIdent(src, sp[0])
		if recv != "io" && recv != "os" && recv != "codecs" {
			continue
		}
		args, closed := shellIRPyCallArgs(src, sp[1])
		if !closed || len(args) == 0 {
			continue
		}
		if mode, ok := shellVerbLiteral([]rune(args[0])); ok && mode != "" && strings.Trim(mode, "rwaxbt+U") == "" {
			return true
		}
	}
	return false
}

// shellIRPyImportAlias reports that ident is an alias bound only by "import module as ident": a fresh name (never os, shutil, io,
// Path or pathlib, whose meaning the readers force) whose every occurrence is that alias or the receiver of a dot call. Any other
// occurrence (an assignment, a parameter, a loop target, an argument) may rebind it.
func shellIRPyImportAlias(src, ident, module string) bool {
	switch ident {
	case "", "os", "shutil", "io", "Path", "pathlib":
		return false
	}
	bound := false
	for _, sp := range shellIRTokenSpans(src) {
		if src[sp[0]:sp[1]] != ident {
			continue
		}
		before := strings.TrimRight(src[:sp[0]], " \t")
		switch {
		case strings.HasSuffix(before, "as") && !(len(before) > 2 && worktreeDelIdentRune(rune(before[len(before)-3]))):
			if !strings.HasSuffix(strings.TrimRight(before[:len(before)-2], " \t"), module) {
				return false
			}
			// The import reader reads a statement that starts with import: not one after the colon of a compound header.
			start := strings.LastIndexAny(before, ";\n\r") + 1
			if !strings.HasPrefix(strings.TrimSpace(before[start:]), "import ") {
				return false
			}
			bound = true
		case shellIRNextNonSpace(src, sp[1]) == '.':
		default:
			return false
		}
	}
	return bound
}
