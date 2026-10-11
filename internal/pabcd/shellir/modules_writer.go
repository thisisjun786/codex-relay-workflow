package shellir

import (
	"regexp"
	"sort"
	"strings"
)

// The writer check of a module source (CRW-1187): a source that writes, copies, moves or links a file where the interpreter looks for
// modules makes the run import text the reader did not read. A call through a writing module (os, shutil, posix, pathlib, or a name the
// source gives one), an open for writing and an archive's extract are judged by the call: when every path operand of the call is a
// string literal whose name no import reaches (a file with an extension that is not a code extension, outside __pycache__), the call
// writes no module whatever other string of the source ends .py. A call the reader cannot evaluate (an operand that is a name, an
// expression, a call (a Path('x') too: the source may define or rebind Path), an f-string, a concatenation, an escape sequence, a star
// argument, a comment inside the call, or the function taken without a call) keeps the rule it had: refused when the source spells a
// code name anywhere. An extract is judged by its receiver: on an instance of a plain class the source defines (plainClassReceiver) it
// is that class's own method and writes nothing by itself; on any other receiver it is an archive's extract when the source names an
// archive, and a writer of its member when that is a code name.

// writerSlot is one path operand of a writing call: its position, the keywords that may give it, and whether the call needs it.
type writerSlot struct {
	pos      int
	keywords []string
	required bool
}

var (
	twoPathSlots = []writerSlot{{0, []string{"src"}, true}, {1, []string{"dst"}, true}}
	openSlots    = []writerSlot{{0, []string{"path", "file"}, true}}
	extractSlots = []writerSlot{{0, []string{"member"}, true}, {1, []string{"path"}, false}}
)

// writerCallSlots are the attributes of writeAPIAttrs whose path operands name what the call writes; the others (fdopen, copyfileobj,
// unpack_archive, make_archive: a descriptor, an open file, an archive's unknown members) are never judged by their operands.
var writerCallSlots = map[string][]writerSlot{
	"copy": twoPathSlots, "copy2": twoPathSlots, "copyfile": twoPathSlots, "copytree": twoPathSlots, "move": twoPathSlots,
	"rename": twoPathSlots, "renames": twoPathSlots, "replace": twoPathSlots, "link": twoPathSlots, "symlink": twoPathSlots,
	"open": openSlots, "extract": extractSlots,
}

var (
	// reOpenCall: a call of open (or of a method open); openMayWrite reads its mode, writerCallHarmless its file.
	reOpenCall = regexp.MustCompile(`\bopen\s*\(`)
	// reWriteMode: the text of a mode literal that writes.
	reWriteMode = regexp.MustCompile(`^(?:[rRbBuU]|[rR][bB]|[bB][rR])?["'][rbtU]*[wax+][a-z+]*["']$`)
	// reExtractCall: a method named extract; it writes a file for an archive the source opens, or when its operand is a code name.
	reExtractCall = regexp.MustCompile(`\.\s*extract\s*\(`)
	// reArchiveName: the source opens an archive (zipfile, tarfile) in its own text.
	reArchiveName = regexp.MustCompile(`\b(zipfile|tarfile|ZipFile|TarFile)\b`)
	// rePathLiteral: an operand that is a plain string literal (no f-string, no escape). A call of Path, PurePath or x.Path is not one:
	// the name may be the source's own (def Path returning a code name), and the reader does not prove the binding (CRW-1187).
	rePathLiteral = regexp.MustCompile(`^((?:[rRbBuU]|[rR][bB]|[bB][rR])?(?:'[^'\\\n]*'|"[^"\\\n]*"))$`)
	reKeywordArg  = regexp.MustCompile(`^([A-Za-z_]\w*)\s*=([^=][\s\S]*)$`)
)

// codeExtensions are the extensions of a name the interpreter may import, or that changes where it imports from, lower case.
var codeExtensions = map[string]bool{"py": true, "pyc": true, "pyo": true, "pyd": true, "pyw": true, "so": true, "pth": true, "zip": true, "egg": true}

// moduleSourceWriterRefusal is "" for a source none of whose writing calls can write a module, else what the source does.
func moduleSourceWriterRefusal(src string, aliases map[string]string) string {
	if !reCodeLiteral.MatchString(src) {
		return ""
	}
	quals := []string{"os", "shutil", "posix", "pathlib"}
	for a, mod := range aliases {
		if mod == "writer" {
			quals = append(quals, regexp.QuoteMeta(a))
		}
	}
	sort.Strings(quals[4:])
	reQual := regexp.MustCompile(`\b(` + strings.Join(quals, "|") + `)\s*\.\s*(` + strings.TrimSuffix(strings.TrimPrefix(writeAPIAttrs, "(?:"), ")") + `)\b`)
	for _, m := range reQual.FindAllStringSubmatchIndex(src, -1) {
		attr := src[m[4]:m[5]]
		if slots, ok := writerCallSlots[attr]; ok && writerCallHarmless(src, m[1], slots) {
			continue
		}
		return "writes, copies or links a file at an import location (" + src[m[2]:m[3]] + "." + attr + " with a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	for _, m := range reOpenCall.FindAllStringIndex(src, -1) {
		paren := m[1] - 1
		if openDeclared(src, m[0]) {
			continue // def open(self, path, mode): a declaration, its formal arguments are no operands
		}
		if !openMayWrite(src, paren, afterDot(src, m[0])) || !afterDot(src, m[0]) && writerCallHarmless(src, paren, openSlots) {
			continue
		}
		return "writes a file at an import location (open for writing, with a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	archive := reArchiveName.MatchString(src)
	for _, m := range reExtractCall.FindAllStringIndex(src, -1) {
		if plainClassReceiver(src, m[0]) != "" {
			continue
		}
		args, ok := pyCallArgs(src, m[1]-1)
		if !ok {
			return "extracts a file the reader cannot name (extract with operands it does not read, beside a code name)"
		}
		for _, a := range args {
			if reCodeLiteral.MatchString(a) {
				return "extracts a file at an import location (extract of a name ending .py, .pyc, .so, .pth or in __pycache__)"
			}
		}
		if archive && !writerCallHarmless(src, m[1]-1, extractSlots) {
			return "extracts an archive member the reader cannot name (extract on an object it cannot prove plain, beside a code name)"
		}
	}
	for _, m := range reWriteAPI.FindAllStringIndex(src, -1) {
		// shutil.copyfile, sh.copy2: the call through the module was judged above
		if q := attrObject(src, m[0]); q != "" && reQual.MatchString(q+"."+src[m[0]:m[1]]) {
			continue
		}
		return "writes, copies or links a file at an import location (a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	return ""
}

// openDeclared is whether the name open at i is the name of a def (or an async def): the word before it, on the same line with only spaces
// and tabs between, is def. A call of open in the default value of an argument is a match of its own; a def on the line before a line
// join is not read (a comment may end in one), so such a def keeps the rule of a call.
func openDeclared(src string, i int) bool {
	j := i
	for j > 0 && (src[j-1] == ' ' || src[j-1] == '\t') {
		j--
	}
	if j == i || j < 3 || src[j-3:j] != "def" {
		return false
	}
	k := j - 3
	return k == 0 || !(isPyLetter(src[k-1]) || src[k-1] >= '0' && src[k-1] <= '9' || src[k-1] == '_' || src[k-1] >= 0x80)
}

// openMayWrite is whether the open whose parenthesis is at paren may open for writing: its operands are not read, its mode is a write mode
// or not a literal; a method open (Path.open, ZipFile.open) has its mode at another place, so any operand that is not a literal counts.
func openMayWrite(src string, paren int, method bool) bool {
	args, ok := pyCallArgs(src, paren)
	if !ok {
		return true
	}
	mode, found := "", false
	for i, a := range args {
		if strings.HasPrefix(a, "*") {
			return true
		}
		v := a
		if kw := reKeywordArg.FindStringSubmatch(a); kw != nil {
			v = strings.TrimSpace(kw[2])
			if kw[1] == "mode" {
				mode, found = v, true
			}
		} else if i == 1 && !method {
			mode, found = v, true
		}
		if method && (reWriteMode.MatchString(v) || rePathLiteral.FindStringSubmatch(v) == nil) {
			return true
		}
	}
	if method {
		return false
	}
	return found && (reWriteMode.MatchString(mode) || rePathLiteral.FindStringSubmatch(mode) == nil)
}

// attrObject is the name whose attribute stands at i (sh for sh.copyfile), or "".
func attrObject(src string, i int) string {
	if !afterDot(src, i) {
		return ""
	}
	j := strings.LastIndexByte(src[:i], '.')
	for j > 0 && (src[j-1] == ' ' || src[j-1] == '\t') {
		j--
	}
	k := j
	for k > 0 && (isPyLetter(src[k-1]) || src[k-1] >= '0' && src[k-1] <= '9' || src[k-1] == '_') {
		k--
	}
	if k > 0 && (src[k-1] == '.' || src[k-1] >= 0x80) {
		return ""
	}
	return src[k:j]
}

// afterDot is whether the name at i is an attribute (x.open): its object, not its operand, names the file.
func afterDot(src string, i int) bool {
	for i > 0 && (src[i-1] == ' ' || src[i-1] == '\t') {
		i--
	}
	return i > 0 && src[i-1] == '.'
}

// writerCallHarmless is whether the call whose parenthesis follows i names, in every path operand, a file no import reaches.
func writerCallHarmless(src string, i int, slots []writerSlot) bool {
	args, ok := pyCallArgs(src, i)
	if !ok {
		return false
	}
	var positional []string
	keyword := map[string]string{}
	for _, a := range args {
		if strings.HasPrefix(a, "*") {
			return false
		}
		if m := reKeywordArg.FindStringSubmatch(a); m != nil {
			keyword[m[1]] = strings.TrimSpace(m[2])
			continue
		}
		positional = append(positional, a)
	}
	for _, s := range slots {
		v, found := "", false
		if s.pos < len(positional) {
			v, found = positional[s.pos], true
		}
		for _, k := range s.keywords {
			if kv, ok := keyword[k]; ok {
				if found {
					return false
				}
				v, found = kv, true
			}
		}
		if !found {
			if s.required {
				return false
			}
			continue
		}
		if !harmlessPathLiteral(v) {
			return false
		}
	}
	return true
}

// harmlessPathLiteral is whether an operand is a literal name no import reaches.
func harmlessPathLiteral(v string) bool {
	m := rePathLiteral.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	lit := m[1]
	q := strings.IndexAny(lit, `'"`)
	name := strings.ToLower(lit[q+1 : len(lit)-1])
	if name == "" || strings.Contains(name, "__pycache__") || strings.HasSuffix(name, "/") {
		return false
	}
	base := name[strings.LastIndexByte(name, '/')+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return false
	}
	return !codeExtensions[base[dot+1:]]
}

// pyCallArgs splits the operands of the call whose parenthesis follows i (after spaces): the text of each top-level operand, trimmed.
// It is not ok when no parenthesis follows, a comment or an unclosed string or bracket stands in the call.
func pyCallArgs(src string, i int) ([]string, bool) {
	args, _, ok := pyCallSpan(src, i)
	return args, ok
}

// pyCallSpan is pyCallArgs with the index of the call's closing parenthesis.
func pyCallSpan(src string, i int) ([]string, int, bool) {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i >= len(src) || src[i] != '(' {
		return nil, 0, false
	}
	var args []string
	depth, start := 0, i+1
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; c {
		case '\'', '"':
			end := pyStringEnd(src, j)
			if end < 0 {
				return nil, 0, false
			}
			j = end
		case '#':
			return nil, 0, false
		case '(', '[', '{':
			depth++
		case ']', '}':
			depth--
		case ')':
			if depth == 0 {
				if a := strings.TrimSpace(src[start:j]); a != "" {
					args = append(args, a)
				}
				return args, j, true
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(src[start:j]))
				start = j + 1
			}
		}
	}
	return nil, 0, false
}

var (
	// rePlainClass: the header of a class with no base but object, no metaclass and no keyword.
	rePlainClass = regexp.MustCompile(`(?m)^([ \t]*)class[ \t]+([A-Za-z_]\w*)[ \t]*(?:\([ \t]*(?:object[ \t]*)?\))?[ \t]*:[ \t]*$`)
	// reClassMachinery: what can make a class call hand back another object, give an instance another attribute or build the class
	// by other means; a source that holds any of them proves no receiver plain.
	// __setattr__, __code__, __globals__ and the gc referents set an attribute, a method's code or a module's names without naming them.
	reClassMachinery = regexp.MustCompile(`\b(setattr|delattr|vars|globals|locals|exec|eval|compile|__dict__|__build_class__|__class__|__bases__|__mro__|__new__|__getattr__|__getattribute__|__init_subclass__|__set_name__|__prepare__|__get__|metaclass|__setattr__|__delattr__|__code__|__globals__|get_referents|get_referrers|get_objects|ctypes)\b`)
	reExtractWord    = regexp.MustCompile(`\bextract\b`)
	reDefExtract     = regexp.MustCompile(`^def[ \t]+extract[ \t]*\(`)
	// reStarImport: a statement from m import * (from.m import * too) binds names the source does not spell (a class of its own among
	// them). It is matched on the lexer's statements, so one after a semicolon or a compound header (if x: from m import *) counts.
	reStarImport = regexp.MustCompile(`^from\b[^;]*\bimport\s*\*`)
)

// plainClassReceiver is the class Name when the .extract( whose dot is at dot is called on Name(...), a fresh instance of a plain class
// the source defines once (no base but object, no decorator) whose own body defines extract with an undecorated def, where Name is bound
// by nothing else (every other occurrence of the name, in a string or a comment too, is a call of it), extract is bound by nothing else
// (plainExtractWords) and the source holds none of the class machinery (reClassMachinery); else "". Such a call runs the class's own
// method, not an archive's: what that method writes is judged where it is written. Another source of the run can still rebind the
// class or its method, so the inventory is held to the same (moduleSourcesReceiverRefusal) (CRW-1187).
func plainClassReceiver(src string, dot int) string {
	end := dot
	for end > 0 && (src[end-1] == ' ' || src[end-1] == '\t') {
		end--
	}
	if end == 0 || src[end-1] != ')' || reClassMachinery.MatchString(src) || !plainExtractWords(src) {
		return ""
	}
	lexed := pyLex(src)
	if lexed.nonASCIICode || lexed.fUnsure {
		return ""
	}
	// a star import rebinds names unseen, after a semicolon or a compound header too
	for _, stmt := range lexed.stmts {
		if reStarImport.MatchString(stmt) {
			return ""
		}
	}
	// a decorator over an extract (over several lines too) hands back what it likes
	for i, line := range lexed.lines {
		if reDefExtract.MatchString(line) && i > 0 && strings.HasPrefix(lexed.lines[i-1], "@") {
			return ""
		}
	}
	for _, h := range rePlainClass.FindAllStringSubmatchIndex(src, -1) {
		name := src[h[4]:h[5]]
		if plainClassCall(src, name, end-1) && plainClassHeader(lexed.lines, name) && plainClassBody(src, h[0], src[h[2]:h[3]]) {
			return name
		}
	}
	return ""
}

// plainClassHeader is whether the logical lines hold one header of the class name, in code (not in a string), with no decorator before
// it: a decorator over several lines or one holding a colon is one logical line.
func plainClassHeader(lines []string, name string) bool {
	reHeader := regexp.MustCompile(`^class\s+` + regexp.QuoteMeta(name) + `\b`)
	headers, plain := 0, false
	for i, line := range lines {
		if !reHeader.MatchString(line) {
			continue
		}
		headers++
		plain = i == 0 || !strings.HasPrefix(lines[i-1], "@")
	}
	return headers == 1 && plain
}

// plainExtractWords is whether every occurrence of the word extract in src is a call .extract( or the name of a def extract(: nothing
// assigns, sets by name or decorates it.
func plainExtractWords(src string) bool {
	for _, w := range reExtractWord.FindAllStringIndex(src, -1) {
		if afterDot(src, w[0]) {
			if j := skipBlanks(src, w[1]); j < len(src) && src[j] == '(' {
				continue
			}
			return false
		}
		line := strings.LastIndexByte(src[:w[0]], '\n') + 1
		if strings.TrimSpace(src[line:w[0]]) != "def" || !reDefExtract.MatchString(strings.TrimLeft(src[line:], " \t")) {
			return false
		}
	}
	return true
}

// plainReceiverClasses are the classes whose instances' extract the writer check of src took for the class's own method.
func plainReceiverClasses(src string) []string {
	if !reCodeLiteral.MatchString(src) {
		return nil
	}
	var names []string
	for _, m := range reExtractCall.FindAllStringIndex(src, -1) {
		if name := plainClassReceiver(src, m[0]); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// moduleSourcesReceiverRefusal is "" when no source of the run can rebind a class a source's extract was proven plain on: no other source
// names the class (an attribute test_x.Reader, a monkeypatch target, an import of it), sets extract other than by a def, or holds the
// class machinery (type(x).extract, __class__); else the source, the class and the other source.
func moduleSourcesReceiverRefusal(rels, bodies []string) string {
	for i, body := range bodies {
		for _, name := range plainReceiverClasses(body) {
			reName := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
			for j, other := range bodies {
				if j != i && (reName.MatchString(other) || reClassMachinery.MatchString(other) || !plainExtractWords(other)) {
					return rels[i] + " calls extract on " + name + "(), which " + rels[j] + " may rebind (it names the class, sets extract or reaches the class machinery)"
				}
			}
		}
	}
	return ""
}

// plainClassCall is whether some call name(...) ends at the parenthesis at close, and every occurrence of name is its one class header
// or a call of it that is not an attribute.
func plainClassCall(src, name string, close int) bool {
	headers, ends := 0, false
	for _, o := range regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).FindAllStringIndex(src, -1) {
		line := strings.LastIndexByte(src[:o[0]], '\n') + 1
		if strings.TrimSpace(src[line:o[0]]) == "class" {
			headers++
			continue
		}
		if afterDotAcrossLines(src, o[0]) || (o[0] > 0 && src[o[0]-1] >= 0x80) {
			return false
		}
		if w := strings.Fields(src[line:o[0]]); len(w) > 0 && (w[len(w)-1] == "def" || w[len(w)-1] == "class" || w[len(w)-1] == "as" || w[len(w)-1] == "import") {
			return false
		}
		_, c, ok := pyCallSpan(src, o[1])
		if !ok {
			return false
		}
		if c == close {
			ends = true
		}
	}
	return headers == 1 && ends
}

// afterDotAcrossLines is afterDot reading past line ends and line joins as well: inside a bracket a name may stand on the line after the
// dot of its attribute (zipfile.\n ZipFile).
func afterDotAcrossLines(src string, i int) bool {
	for i > 0 && strings.IndexByte(" \t\r\n\\", src[i-1]) >= 0 {
		i--
	}
	return i > 0 && src[i-1] == '.'
}

// plainClassBody is whether the class whose header line starts at start (with that indent) is not decorated and its own body defines
// extract with an undecorated def (plainClassHeader and plainClassReceiver read the decorators over several lines).
func plainClassBody(src string, start int, indent string) bool {
	if prev := lastCodeLine(src[:start]); strings.HasPrefix(strings.TrimSpace(prev), "@") {
		return false
	}
	nl := strings.IndexByte(src[start:], '\n')
	if nl < 0 {
		return false
	}
	body, own, prevLine := "", false, ""
	for _, line := range strings.Split(src[start+nl+1:], "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if len(lead) <= len(indent) {
			break
		}
		if body == "" {
			body = lead
		}
		if lead == body && reDefExtract.MatchString(t) {
			if strings.HasPrefix(strings.TrimSpace(prevLine), "@") {
				return false
			}
			own = true
		}
		prevLine = line
	}
	return own
}

// lastCodeLine is the last line of text that is not blank or a comment, or "".
func lastCodeLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			return lines[i]
		}
	}
	return ""
}

// skipBlanks is the index of the first character at or after i that is not a space or a tab.
func skipBlanks(src string, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i
}

// pyStringEnd is the index of the closing quote of the string whose opening quote is at i (a triple quote too), or -1.
func pyStringEnd(src string, i int) int {
	q := src[i]
	if strings.HasPrefix(src[i:], strings.Repeat(string(q), 3)) {
		for j := i + 3; j+2 < len(src); j++ {
			if src[j] == '\\' {
				j++
				continue
			}
			if src[j] == q && src[j+1] == q && src[j+2] == q {
				return j + 2
			}
		}
		return -1
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '\n':
			return -1
		case q:
			return j
		}
	}
	return -1
}
