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

var (
	// importSysAttr: the attributes of sys that decide where the interpreter finds code.
	importSysAttrs = "path|pycache_prefix|meta_path|path_hooks|path_importer_cache|__dict__"
	// importMachinery: names that load a file by its path or build a finder or loader.
	reMachineryName = regexp.MustCompile(`\b(SourcelessFileLoader|SourceFileLoader|ExtensionFileLoader|SourceLoader|FileLoader|spec_from_file_location|spec_from_loader|module_from_spec|FileFinder|PathFinder|load_source|load_compiled|load_dynamic|run_path|zipimporter|zipimport|zipapp|marshal|compileall|py_compile|addsitedir|pycache_prefix)\b`)
	reImportlibSub  = regexp.MustCompile(`\bimportlib\s*\.\s*(machinery|util)\b`)
	reImportEnv     = regexp.MustCompile(`\b(PYTHONPATH|PYTHONPYCACHEPREFIX|PYTHONHOME|PYTHONSTARTUP|PYTHONSAFEPATH)\b`)
	reSysLiteral    = regexp.MustCompile(`["']sys["']`)
	reCodeLiteral   = regexp.MustCompile(`["'][^"'\n]*(\.(py|pyc|pyd|so|pth)|__pycache__[^"'\n]*)["']`)
	// reWriteAPI: a call that writes, copies, moves or links a file, by a name that is not an everyday word (a bare copy, move or replace is a
	// method of strings and containers) or qualified by os or shutil, or imported from them; open with a write mode.
	reWriteAPI = regexp.MustCompile(`\b(write_text|write_bytes|copyfile|copy2|copytree|copymode|symlink_to|hardlink_to|extractall|truncate|O_WRONLY|O_RDWR|O_CREAT|O_APPEND)\b` +
		`|\b(os|shutil|posix|pathlib)\s*\.\s*(copy|move|rename|renames|replace|link|symlink|extract|open)\b` +
		`|\bfrom\s+(shutil|os|posix)\s+import\s*\(|\bfrom\s+(shutil|os|posix)\s+import\s+[^\n]*\b(copy|move|rename|renames|replace|link|symlink)\b` +
		`|\bopen\s*\([^)]*["'][rbtU]*[wax+][a-z+]*["']`)
	reGetattrCall = regexp.MustCompile(`\b(getattr|setattr|delattr|vars)\s*\(\s*([A-Za-z_][A-Za-z_0-9]*)\b`)
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
	if what := moduleSourceRefusalOf(src, lexed.stmts); what != "" {
		return what
	}
	if lexed.view != src {
		return moduleSourceRefusalOf(lexed.view, lexed.stmts)
	}
	return ""
}

// moduleSourceRefusalOf is the scans of moduleSourceRefusal on one text, with the statements of the source.
func moduleSourceRefusalOf(src string, stmts []string) string {
	// aliases maps each name the source gives to sys, importlib or builtins to the module (import sys as s, import importlib).
	aliases := map[string]string{"sys": "sys", "importlib": "importlib", "builtins": "builtins"}
	for _, stmt := range stmts {
		if what := pyImportStatement(stmt, aliases); what != "" {
			return what
		}
	}
	for a, mod := range aliases {
		qa := regexp.QuoteMeta(a)
		switch mod {
		case "sys":
			if m := regexp.MustCompile(`\b` + qa + `\s*\.\s*(` + importSysAttrs + `)\b`).FindStringSubmatch(src); m != nil {
				if m[1] == "__dict__" {
					return "reads the namespace of sys (" + a + ".__dict__)"
				}
				return "uses sys." + m[1] + " (as " + a + "." + m[1] + ")"
			}
		case "importlib", "builtins":
			if m := regexp.MustCompile(`\b` + qa + `\s*\.\s*__dict__\b`).FindString(src); m != "" {
				return "reads the namespace of " + mod + " (" + a + ".__dict__)"
			}
		}
		if mod == "importlib" {
			for _, m := range regexp.MustCompile(`\b`+qa+`\s*\.\s*([A-Za-z_]\w*)`).FindAllStringSubmatch(src, -1) {
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
	if what := dynamicImportCall(src, aliases); what != "" {
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
	if reCodeLiteral.MatchString(src) && reWriteAPI.MatchString(src) {
		return "writes, copies or links a file at an import location (a name ending .py, .pyc, .so, .pth or in __pycache__)"
	}
	return ""
}

// pyLexed is a Python source split the way the tokenizer sees it.
type pyLexed struct {
	// stmts are the logical statements: split at a newline, a semicolon or a colon outside every string and bracket (so the statement
	// that follows a compound header, "if x: import sys as s", is a statement of its own), with comments removed and a line join read
	// as one space. The text of a string stays in its statement.
	stmts []string
	// view is the source without its comments, with each explicit line join (a backslash before a line end) read as one space.
	view string
	// nonASCIICode is whether a character outside ASCII stands in code (outside a string and a comment).
	nonASCIICode bool
}

// pyLex reads a Python source: strings (single, double and triple quoted, with any prefix, a backslash keeping the next character in
// the string, as in raw strings too), comments, explicit line joins, brackets. A single-quoted string that meets a line end ends
// there (the interpreter refuses such a source, and nothing runs). It does not parse Python: an unbalanced source gives statements that
// are cut where a bracket count says, and that only moves where an import statement is seen; the scans read the whole text as well.
func pyLex(src string) pyLexed {
	var out pyLexed
	var view, cur strings.Builder
	depth := 0
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out.stmts = append(out.stmts, s)
		}
		cur.Reset()
	}
	put := func(b byte) {
		view.WriteByte(b)
		cur.WriteByte(b)
	}
	n := len(src)
	i := 0
	if strings.HasPrefix(src, "\xef\xbb\xbf") {
		i = 3 // a byte order mark is no name
	}
	for i < n {
		c := src[i]
		switch {
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '\\' && i+1 < n && (src[i+1] == '\n' || (src[i+1] == '\r' && i+2 < n && src[i+2] == '\n')):
			if src[i+1] == '\r' {
				i++
			}
			i += 2
			put(' ')
		case c == '\'' || c == '"':
			quote := c
			triple := i+2 < n && src[i+1] == quote && src[i+2] == quote
			width := 1
			if triple {
				width = 3
			}
			for k := 0; k < width; k++ {
				put(quote)
			}
			i += width
			for i < n {
				d := src[i]
				if d == '\\' && i+1 < n {
					put(d)
					put(src[i+1])
					i += 2
					continue
				}
				if d == quote && (!triple || (i+2 < n && src[i+1] == quote && src[i+2] == quote)) {
					for k := 0; k < width; k++ {
						put(quote)
					}
					i += width
					break
				}
				if d == '\n' && !triple {
					break
				}
				put(d)
				i++
			}
		case c == '(' || c == '[' || c == '{':
			depth++
			put(c)
			i++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			put(c)
			i++
		case c == '\n':
			view.WriteByte(c)
			if depth == 0 {
				flush()
			} else {
				cur.WriteByte(' ')
			}
			i++
		case c == ';' && depth == 0:
			view.WriteByte(c)
			flush()
			i++
		case c == ':' && depth == 0 && !(i+1 < n && src[i+1] == '='):
			view.WriteByte(c)
			flush()
			i++
		default:
			if c >= 0x80 {
				out.nonASCIICode = true
			}
			put(c)
			i++
		}
	}
	flush()
	out.view = view.String()
	return out
}

// importlibPlainAttrs are the attributes of importlib a test uses to import by name, to reload or to read package data; the rest of the
// package (machinery, util, abc, the loaders and finders) loads code by path or builds the import system.
var importlibPlainAttrs = map[string]bool{"import_module": true, "reload": true, "invalidate_caches": true, "resources": true, "metadata": true, "__name__": true}

var (
	reLoaderPaths = regexp.MustCompile(`\b(__path__|submodule_search_locations)\b`)
	reSimpleCall  = regexp.MustCompile(`^\s*\(\s*(?:"[A-Za-z0-9_.]*"|'[A-Za-z0-9_.]*')\s*[,)]`)
)

// dynamicImportCall refuses __import__ and import_module (and the names the source gives them) with anything but one plain module name: a
// name built at run time can be sys, importlib or a module found outside the inventory by a path the source set up. A name that is only
// imported or listed in an import statement is no call.
func dynamicImportCall(src string, aliases map[string]string) string {
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
		name := src[loc[0]:loc[1]]
		rest := src[loc[1]:]
		if strings.HasPrefix(strings.TrimLeft(rest, " \t"), "(") {
			if !reSimpleCall.MatchString(rest) {
				return "imports a module by a name built at run time (" + name + ")"
			}
			continue
		}
		prev := strings.TrimRight(src[:loc[0]], " \t\r\n")
		if !strings.HasSuffix(prev, "import") && !strings.HasSuffix(prev, " as") && !strings.HasSuffix(prev, ",") && !strings.HasSuffix(prev, "(") && !strings.HasSuffix(prev, "\\") {
			return "refers to " + name + " other than in an import statement"
		}
	}
	return ""
}

var sysCodeNames = map[string]bool{"path": true, "pycache_prefix": true, "meta_path": true, "path_hooks": true, "path_importer_cache": true, "__dict__": true}

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
			}
		}
	case "from":
		if len(fields) < 4 || fields[2] != "import" {
			return ""
		}
		mod := fields[1]
		names := splitImportItems(fields[3:])
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
				if n[0] == "import_module" && len(n) == 3 && n[1] == "as" {
					aliases[n[2]] = "importfn"
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
