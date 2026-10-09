package shellir

import (
	"path/filepath"
	"strings"
)

// jsonToolUse is one python -m json.tool the walk read: the interpreter name, its directory (the first entry of its module
// search path) and the index of its record in walker.out.
type jsonToolUse struct {
	name string
	dir  Dir
	at   int
}

// jsonModuleName is whether a file of the module search path directory is a module python imports for the name json: a source
// or byte-code module (json.py, json.pyc, json.pyw) or a compiled extension module, bare or with an ABI tag (json.so,
// json.abi3.so, json.cpython-312-x86_64-linux-gnu.so, json.pyd).
func jsonModuleName(n string) bool {
	switch n {
	case "json.py", "json.pyc", "json.pyw":
		return true
	}
	return strings.HasPrefix(n, "json.") && (strings.HasSuffix(n, ".so") || strings.HasSuffix(n, ".pyd"))
}

// jsonToolQuiet are the programs that create no file but through the redirections of their record: next to them the files a
// text writes are its redirections, which checkJSONToolWrites reads. A program outside the list (cp, tar, unzip, curl, dd, an
// interpreter, a shell script, a function) may write a json module the reader cannot name.
var jsonToolQuiet = map[string]bool{
	"": true, "printf": true, "echo": true, "cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true,
	"jq": true, "wc": true, "tr": true, "cut": true, "ls": true, "pwd": true, "cd": true, "true": true, "false": true, ":": true,
	"test": true, "[": true, "sleep": true, "date": true, "wait": true, "set": true, "exit": true,
}

// checkJSONToolWrites proves, once the whole text is read, that no record of it writes a json module where a python -m json.tool
// of the text finds one. Every record counts, the ones after the module too: a loop runs a later copy before the next
// json.tool, and a background or pipeline neighbour runs at the same time. A record other than json.tool itself must be a
// program of jsonToolQuiet, and no redirection may write a json module (json.py, json.so, json.*.so, ...) into the module's
// directory or an __init__ into its json directory; a redirection that may (its target or its directory unknown) is refused.
// Directories are compared after the symbolic links that exist are followed, so a link to the directory is the directory.
func (w *walker) checkJSONToolWrites() error {
	if len(w.jsonTools) == 0 {
		return nil
	}
	self := map[int]bool{}
	for _, u := range w.jsonTools {
		self[u.at] = true
	}
	for _, u := range w.jsonTools {
		if u.dir.Unset {
			continue
		}
		for i, e := range w.out {
			if !self[i] && (e.Kind != KindCommand || !jsonToolQuiet[e.Name]) {
				what := e.Name
				if e.Kind == KindScriptFile && e.Script.Known {
					what = e.Name + " " + e.Script.Value
				}
				return unreadablef("%s -m json.tool runs in a text that runs %s, which may write a json module the reader cannot see", u.name, what)
			}
			for _, r := range e.Redirs {
				if err := jsonToolRedirect(u, r, e.Dir); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// jsonToolRedirect refuses a redirection that writes, or may write, a module python -m json.tool imports.
func jsonToolRedirect(u jsonToolUse, r Redir, recDir Dir) error {
	switch r.Op {
	case "<", "<<", "<<-", "<<<", "<&":
		return nil // reads, here-documents, and descriptor copies create no file
	case ">&":
		if r.Target.Known && (r.Target.Value == "-" || isDigits(r.Target.Value)) {
			return nil // 2>&1, >&-
		}
	}
	if !r.Target.Known {
		return unreadablef("%s -m json.tool runs in a text that writes a file it cannot name (%s)", u.name, r.Target.Reason)
	}
	t := strings.TrimRight(r.Target.Value, "/")
	base := t[strings.LastIndexByte(t, '/')+1:]
	module, init := jsonModuleName(base), strings.HasPrefix(base, "__init__.")
	if !module && !init {
		return nil // a file of any other name is no json module wherever it lies
	}
	if !filepath.IsAbs(t) && !recDir.Known {
		return unreadablef("%s -m json.tool runs in a text that writes %s in a directory the reader does not know", u.name, t)
	}
	if !filepath.IsAbs(t) {
		t = recDir.Path + "/" + t
	}
	parent := realPath(t[:strings.LastIndexByte(t, '/')+1])
	want := realPath(u.dir.Path)
	if init {
		want = realPath(u.dir.Path + "/json")
	}
	if parent == want {
		return unreadablef("%s -m json.tool imports %s, which this text writes", u.name, r.Target.Value)
	}
	return nil
}

// realPath is p with the symbolic links that exist followed (dot-dot after a link leaves the link's target, as the kernel does);
// the part of the path that does not exist is joined as written.
func realPath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	t := strings.TrimRight(p, "/")
	i := strings.LastIndexByte(t, '/')
	if i <= 0 {
		return filepath.Clean(p)
	}
	return filepath.Join(realPath(t[:i]), t[i+1:])
}
