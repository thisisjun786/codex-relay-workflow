package hook

import (
	"regexp"
	"strings"
)

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

// shellIRStructuralWriteUnknown reports a program that holds a file API and a write the reader cannot attribute to a literal
// destination: a bare or unattached write name, a run-time name (getattr, __import__, eval, ...), or a computed subscript.
func shellIRStructuralWriteUnknown(src string, python bool) bool {
	spans := shellIRTokenSpans(src)
	imports := shellIRFromImportsOf(src, python)
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
	for _, sp := range spans {
		tok := src[sp[0]:sp[1]]
		if shellIRRunTimeName(tok) {
			return true
		}
		if !shellIRWriteNames(tok) {
			continue
		}
		if imports.skips(sp[0]) || imports.calls(tok) {
			continue // a from-import of shutil or os binds this name, and the reader names the destination of each call to it (CRW-900 D4)
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
		return !(recv == "shutil" && shellIRPyPlainModule(src, "shutil")) && !shellIRPyImportAlias(src, recv, "shutil")
	case "rename", "renames", "symlink", "link":
		recv := shellIRPyReceiverIdent(src, sp[0])
		return !shellIRPyPathCallReceiver(src, sp[0]) && !(recv == "os" && shellIRPyPlainModule(src, "os")) && !shellIRPyImportAlias(src, recv, "os")
	case "replace":
		if recv := shellIRPyReceiverIdent(src, sp[0]); (recv == "os" && shellIRPyPlainModule(src, "os")) || shellIRPyImportAlias(src, recv, "os") || shellIRPyPathCallReceiver(src, sp[0]) {
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

// shellIRPyImportAlias reports that ident is an alias bound only by "import module as ident": a name (never io, Path or pathlib,
// whose meaning the readers force; os and shutil may be the alias of each other, CRW-900 D2) whose every occurrence is that alias
// or the receiver of a dot call. Any other
// occurrence (an assignment, a parameter, a loop target, an argument) may rebind it.
func shellIRPyImportAlias(src, ident, module string) bool {
	switch ident {
	case "", "io", "Path", "pathlib":
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
			// The statement must be exactly "import module as ident": a statement after the colon of a compound header counts
			// (CRW-900 D1), and a longer import list does not.
			start := strings.LastIndexAny(before, ";\n\r:") + 1
			if strings.TrimSpace(before[start:len(before)-2]) != "import "+module {
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

// shellIRFromImports is what the from-imports of shutil and os in a Python program bind for the structural check: the offsets
// of the imported and alias tokens of each copy, rename or link import that ends its statement, and the local names those
// imports bind. A local name is kept only when every use of it is a call of that name.
type shellIRFromImports struct {
	skip  map[int]bool
	local map[string]bool
}

// shellIRFromHeadRe matches the start of a from-import of shutil or os: at the start of the program, or after a semicolon, a
// line break or a compound header's colon. The imported names follow it.
var shellIRFromHeadRe = regexp.MustCompile(`(?:^|[:;\r\n])[ \t]*from[ \t]+(shutil|os)[ \t]+import[ \t]+`)

// shellIRFromItem is one name of a from-import list: the imported name, its alias (if any) and the offsets of both tokens.
type shellIRFromItem struct {
	name, alias     string
	nameAt, aliasAt int
}

// shellIRFromItems reads a from-import list that starts at off: name [as alias] items separated by commas, ending at the end of
// the statement. Any other shape (a parenthesised list, a star, a missing comma) is not read.
func shellIRFromItems(src string, off int) ([]shellIRFromItem, bool) {
	var items []shellIRFromItem
	ident := func(p int) int {
		for p < len(src) && (src[p] == '_' || src[p] >= 'a' && src[p] <= 'z' || src[p] >= 'A' && src[p] <= 'Z' || src[p] >= '0' && src[p] <= '9') {
			p++
		}
		return p
	}
	skipSpace := func(p int) int {
		for p < len(src) && (src[p] == ' ' || src[p] == '\t') {
			p++
		}
		return p
	}
	p := off
	for {
		p = skipSpace(p)
		end := ident(p)
		if end == p {
			return nil, false
		}
		item := shellIRFromItem{name: src[p:end], nameAt: p}
		p = skipSpace(end)
		if strings.HasPrefix(src[p:], "as") && p+2 < len(src) && (src[p+2] == ' ' || src[p+2] == '\t') {
			p = skipSpace(p + 2)
			end = ident(p)
			if end == p {
				return nil, false
			}
			item.alias, item.aliasAt = src[p:end], p
			p = skipSpace(end)
		}
		items = append(items, item)
		if p < len(src) && src[p] == ',' {
			p++
			continue
		}
		if p == len(src) || src[p] == ';' || src[p] == '\n' || src[p] == '\r' {
			return items, true
		}
		return nil, false
	}
}

// shellIRFromImportsOf reads the from-imports of a Python program: each name a from-import of shutil or os binds to a copy,
// rename or link function, in a statement that ends after the list. Any other name is left out, so it stays a bare write name.
func shellIRFromImportsOf(src string, python bool) shellIRFromImports {
	out := shellIRFromImports{skip: map[int]bool{}, local: map[string]bool{}}
	if !python {
		return out
	}
	type cand struct {
		local string
		at    []int
	}
	var cands []cand
	for _, m := range shellIRFromHeadRe.FindAllStringSubmatchIndex(src, -1) {
		items, ok := shellIRFromItems(src, m[1])
		if !ok {
			continue
		}
		module := src[m[2]:m[3]]
		for _, item := range items {
			if !shellWriteCopyFunc(module, item.name) {
				continue
			}
			c := cand{local: item.name, at: []int{item.nameAt}}
			if item.alias != "" {
				c.local = item.alias
				c.at = append(c.at, item.aliasAt)
			}
			cands = append(cands, c)
		}
	}
	active := make([]bool, len(cands))
	for i := range active {
		active[i] = true
	}
	for changed := true; changed; {
		changed = false
		out.skip, out.local = map[int]bool{}, map[string]bool{}
		for i, c := range cands {
			if active[i] {
				for _, at := range c.at {
					out.skip[at] = true
				}
				out.local[c.local] = true
			}
		}
		for i, c := range cands {
			if active[i] && !shellIRFromUsesAreCalls(src, c.local, out.skip) {
				active[i], changed = false, true
			}
		}
	}
	return out
}

// shellIRFromUsesAreCalls reports that every use of local outside the import tokens skip names is a call of it: a name whose
// value is taken (f = c, g(c)) may be a write the text does not show, so the import is not accepted.
func shellIRFromUsesAreCalls(src, local string, skip map[int]bool) bool {
	for _, use := range shellIRTokenSpans(src) {
		if src[use[0]:use[1]] != local || skip[use[0]] {
			continue
		}
		if shellIRNextNonSpace(src, use[1]) != '(' || shellIRPrevNonSpace(src, use[0]) == '.' || strings.HasSuffix(strings.TrimRight(src[:use[0]], " \t"), "def") {
			return false
		}
	}
	return true
}

// skips reports that the token at offset is an imported or alias name of a from-import the check accepts.
func (f shellIRFromImports) skips(offset int) bool {
	return f.skip[offset]
}

// calls reports that tok is a local name of an accepted from-import: each of its uses is a call, so the reader names them.
func (f shellIRFromImports) calls(tok string) bool {
	return f.local[tok]
}

// shellIRPyPlainModule reports that ident still names the module it is named for: every use of it is a plain import (import os,
// shutil), or the receiver of a dot call. A parameter, an assignment, an alias or a loop target may rebind the name, so a call
// on it is then not the module's function, and the reader's binding does not show it (CRW-900 post-evaluation d2).
func shellIRPyPlainModule(src, ident string) bool {
	for _, sp := range shellIRTokenSpans(src) {
		if src[sp[0]:sp[1]] != ident || shellIRNextNonSpace(src, sp[1]) == '.' {
			continue
		}
		before := strings.TrimRight(src[:sp[0]], " \t")
		if strings.HasSuffix(before, "import") && (len(before) == 6 || !worktreeDelIdentRune(rune(before[len(before)-7]))) {
			continue
		}
		if strings.HasSuffix(before, ",") {
			start := strings.LastIndexAny(before, ";\n\r:") + 1
			if strings.HasPrefix(strings.TrimSpace(before[start:]), "import ") {
				continue
			}
		}
		return false
	}
	return true
}
