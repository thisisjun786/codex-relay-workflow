package shellir

import (
	"regexp"
	"strings"
)

// A module run reads the Python sources of its inventory and takes them for the whole code that runs. A source that moves the
// import path, sets a cache prefix, loads a file through the import machinery by path or writes a file where the interpreter looks
// for modules makes the interpreter run code the reader did not read, whatever the startup flags are (python -I still lets the
// source change sys.path). moduleSourceRefusal finds those in the text of a source at token level and refuses it, naming the file and the
// construct (CRW-1187). The list is closed and narrow: an ordinary import, importlib.import_module and importlib.reload, reading
// sys.argv, sys.modules or sys.executable, and writes to files that are not module files stay allowed. Python the text builds or
// fetches at run time (getattr on a name built from pieces, globals(), exec of a string) is the reader's general limit and is not
// claimed here.

// writeAPIAttrs are the attributes of os, shutil, posix and pathlib that write, copy, move or link a file (a regular expression group).
const writeAPIAttrs = `(?:copy|copy2|copyfile|copyfileobj|copytree|move|rename|renames|replace|link|symlink|extract|open|fdopen|unpack_archive|make_archive)`

var (
	// importSysAttr: the attributes of sys that decide where the interpreter finds code.
	importSysAttrs = "path|pycache_prefix|meta_path|path_hooks|path_importer_cache|__dict__"
	// importMachinery: names that load a file by its path or build a finder or loader.
	reMachineryName = regexp.MustCompile(`\b(SourcelessFileLoader|SourceFileLoader|ExtensionFileLoader|SourceLoader|FileLoader|spec_from_file_location|spec_from_loader|module_from_spec|FileFinder|PathFinder|load_source|load_compiled|load_dynamic|run_path|zipimporter|zipimport|zipapp|marshal|compileall|py_compile|addsitedir|pycache_prefix)\b`)
	reImportlibSub  = regexp.MustCompile(`\bimportlib\s*\.\s*(machinery|util)\b`)
	reImportEnv     = regexp.MustCompile(`\b(PYTHONPATH|PYTHONPYCACHEPREFIX|PYTHONHOME|PYTHONSTARTUP|PYTHONSAFEPATH)\b`)
	reSysLiteral    = regexp.MustCompile(`["']sys["']`)
	reCodeLiteral   = regexp.MustCompile(`["'][^"'\n]*(\.(py|pyc|pyd|so|pth)|__pycache__[^"'\n]*)["']`)
	// reWriteAPI: a write, copy, move or link of a file by a name that is not an everyday word (a bare copy, move or replace is a method of
	// strings and containers), or imported from a writing module (a star import too). The calls through a writing module, an open for
	// writing and an archive's extract are judged by their operands (moduleSourceWriterRefusal).
	reWriteAPI = regexp.MustCompile(`\b(write_text|write_bytes|copyfile|copy2|copytree|copymode|copyfileobj|unpack_archive|make_archive|symlink_to|hardlink_to|extractall|truncate|O_WRONLY|O_RDWR|O_CREAT|O_APPEND)\b` +
		// a path object moved onto an import location (Path('x').rename('late.py'), p.replace(Path('late.py'))): the one operand is a code name
		// (str.replace has two, so a replace of ".py" in a name is not this)
		`|\.\s*(rename|replace)\s*\(\s*(?:(?:[\w.]*\bPath|PurePath)\s*\(\s*)?[rbuf]*["'][^"'\n]*(\.(py|pyc|pyd|so|pth)|__pycache__[^"'\n]*)["']\s*\)+` +
		`|\bfrom\s+(shutil|os|posix|pathlib)\s+import\s*[(*]|\bfrom\s+(shutil|os|posix)\s+import\s+[^\n]*\b(copy|move|rename|renames|replace|link|symlink)\b`)
	reGetattrCall = regexp.MustCompile(`\b(getattr|setattr|delattr|vars)\s*\(\s*(?:\(\s*)*([A-Za-z_][A-Za-z_0-9]*)\b`)
)

// moduleSourceRefusal is "" for a source whose imports stay in the inventory, else what the source does, in a few words.
//
// The source is read as the interpreter's tokenizer would split it before any scan (pyLex): strings and comments are told apart from
// code, an explicit line join is a space, and a name Python would fold to another (a non-ASCII identifier is normalized to NFKC) is
// refused where it stands in code. The scans below run on the text as written and on the text without its comments and line joins; the
// statements the aliases of sys, importlib and builtins are collected from are the lexer's (CRW-1187).
func moduleSourceRefusal(src string) string {
	lexed := pyLex(src)
	if lexed.nonASCIICode {
		return "uses a name with non-ASCII characters outside a string, which Python folds to other names (sys can be written as another spelling)"
	}
	if lexed.fUnsure {
		return "holds an f-string the reader does not split (a comment in a replacement field, or a backslash before a brace or N)"
	}
	if m := reAttrSys.FindString(lexed.code); m != "" {
		return "reaches sys as an attribute of another module (" + strings.Join(strings.Fields(m), "") + "), which is sys.path by another name"
	}
	if what := moduleSourceRefusalOf(src, lexed.stmts); what != "" {
		return what
	}
	if lexed.view != src {
		return moduleSourceRefusalOf(lexed.view, lexed.stmts)
	}
	return ""
}

// reAttrAfter is what may stand between a name and the dot of an attribute of the object a bracket around it gives: (sys), ((sys)),
// (s := sys), (sys or x), [sys][0], (sys, x)[0], {'k': sys}['k'] (CRW-1187). The name with no bracket after it is the plain s.path.
const reAttrAfter = `(?:[^()\[\]{}]*[)\]}]|\s*\[[^\[\]\n]*\])*\s*\.\s*`

// reAttrSys: a name sys that is an attribute (os.sys, a.b.sys): the module sys under another module's name.
var reAttrSys = regexp.MustCompile(`\.\s*sys\b`)

// moduleSourceRefusalOf is the scans of moduleSourceRefusal on one text, with the statements of the source.
func moduleSourceRefusalOf(src string, stmts []string) string {
	// aliases maps each name the source gives to sys, importlib or builtins to the module (import sys as s, import importlib).
	aliases := map[string]string{"sys": "sys", "importlib": "importlib", "builtins": "builtins", "__builtins__": "builtins"}
	for _, stmt := range stmts {
		if what := pyImportStatement(stmt, aliases); what != "" {
			return what
		}
	}
	for a, mod := range aliases {
		qa := regexp.QuoteMeta(a)
		switch mod {
		case "sys":
			if m := regexp.MustCompile(`\b` + qa + reAttrAfter + `(` + importSysAttrs + `)\b`).FindStringSubmatch(src); m != nil {
				if m[1] == "__dict__" {
					return "reads the namespace of sys (" + a + ".__dict__)"
				}
				return "uses sys." + m[1] + " (as " + a + "." + m[1] + ")"
			}
		case "importlib", "builtins":
			if m := regexp.MustCompile(`\b` + qa + reAttrAfter + `__dict__\b`).FindString(src); m != "" {
				return "reads the namespace of " + mod + " (" + a + ".__dict__)"
			}
		}
		if mod == "importlib" {
			for _, m := range regexp.MustCompile(`\b`+qa+reAttrAfter+`([A-Za-z_]\w*)`).FindAllStringSubmatch(src, -1) {
				if !importlibPlainAttrs[m[1]] {
					return "uses importlib." + m[1] + " (as " + a + "." + m[1] + ")"
				}
			}
		}
	}
	for _, m := range reGetattrCall.FindAllStringSubmatch(src, -1) {
		if mod, ok := aliases[m[2]]; ok {
			return "reads " + mod + " by name (" + m[1] + "(" + m[2] + ", ...))"
		}
	}
	if what := dynamicImportCall(src, stmts, aliases); what != "" {
		return what
	}
	if m := reLoaderPaths.FindString(src); m != "" {
		return "changes where a package looks for its modules (" + m + ")"
	}
	if reSysLiteral.MatchString(src) {
		return "fetches the module sys by its name as a string"
	}
	if m := reImportlibSub.FindStringSubmatch(src); m != nil {
		return "uses importlib." + m[1]
	}
	if m := reMachineryName.FindString(src); m != "" {
		return "uses " + m + " to load code by path"
	}
	if m := reImportEnv.FindString(src); m != "" {
		return "names " + m + ", which redirects imports"
	}
	return moduleSourceWriterRefusal(src, aliases)
}

// pyLexed is a Python source split the way the tokenizer sees it.
type pyLexed struct {
	// stmts are the logical statements: split at a newline, a semicolon or a colon outside every string and bracket (so the statement
	// that follows a compound header, "if x: import sys as s", is a statement of its own), with comments removed and a line join read
	// as one space. The text of a string stays in its statement.
	stmts []string
	// lines are the logical lines: split only at a newline outside every string and bracket, with comments removed and a line join
	// read as one space (a decorator over several lines, or one that holds a colon, is one line).
	lines []string
	// view is the source without its comments, with each explicit line join (a backslash before a line end) read as one space.
	view string
	// code is view with the text of every string that is not an f-string (or a t-string) blanked: an f-string holds expressions, and
	// it is kept whole. What a name or an attribute in code is matched against, so that a word in a string (a mock.patch target) is not
	// taken for a use of it.
	code string
	// nonASCIICode is whether a character outside ASCII stands in code: outside a string and a comment, or in the expression of a
	// replacement field of an f-string.
	nonASCIICode bool
	// fUnsure is whether an f-string holds a form whose split the reader does not follow: a comment in a replacement field, or a
	// backslash before a brace or before N (\N{...} is an escape in one string and a replacement field in a raw one).
	fUnsure bool
}

// pyLexer is the state of pyLex.
type pyLexer struct {
	src             string
	i               int
	view, cur, code strings.Builder
	line            strings.Builder
	out             pyLexed
}

// put writes a byte of code (or a byte of an f-string, which is kept whole) to the statement and both views.
func (l *pyLexer) put(b byte) {
	l.view.WriteByte(b)
	l.cur.WriteByte(b)
	l.line.WriteByte(b)
	l.code.WriteByte(b)
}

// putString writes a byte of a string: the statement and the view keep it, the code view keeps it only for an f-string.
func (l *pyLexer) putString(b byte, keep bool) {
	l.view.WriteByte(b)
	l.cur.WriteByte(b)
	l.line.WriteByte(b)
	if keep || b == '\n' {
		l.code.WriteByte(b)
	} else {
		l.code.WriteByte(' ')
	}
}

// pyStringPrefixes are the prefixes of a string literal, lower case; f and t make it a string with replacement fields.
var pyStringPrefixes = map[string]bool{"": true, "r": true, "u": true, "b": true, "br": true, "rb": true, "f": true, "fr": true, "rf": true, "t": true, "tr": true, "rt": true}

// stringPrefix is the prefix of the string whose quote is at i, lower case: the whole name before the quote when it is a string prefix,
// else "" (a name such as if or else before a quote is a keyword, and the string has no prefix).
func stringPrefix(src string, i int) string {
	j := i
	for j > 0 && (isPyLetter(src[j-1]) || src[j-1] >= '0' && src[j-1] <= '9' || src[j-1] == '_' || src[j-1] >= 0x80) {
		j--
	}
	p := strings.ToLower(src[j:i])
	if pyStringPrefixes[p] {
		return p
	}
	return ""
}

// closes is whether the string of quote (triple or not) ends at l.i.
func (l *pyLexer) closes(quote byte, triple bool) bool {
	s, i := l.src, l.i
	if s[i] != quote {
		return false
	}
	return !triple || (i+2 < len(s) && s[i+1] == quote && s[i+2] == quote)
}

// str reads the string whose quote is at l.i. inF: the string stands in a replacement field of an f-string, which is kept whole.
func (l *pyLexer) str(inF bool) {
	s, n := l.src, len(l.src)
	quote := s[l.i]
	prefix := stringPrefix(s, l.i)
	fields := strings.ContainsAny(prefix, "ft")
	triple := l.i+2 < n && s[l.i+1] == quote && s[l.i+2] == quote
	width := 1
	if triple {
		width = 3
	}
	for k := 0; k < width; k++ {
		l.put(quote)
	}
	l.i += width
	if fields {
		l.fLiteral(quote, triple, true)
		return
	}
	keep := inF
	for l.i < n {
		d := s[l.i]
		if d == '\\' && l.i+1 < n {
			l.putString(d, keep)
			l.putString(s[l.i+1], keep)
			l.i += 2
			continue
		}
		if l.closes(quote, triple) {
			for k := 0; k < width; k++ {
				l.put(quote)
			}
			l.i += width
			return
		}
		if d == '\n' && !triple {
			return // the interpreter refuses the source
		}
		l.putString(d, keep)
		l.i++
	}
}

// fLiteral reads the text of an f-string from l.i to its closing quote (top) or, in a format spec (top false), to the brace that ends
// the replacement field the spec belongs to. A replacement field is read by fField (PEP 701: its expression may hold strings in any
// quote, the quote of the f-string too).
func (l *pyLexer) fLiteral(quote byte, triple, top bool) {
	s, n := l.src, len(l.src)
	width := 1
	if triple {
		width = 3
	}
	for l.i < n {
		d := s[l.i]
		switch {
		case d == '\\' && l.i+1 < n:
			if e := s[l.i+1]; e == '{' || e == '}' || e == 'N' {
				l.out.fUnsure = true
			}
			l.put(d)
			l.put(s[l.i+1])
			l.i += 2
		case l.closes(quote, triple):
			if top {
				for k := 0; k < width; k++ {
					l.put(quote)
				}
				l.i += width
			}
			return // a spec that meets the quote leaves it to the literal around it (the interpreter refuses the source)
		case d == '\n' && !triple:
			return
		case d == '{' && l.i+1 < n && s[l.i+1] == '{' && top:
			l.put(d)
			l.put(d)
			l.i += 2
		case d == '{':
			l.put(d)
			l.i++
			l.fField(quote, triple)
		case d == '}' && !top:
			l.put(d)
			l.i++
			return
		default:
			l.put(d)
			l.i++
		}
	}
}

// fField reads the expression of a replacement field from l.i to the brace that ends it, with its format spec.
func (l *pyLexer) fField(quote byte, triple bool) {
	s, n := l.src, len(l.src)
	depth := 0
	for l.i < n {
		d := s[l.i]
		switch {
		case d == '\'' || d == '"':
			l.str(true)
		case d == '#':
			l.out.fUnsure = true
			l.put(d)
			l.i++
		case d == '(' || d == '[' || d == '{':
			depth++
			l.put(d)
			l.i++
		case (d == ')' || d == ']') && depth > 0:
			depth--
			l.put(d)
			l.i++
		case d == '}':
			l.put(d)
			l.i++
			if depth == 0 {
				return
			}
			depth--
		case d == ':' && depth == 0:
			l.put(d)
			l.i++
			l.fLiteral(quote, triple, false)
			return
		default:
			if d >= 0x80 {
				l.out.nonASCIICode = true
			}
			l.put(d)
			l.i++
		}
	}
}

// pyLex reads a Python source: strings (single, double and triple quoted, with any prefix, a backslash keeping the next character in
// the string, as in raw strings too; f-strings and t-strings with their replacement fields), comments, explicit line joins, brackets. A
// single-quoted string that meets a line end ends there (the interpreter refuses such a source, and nothing runs). It does not parse
// Python: an unbalanced source gives statements that are cut where a bracket count says, and that only moves where an import statement
// is seen; the scans read the whole text as well.
func pyLex(src string) pyLexed {
	l := &pyLexer{src: src}
	depth := 0
	flush := func() {
		if s := strings.TrimSpace(l.cur.String()); s != "" {
			l.out.stmts = append(l.out.stmts, s)
		}
		l.cur.Reset()
	}
	flushLine := func() {
		if s := strings.TrimSpace(l.line.String()); s != "" {
			l.out.lines = append(l.out.lines, s)
		}
		l.line.Reset()
	}
	n := len(src)
	if strings.HasPrefix(src, "\xef\xbb\xbf") {
		l.i = 3 // a byte order mark is no name
	}
	for l.i < n {
		c := src[l.i]
		switch {
		case c == '#':
			for l.i < n && src[l.i] != '\n' {
				l.i++
			}
		case c == '\\' && l.i+1 < n && (src[l.i+1] == '\n' || (src[l.i+1] == '\r' && l.i+2 < n && src[l.i+2] == '\n')):
			if src[l.i+1] == '\r' {
				l.i++
			}
			l.i += 2
			l.put(' ')
		case c == '\'' || c == '"':
			l.str(false)
		case c == '(' || c == '[' || c == '{':
			depth++
			l.put(c)
			l.i++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			l.put(c)
			l.i++
		case c == '\n':
			l.view.WriteByte(c)
			l.code.WriteByte(c)
			if depth == 0 {
				flush()
				flushLine()
			} else {
				l.cur.WriteByte(' ')
				l.line.WriteByte(' ')
			}
			l.i++
		case c == ';' && depth == 0:
			l.view.WriteByte(c)
			l.code.WriteByte(c)
			l.line.WriteByte(c)
			flush()
			l.i++
		case c == ':' && depth == 0 && !(l.i+1 < n && src[l.i+1] == '='):
			l.view.WriteByte(c)
			l.code.WriteByte(c)
			l.line.WriteByte(c)
			flush()
			l.i++
		default:
			if c >= 0x80 {
				l.out.nonASCIICode = true
			}
			l.put(c)
			l.i++
		}
	}
	flush()
	flushLine()
	l.out.view = l.view.String()
	l.out.code = l.code.String()
	return l.out
}

// isPyLetter is whether a byte can be part of a string prefix.
func isPyLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// importlibPlainAttrs are the attributes of importlib a test uses to import by name, to reload or to read package data; the rest of the
// package (machinery, util, abc, the loaders and finders) loads code by path or builds the import system.
var importlibPlainAttrs = map[string]bool{"import_module": true, "reload": true, "invalidate_caches": true, "resources": true, "metadata": true, "__name__": true}

var (
	reLoaderPaths = regexp.MustCompile(`\b(__path__|submodule_search_locations)\b`)
	reSimpleCall  = regexp.MustCompile(`^\s*\(\s*(?:"[A-Za-z0-9_.]*"|'[A-Za-z0-9_.]*')\s*[,)]`)
)

// dynamicImportCall refuses __import__ and import_module (and the names the source gives them) with anything but one plain module name: a
// name built at run time can be sys, importlib or a module found outside the inventory by a path the source set up. A name that is not
// called stands only in an import statement ((__import__)(...), [__import__][0](...) and an argument are references that are called
// elsewhere).
func dynamicImportCall(src string, stmts []string, aliases map[string]string) string {
	names := []string{"__import__", "import_module"}
	for a, mod := range aliases {
		if mod == "importfn" {
			names = append(names, a)
		}
	}
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`\b(` + strings.Join(names, "|") + `)\b`)
	for _, loc := range re.FindAllStringIndex(src, -1) {
		rest := src[loc[1]:]
		if strings.HasPrefix(strings.TrimLeft(rest, " \t"), "(") && !reSimpleCall.MatchString(rest) {
			return "imports a module by a name built at run time (" + src[loc[0]:loc[1]] + ")"
		}
	}
	for _, stmt := range stmts {
		if reImportStmt.MatchString(stmt) {
			continue
		}
		for _, loc := range re.FindAllStringIndex(stmt, -1) {
			if !strings.HasPrefix(strings.TrimLeft(stmt[loc[1]:], " \t"), "(") {
				return "refers to " + stmt[loc[0]:loc[1]] + " other than in an import statement"
			}
		}
	}
	return ""
}

// reImportStmt is a whole import statement as pyLex gives it: import a, b as c / from a.b import (c, d as e).
var reImportStmt = regexp.MustCompile(`^(?:import\s+[\w.]+(?:\s+as\s+\w+)?(?:\s*,\s*[\w.]+(?:\s+as\s+\w+)?)*\s*,?|from\s+\.*[\w.]*\s+import\s+(?:\(\s*)?[\w\s,*]*(?:\)\s*)?)$`)

// builtinsByName are the builtins that reach an attribute or a namespace by a name given at run time: under another name, the scans for
// them do not see the call.
var builtinsByName = map[string]bool{"getattr": true, "setattr": true, "delattr": true, "vars": true, "globals": true, "locals": true, "eval": true, "exec": true, "compile": true}

var sysCodeNames = map[string]bool{"path": true, "pycache_prefix": true, "meta_path": true, "path_hooks": true, "path_importer_cache": true, "__dict__": true}

// writerModules are the modules whose attributes write, copy, move or link files: a name the source gives them (import shutil as sh) is
// matched against the attributes of writeAPIAttrs like the module's own name, in moduleSourceWriterRefusal (CRW-1187).
var writerModules = map[string]bool{"os": true, "shutil": true, "posix": true, "pathlib": true}

// pyImportStatement judges an import statement: it names the sys aliases, and refuses a bring-in of an import-path attribute of sys,
// of the import machinery or of the imp module.
func pyImportStatement(stmt string, aliases map[string]string) string {
	fields := strings.Fields(strings.NewReplacer("(", " ", ")", " ", ",", " , ").Replace(stmt))
	if len(fields) < 2 {
		return ""
	}
	// A statement may follow a compound header on its line ("if x: import sys"): look from the keyword.
	for i, f := range fields {
		if (f == "import" || f == "from") && (i == 0 || strings.HasSuffix(fields[i-1], ":") || fields[i-1] == "else" || fields[i-1] == "try:") {
			fields = fields[i:]
			break
		}
	}
	switch fields[0] {
	case "import":
		items := splitImportItems(fields[1:])
		for _, it := range items {
			mod := it[0]
			switch {
			case mod == "imp" || strings.HasPrefix(mod, "imp."):
				return "imports imp, which loads files by path"
			case mod == "importlib.machinery" || mod == "importlib.util":
				return "imports " + mod
			case (mod == "sys" || mod == "importlib" || mod == "builtins") && len(it) == 3 && it[1] == "as":
				aliases[it[2]] = mod
			case writerModules[mod] && len(it) == 3 && it[1] == "as":
				aliases[it[2]] = "writer"
			}
		}
	case "from":
		if len(fields) < 4 || fields[2] != "import" {
			return ""
		}
		mod := fields[1]
		names := splitImportItems(fields[3:])
		if mod != "sys" {
			for _, n := range names {
				if n[0] == "sys" {
					return "imports sys from " + mod + ", which is sys.path by another name"
				}
			}
		}
		switch {
		case mod == "imp" || strings.HasPrefix(mod, "imp."):
			return "imports from imp, which loads files by path"
		case mod == "importlib.machinery" || mod == "importlib.util" || strings.HasPrefix(mod, "importlib.machinery.") || strings.HasPrefix(mod, "importlib.util."):
			return "imports from " + mod
		case mod == "importlib":
			for _, n := range names {
				if n[0] == "machinery" || n[0] == "util" || n[0] == "*" {
					return "imports importlib." + n[0]
				}
				if (n[0] == "import_module" || n[0] == "__import__") && len(n) == 3 && n[1] == "as" {
					aliases[n[2]] = "importfn"
				}
			}
		case mod == "builtins":
			for _, n := range names {
				if n[0] == "__import__" && len(n) == 3 && n[1] == "as" {
					aliases[n[2]] = "importfn"
				}
				if builtinsByName[n[0]] && len(n) == 3 && n[1] == "as" {
					return "imports builtins." + n[0] + " under another name (" + n[2] + ")"
				}
			}
		case mod == "sys":
			for _, n := range names {
				if sysCodeNames[n[0]] || n[0] == "*" {
					return "imports sys." + n[0] + " by name"
				}
			}
		}
	}
	return ""
}

// splitImportItems reads "a , b as c" into [[a] [b as c]] (name, then as and the alias when there is one).
func splitImportItems(fields []string) [][]string {
	var items [][]string
	var cur []string
	for _, f := range fields {
		if f == "," {
			if len(cur) > 0 {
				items = append(items, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, f)
	}
	if len(cur) > 0 {
		items = append(items, cur)
	}
	return items
}
