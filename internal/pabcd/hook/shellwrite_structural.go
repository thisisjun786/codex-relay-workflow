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
		"copy", "copy2", "copyfile", "copyFile", "copytree", "move", "rename", "renameSync", "replace", "symlink", "link":
		return true
	}
	return false
}

// shellIRStructuralWriteUnknown reports a program that holds a file API and a write the reader cannot attribute to a literal
// destination: a bare or unattached write name, a run-time name (getattr, __import__, eval, ...), or a computed subscript.
func shellIRStructuralWriteUnknown(src string) bool {
	spans := shellIRTokenSpans(src)
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
		if shellIRPrevNonSpace(src, sp[0]) != '.' {
			return true
		}
		if shellIRNextNonSpace(src, sp[1]) != '(' {
			return true
		}
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
