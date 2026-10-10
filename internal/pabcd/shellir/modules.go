package shellir

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Non-executing options are accepted only alone: ruby -v script still runs a script.
func interpreterNoExec(lang string, args []Word) bool {
	if len(args) != 1 || !args[0].Known {
		return false
	}
	v := args[0].Value
	switch lang {
	case "python":
		return v == "-V" || v == "--version" || v == "-h"
	case "node":
		return v == "-v" || v == "--version"
	case "ruby", "perl":
		return v == "-v"
	}
	return false
}

// The module grammar is closed. A module's source files are carried Python
// programs, including bounded discovery; no shell or Python code is executed.
const maxModuleFiles = 128
const maxModuleBytes = 1 << 20

func (w *walker) pythonModule(prog Word, args []Word, assigns []Assign, redirs []Redir, st *state, ctx Context) (bool, error) {
	module, at := "", 0
	for i, a := range args {
		if !a.Known {
			break
		}
		if a.Value == "-m" || strings.HasPrefix(a.Value, "-m") {
			at = i + 1
			module = strings.TrimPrefix(a.Value, "-m")
			if module == "" {
				if at >= len(args) || !args[at].Known {
					return true, unreadablef("python module is not known")
				}
				module = args[at].Value
				at++
			}
			break
		}
		// Only no-argument startup flags precede -m. No code/value option can hide it.
		if !strings.HasPrefix(a.Value, "-") || len(a.Value) < 2 || strings.Trim(a.Value[1:], "BbdEIOPqRsSuvx") != "" {
			break
		}
	}
	if module == "" {
		return false, nil
	}
	switch module {
	case "unittest", "pytest", "py_compile", "json.tool":
	default:
		return true, unreadablef("python module is outside the fixed list")
	}
	if ctx.Loop || ctx.Background || ctx.Unsequenced {
		return true, unreadablef("module imports run in an unordered context")
	}
	if why := w.pycachePrefixSet(args[:at], assigns, st); why != "" {
		return true, unreadablef("PYTHONPYCACHEPREFIX is %s; the interpreter loads compiled code from that cache, which is not read%s", why, pycachePrefixRoute)
	}
	var files []string
	for i := at; i < len(args); i++ {
		a := args[i]
		if !a.Known {
			return true, unreadablef("python module operand is not known")
		}
		v := a.Value
		if strings.HasPrefix(v, "-") {
			switch v {
			case "-v", "-q", "-s", "-f", "-b", "--verbose", "--quiet", "--failfast", "--buffer":
				continue
			default:
				return true, unreadablef("python module option is not modelled")
			}
		}
		if module == "unittest" && !strings.HasSuffix(v, ".py") {
			v = strings.ReplaceAll(v, ".", "/") + ".py"
		}
		files = append(files, v)
	}
	w.out = append(w.out, Exec{Kind: KindCommand, Program: prog, Name: programName(prog.Value), Args: args, Assigns: assigns, Redirs: redirs, Dir: st.dir, Ctx: ctx})
	if module == "json.tool" && len(files) > 1 {
		return true, unreadablef("json.tool output operand is not modelled")
	}
	if st.dir.Unset {
		return true, nil
	} // environment/directory readings own discovery
	if !st.dir.Known || st.dir.Path == "" {
		return true, unreadablef("python module directory is unknown")
	}
	if err := w.moduleImportsClear(st); err != nil {
		return true, err
	}
	// Importing the runner itself can write under a configured cache prefix,
	// even when discovery finds no local source or json.tool only reads data.
	if moduleBytecodeEnabled(args[:at], assigns, st) {
		prefix, known := moduleStartupEnv("PYTHONPYCACHEPREFIX", args[:at], assigns, st)
		if prefix != "" || !known && moduleAssigned("PYTHONPYCACHEPREFIX", assigns) {
			if !known {
				prefix = "\x00unknown"
			}
			name, cacheArgs := fileRecord([]string{prefix})
			w.out = append(w.out, Exec{Kind: KindCommand, Name: name, Args: cacheArgs, Dir: st.dir, Ctx: ctx})
		}
	}
	if module == "json.tool" {
		return true, nil
	}
	if len(files) == 0 && module == "py_compile" {
		return true, unreadablef("py_compile needs file operands")
	}
	physical, err := filepath.EvalSymlinks(st.dir.Path)
	if err != nil {
		return true, unreadablef("module directory cannot be resolved")
	}
	if module == "unittest" || module == "pytest" {
		// Imports, package initializers and collection hooks can execute even
		// with explicit operands. Inspect the bounded local source inventory;
		// the test selection alone never establishes an execution boundary.
		err := filepath.WalkDir(physical, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == ".crw" {
					return filepath.SkipDir
				}
				return nil
			}
			rel := strings.TrimPrefix(p, physical+"/")
			if d.Type()&os.ModeSymlink != 0 {
				return unreadablef("module import inventory contains a link (%s)", rel)
			}
			n := d.Name()
			if strings.HasSuffix(n, ".pyc") {
				// Python loads a cache entry instead of its source whenever the entry's header agrees with the source, and code
				// inside the run (a test module calling os.utime, or rewriting a source) can make any entry agree before it is
				// imported. So no entry is trusted, stale or not; the reason names it and the way past it (CRW-1178).
				return pycacheRefusal(rel)
			}
			if strings.HasSuffix(n, ".pyd") || strings.Contains(n, ".so") {
				return unreadablef("module imports compiled code that is not read (%s)", rel)
			}
			if strings.HasSuffix(n, ".py") {
				files = append(files, p)
				if len(files) > maxModuleFiles*2 {
					return unreadablef("module inventory file limit exceeded")
				}
			}
			return nil
		})
		if err != nil {
			var u *Unreadable
			if errors.As(err, &u) {
				return true, u
			}
			return true, unreadablef("module import inventory cannot be read (%s)", walkErrorWhat(err, physical))
		}
		// Pytest also loads ancestor conftest files and configuration. Config
		// can name plugins whose execution set this reader cannot establish.
		if module == "pytest" {
			for dir := physical; ; dir = filepath.Dir(dir) {
				for _, name := range []string{"pytest.ini", ".pytest.ini", "pyproject.toml", "tox.ini", "setup.cfg"} {
					if _, err := os.Lstat(filepath.Join(dir, name)); err == nil || !os.IsNotExist(err) {
						return true, unreadablef("pytest configuration is not modelled")
					}
				}
				p := filepath.Join(dir, "conftest.py")
				if _, err := os.Lstat(p); err == nil {
					files = append(files, p)
				} else if !os.IsNotExist(err) {
					return true, unreadablef("pytest collection file cannot be read")
				}
				if filepath.Dir(dir) == dir {
					break
				}
			}
		}
	}
	// Preserve operand components until the kernel resolves links and '..'.
	seen := map[string]bool{}
	unique := files[:0]
	for _, file := range files {
		if !filepath.IsAbs(file) {
			file = strings.TrimSuffix(physical, "/") + "/" + file
		}
		if !seen[file] {
			seen[file] = true
			unique = append(unique, file)
		}
	}
	files = unique
	if module == "unittest" || module == "pytest" {
		for _, file := range files {
			resolved, err := filepath.EvalSymlinks(file)
			if err != nil || !(resolved == physical || strings.HasPrefix(resolved, physical+"/")) {
				// External sources may import siblings outside this inventory.
				return true, unreadablef("module operand imports outside the local inventory")
			}
		}
	}

	total := 0
	if len(files) > maxModuleFiles {
		return true, unreadablef("module file limit exceeded")
	}
	for _, file := range files {
		if !filepath.IsAbs(file) {
			file = strings.TrimSuffix(physical, "/") + "/" + file
		}
		if w.createdByText(file, st.dir) {
			return true, unreadablef("module source was rewritten by the command")
		}
		body, err := readModuleFile(file, maxModuleBytes-total)
		if err != nil {
			return true, unreadablef("module file read refused")
		}
		total += len(body)
		if module == "py_compile" || moduleBytecodeEnabled(args[:at], assigns, st) {
			// -B suppresses import caches, but never explicit py_compile.
			// Model the prefix itself conservatively: every cache is below it.
			cache := moduleCacheDir(file, args[:at], assigns, st)
			name, cacheArgs := fileRecord([]string{cache})
			w.out = append(w.out, Exec{Kind: KindCommand, Name: name, Args: cacheArgs, Dir: st.dir, Ctx: ctx})
		}
		w.out = append(w.out, Exec{Kind: KindCommand, Program: prog, Name: programName(prog.Value), Dir: st.dir, Ctx: ctx, Inline: &Inline{Language: "python", Source: Word{Known: true, Value: body}}})
	}
	return true, nil
}

// pycacheRoute and pycachePrefixRoute are the ways past a cache refusal. The cache is removed by a command of its own (in the same
// command the entry is still there when the text is read), and the run then writes no new one; a prefix is not set for the run, or
// the interpreter is told to ignore PYTHON* variables. Both fit the reason bound of the hooks with the file named (see shortRel).
const (
	pycacheRoute       = "; remove __pycache__ in a separate command first, then run with python -B (PYTHONDONTWRITEBYTECODE=1) so no new cache is written"
	pycachePrefixRoute = "; unset PYTHONPYCACHEPREFIX in the environment and do not set it in the command, or run python -E -B"
)

// pycacheRefusal is the refusal of compiled code in the module inventory, which names the file (rel, below the project) and the way past
// it: an entry of a __pycache__ directory is removed with that directory; any other .pyc is a module of its own, removed by name.
func pycacheRefusal(rel string) error {
	if filepath.Base(filepath.Dir(rel)) == "__pycache__" {
		return unreadablef("module imports compiled code that is not read (%s)%s", shortRel(rel), pycacheRoute)
	}
	return unreadablef("module imports compiled code that is not read (%s); remove it in a separate command first", shortRel(rel))
}

// shortRel bounds a project-relative name for a reason, keeping its start and its file name.
func shortRel(rel string) string {
	const limit = 96
	if len(rel) <= limit {
		return rel
	}
	return rel[:limit/2-2] + "..." + rel[len(rel)-limit/2+1:]
}

// pycachePrefixSet says how a module run may be given PYTHONPYCACHEPREFIX, or "" when it is not: the interpreter then reads and writes
// every cache entry under <prefix>/<source directory>, even with -B, where the inventory never looks. The variable is set when the run
// assigns it, when the text names it anywhere (an assignment, export, declare, eval, env: every literal way to set it spells the name),
// when a command of the text may set a variable whose name the reader does not know, and when the environment the reader is given sets
// it (empty is unset for the interpreter). -E and -I make the interpreter ignore it (CRW-1178).
func (w *walker) pycachePrefixSet(args []Word, assigns []Assign, st *state) string {
	if moduleIgnoresEnv(args) {
		return ""
	}
	switch {
	case moduleAssigned("PYTHONPYCACHEPREFIX", assigns):
		return "assigned for the run"
	case w.prefixNamed:
		return "named by the command text"
	case w.prefixUnknown:
		return "possibly set by a command whose variable names are not known"
	}
	if st.lookup != nil {
		if v, ok := st.lookup("PYTHONPYCACHEPREFIX"); ok && v != "" {
			return "set in the environment"
		}
	}
	return ""
}

// moduleIgnoresEnv is whether the interpreter's startup flags include -E or -I, with which it ignores every PYTHON* variable.
func moduleIgnoresEnv(args []Word) bool {
	for _, a := range args {
		if a.Known && strings.HasPrefix(a.Value, "-") && !strings.HasPrefix(a.Value, "--") && strings.ContainsAny(a.Value[1:], "EI") {
			return true
		}
	}
	return false
}

// textNamesPycachePrefix is whether a text spells PYTHONPYCACHEPREFIX anywhere, as textNamesCdpath is for CDPATH.
func textNamesPycachePrefix(src string) bool {
	return strings.Contains(src, "PYTHONPYCACHEPREFIX")
}

// walkErrorWhat names what failed in a directory walk: the operation, the cause and the file or directory it failed on, relative to the
// project root (physical), so the host's path is not repeated and the reader's reason says what to fix.
func walkErrorWhat(err error, root string) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		what := pe.Op + ": " + pe.Err.Error()
		switch {
		case pe.Path == root:
			what += " in ."
		case strings.HasPrefix(pe.Path, root+"/"):
			what += " in " + strings.TrimPrefix(pe.Path, root+"/")
		}
		return what
	}
	return "walk failed"
}

func moduleEnv(name string, assigns []Assign, st *state) (string, bool) {
	for i := len(assigns) - 1; i >= 0; i-- {
		if assigns[i].Name == name {
			return assigns[i].Value.Value, assigns[i].Value.Known
		}
	}
	return st.value(name), st.known(name)
}
func moduleAssigned(name string, assigns []Assign) bool {
	for _, a := range assigns {
		if a.Name == name {
			return true
		}
	}
	return false
}
func moduleStartupEnv(name string, args []Word, assigns []Assign, st *state) (string, bool) {
	if moduleIgnoresEnv(args) {
		return "", true // Python -E and -I ignore PYTHON* environment variables.
	}
	return moduleEnv(name, assigns, st)
}

func moduleBytecodeEnabled(args []Word, assigns []Assign, st *state) bool {
	for _, a := range args {
		if a.Known && strings.HasPrefix(a.Value, "-") && !strings.HasPrefix(a.Value, "--") && strings.Contains(a.Value[1:], "B") {
			return false
		}
	}
	v, known := moduleStartupEnv("PYTHONDONTWRITEBYTECODE", args, assigns, st)
	return !known || v == ""
}
func moduleCacheDir(file string, args []Word, assigns []Assign, st *state) string {
	prefix, known := moduleStartupEnv("PYTHONPYCACHEPREFIX", args, assigns, st)
	if !known {
		for _, a := range assigns {
			if a.Name == "PYTHONPYCACHEPREFIX" {
				return "\x00unknown"
			}
		}
	}
	if prefix != "" {
		return prefix
	}
	return file[:strings.LastIndexByte(file, '/')] + "/__pycache__"
}

func readModuleFile(path string, limit int) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > int64(limit) {
		return "", unreadablef("module file is not bounded regular data")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", unreadablef("module byte limit exceeded")
	}
	return string(b), nil
}

// The fixed modules still import from cwd. Refuse local shadows (including
// compiled files), and command-time code writes whose imports cannot be proven.
func (w *walker) moduleImportsClear(st *state) error {
	names := map[string]bool{}
	for _, n := range strings.Fields("json argparse re shutil inspect unittest pytest py_compile os sys pathlib importlib typing collections contextlib traceback linecache warnings functools types enum dataclasses io tokenize token ast copy gettext _pytest pluggy site sitecustomize usercustomize") {
		names[n] = true
	}
	entries, err := os.ReadDir(st.dir.Path)
	if err != nil {
		return unreadablef("module import directory cannot be read")
	}
	for _, e := range entries {
		base := strings.SplitN(e.Name(), ".", 2)[0]
		if !names[base] {
			continue
		}
		if e.IsDir() {
			children, err := os.ReadDir(filepath.Join(st.dir.Path, e.Name()))
			if err != nil {
				return unreadablef("module package cannot be read")
			}
			for _, c := range children {
				if strings.HasPrefix(c.Name(), "__init__.") {
					return unreadablef("local package shadows a fixed module import")
				}
			}
		} else if strings.HasSuffix(e.Name(), ".py") || strings.HasSuffix(e.Name(), ".pyc") || strings.Contains(e.Name(), ".so") || strings.HasSuffix(e.Name(), ".pyd") || e.Type()&os.ModeSymlink != 0 {
			return unreadablef("local file shadows a fixed module import")
		}
	}
	codePath := func(v string) bool {
		return strings.HasSuffix(v, ".py") || strings.HasSuffix(v, ".pyc") || strings.Contains(v, ".so") || strings.HasSuffix(v, ".pyd") || names[filepath.Base(v)] || strings.Contains(v, "__pycache__")
	}
	for _, e := range w.out {
		if e.Kind == KindScriptFile || e.Inline != nil || e.Name == "tar" || e.Name == "unzip" {
			return unreadablef("a carried program may rewrite module imports")
		}
		for _, r := range e.Redirs {
			if strings.Contains(r.Op, ">") && (!r.Target.Known || codePath(r.Target.Value)) {
				return unreadablef("module import may be rewritten")
			}
		}
		switch e.Name {
		case "cp", "mv", "ln", "install", "tee", "dd", "curl", "wget", "touch":
			for _, a := range e.Args {
				if !a.Known || codePath(strings.TrimPrefix(a.Value, "of=")) {
					return unreadablef("module import may be rewritten")
				}
			}
		}
	}
	return nil
}
