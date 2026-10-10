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
func moduleSourceRefusal(src string) string {
	aliases := map[string]bool{"sys": true}
	for _, stmt := range pyStatements(src) {
		if what := pyImportStatement(stmt, aliases); what != "" {
			return what
		}
	}
	for a := range aliases {
		if m := regexp.MustCompile(`\b` + regexp.QuoteMeta(a) + `\s*\.\s*(` + importSysAttrs + `)\b`).FindStringSubmatch(src); m != nil {
			if m[1] == "__dict__" {
				return "reads the namespace of sys (" + a + ".__dict__)"
			}
			return "uses sys." + m[1] + " (as " + a + "." + m[1] + ")"
		}
	}
	for _, m := range reGetattrCall.FindAllStringSubmatch(src, -1) {
		if aliases[m[2]] {
			return "reads sys by name (" + m[1] + "(" + m[2] + ", ...))"
		}
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

// pyStatements splits a Python source into its logical statements: physical lines joined by a backslash or by an open bracket, then
// split at semicolons. The text of strings and comments is kept as it is (a quote is not tracked), so a statement may be cut or joined
// wrongly by a bracket in a string; that only moves where an import statement is seen, and the token scans above read the whole text.
func pyStatements(src string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		for _, part := range strings.Split(cur.String(), ";") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		cur.Reset()
	}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.Index(line, "#"); i >= 0 && depth == 0 && !strings.ContainsAny(line[:i], `"'`) {
			line = line[:i]
		}
		cont := strings.HasSuffix(line, "\\")
		if cont {
			line = strings.TrimSuffix(line, "\\")
		}
		cur.WriteString(line)
		cur.WriteByte(' ')
		depth += strings.Count(line, "(") + strings.Count(line, "[") + strings.Count(line, "{")
		depth -= strings.Count(line, ")") + strings.Count(line, "]") + strings.Count(line, "}")
		if depth < 0 {
			depth = 0
		}
		if !cont && depth == 0 {
			flush()
		}
	}
	flush()
	return out
}

var sysCodeNames = map[string]bool{"path": true, "pycache_prefix": true, "meta_path": true, "path_hooks": true, "path_importer_cache": true, "__dict__": true}

// pyImportStatement judges an import statement: it names the sys aliases, and refuses a bring-in of an import-path attribute of sys,
// of the import machinery or of the imp module.
func pyImportStatement(stmt string, aliases map[string]bool) string {
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
			case mod == "sys" && len(it) == 3 && it[1] == "as":
				aliases[it[2]] = true
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
