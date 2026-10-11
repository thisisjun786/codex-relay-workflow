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

func (w *walker) pythonModule(prog Word, args []Word, assigns []Assign, redirs []Redir, st *state, ctx Context) (handled bool, err error) {
	// first is where the records of this run begin; a run that passed the checks of its directory is kept in w.moduleRuns, to judge
	// the records that run beside it (writersBesideModules, CRW-1187).
	first, tracked := -1, false
	defer func() {
		if tracked && err == nil {
			w.moduleRuns = append(w.moduleRuns, moduleRun{first: first, end: len(w.out)})
		}
	}()
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
	first = len(w.out)
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
	if err := w.moduleImportsClear(module, args[:at], assigns, st); err != nil {
		return true, err
	}
	tracked = true
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
	var rels, bodies []string
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
		if module == "unittest" || module == "pytest" {
			// A source that moves the import path or loads a file by path runs code the inventory did not read (CRW-1187). py_compile
			// only compiles its operands, so their text imports nothing.
			if what := moduleSourceRefusal(body); what != "" {
				return true, unreadablef("module source %s %s: the reader cannot read what it loads; remove that from the test sources or keep the code to be run in the project files (python -I does not change this)", shortRel(strings.TrimPrefix(file, physical+"/")), what)
			}
			rels, bodies = append(rels, shortRel(strings.TrimPrefix(file, physical+"/"))), append(bodies, body)
		}
		if module == "py_compile" || moduleBytecodeEnabled(args[:at], assigns, st) {
			// -B suppresses import caches, but never explicit py_compile.
			// Model the prefix itself conservatively: every cache is below it.
			cache := moduleCacheDir(file, args[:at], assigns, st)
			name, cacheArgs := fileRecord([]string{cache})
			w.out = append(w.out, Exec{Kind: KindCommand, Name: name, Args: cacheArgs, Dir: st.dir, Ctx: ctx})
		}
		w.out = append(w.out, Exec{Kind: KindCommand, Program: prog, Name: programName(prog.Value), Dir: st.dir, Ctx: ctx, Inline: &Inline{Language: "python", Source: Word{Known: true, Value: body}}})
	}
	if what := moduleSourcesReceiverRefusal(rels, bodies); what != "" {
		return true, unreadablef("module source %s: the reader cannot read what it loads (python -I does not change this)", what)
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
// assigns it, when the text names it anywhere (an assignment, export, declare, eval, env), when a command or an expansion of the text
// may assign a variable whose name it does not spell (a name built at run time: export "$N", ${!N:=x}, an arithmetic evaluation of a
// value the walk does not know or that names other variables, declare -i), and when the environment the reader is given sets it
// (empty is unset for the interpreter). -E and -I make the interpreter ignore it (CRW-1178).
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
		return "possibly set through a variable name the command text does not spell"
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

// moduleFixedNames are the module names a unittest or pytest run, and a code path written before a run, is judged against.
var moduleFixedNames = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields("json argparse re shutil inspect unittest pytest py_compile os sys pathlib importlib typing collections contextlib traceback linecache warnings functools types enum dataclasses io tokenize token ast copy gettext _pytest pluggy site sitecustomize usercustomize") {
		m[n] = true
	}
	return m
}()

// moduleRun is the records one module run left in the walk: out[first:end], the run itself and the sources it carries.
type moduleRun struct{ first, end int }

// moduleSafePath is whether the run starts with no entry for the working directory in sys.path: -I, or -P with PYTHONPATH proven unset
// or empty (-P alone keeps PYTHONPATH, which may name the directory; -EP ignores it; a text that spells the name, builds it from parts the walk
// knows (printf -v "$N$M") or may build a variable name at run time proves nothing; neither does a reading that is given no environment). The python -m of json.tool and py_compile then imports
// nothing from the directory.
func (w *walker) moduleSafePath(args []Word, assigns []Assign, st *state) bool {
	isolated, noPath, noEnv := false, false, false
	for _, a := range args {
		if !a.Known || !strings.HasPrefix(a.Value, "-") || strings.HasPrefix(a.Value, "--") || strings.HasPrefix(a.Value, "-m") {
			continue
		}
		isolated = isolated || strings.Contains(a.Value[1:], "I")
		noPath = noPath || strings.Contains(a.Value[1:], "P")
		noEnv = noEnv || strings.Contains(a.Value[1:], "E")
	}
	if isolated {
		return true
	}
	if noPath && noEnv {
		// -E makes the interpreter ignore every PYTHON* variable, PYTHONPATH among them, and -P leaves the directory out of sys.path: the
		// environment, and the names the text builds, cannot put it back (CRW-1187).
		return true
	}
	if !noPath || w.pythonPathNamed || w.prefixUnknown || moduleAssigned("PYTHONPATH", assigns) {
		return false
	}
	if st.lookup == nil {
		// No environment is known, so PYTHONPATH may name the directory: -P alone proves nothing (the guard that is given none answers
		// with -I, or runs the module in the directory it checked).
		return false
	}
	if v, ok := st.lookup("PYTHONPATH"); ok && v != "" {
		return false
	}
	return true
}

// The fixed modules still import from cwd. Refuse local shadows (including
// compiled files), and command-time code writes whose imports cannot be proven.
// The list for unittest and pytest is the names their own imports use; their sources are read by the inventory walk. json.tool and
// py_compile read no inventory, so every module name of the standard library the interpreter can import at start is a shadow there
// (stdlibShadowName), unless the run leaves the working directory out of sys.path (moduleSafePath).
func (w *walker) moduleImportsClear(module string, flags []Word, assigns []Assign, st *state) error {
	names := moduleFixedNames
	reads := module == "json.tool" || module == "py_compile"
	broad := reads && !w.moduleSafePath(flags, assigns, st)
	// A run of json.tool or py_compile that leaves the directory out of sys.path imports nothing from it, whatever the names in it are
	// (the fixed names of the list included); the writer check below still applies (CRW-1187).
	isolated := reads && !broad
	entries, err := os.ReadDir(st.dir.Path)
	if err != nil {
		return unreadablef("module import directory cannot be read")
	}
	for _, e := range entries {
		if isolated {
			break
		}
		base := strings.SplitN(e.Name(), ".", 2)[0]
		if !names[base] && !(broad && stdlibShadowName(base)) {
			continue
		}
		if e.IsDir() {
			children, err := os.ReadDir(filepath.Join(st.dir.Path, e.Name()))
			if err != nil {
				return unreadablef("module package cannot be read")
			}
			for _, c := range children {
				if strings.HasPrefix(c.Name(), "__init__.") {
					return unreadablef("local package shadows a fixed module import (%s)%s", shortRel(e.Name()), shadowRoute(module, broad))
				}
			}
		} else if strings.HasSuffix(e.Name(), ".py") || strings.HasSuffix(e.Name(), ".pyc") || strings.Contains(e.Name(), ".so") || strings.HasSuffix(e.Name(), ".pyd") || e.Type()&os.ModeSymlink != 0 {
			return unreadablef("local file shadows a fixed module import (%s)%s", shortRel(e.Name()), shadowRoute(module, broad))
		}
	}
	for _, e := range w.out {
		if err := moduleWriterRefusal(e); err != nil {
			return err
		}
	}
	return nil
}

// shadowRoute is the way past a shadow refusal of a run that reads no inventory: python -I leaves the directory out of sys.path.
func shadowRoute(module string, broad bool) string {
	if broad {
		return "; run python -I -m " + module + " or move the file"
	}
	return ""
}

// codePath is whether a written name is where the interpreter looks for code.
func moduleCodePath(v string) bool {
	names := moduleFixedNames
	return strings.HasSuffix(v, ".py") || strings.HasSuffix(v, ".pyc") || strings.Contains(v, ".so") || strings.HasSuffix(v, ".pyd") || names[filepath.Base(v)] || strings.Contains(v, "__pycache__")
}

// moduleWriterRefusal is the refusal of one record that may write module imports: a carried program, an archive extraction, a redirection
// or a copy-like command whose destination is not known or is a code path.
func moduleWriterRefusal(e Exec) error {
	if e.Kind == KindScriptFile || e.Inline != nil || e.Name == "tar" || e.Name == "unzip" {
		return unreadablef("a carried program may rewrite module imports")
	}
	for _, r := range e.Redirs {
		if strings.Contains(r.Op, ">") && (!r.Target.Known || moduleCodePath(r.Target.Value)) {
			return unreadablef("module import may be rewritten")
		}
	}
	switch e.Name {
	case "cp", "mv", "ln", "install", "tee", "dd", "curl", "wget", "touch":
		for _, a := range e.Args {
			if !a.Known || moduleCodePath(strings.TrimPrefix(a.Value, "of=")) {
				return unreadablef("module import may be rewritten")
			}
		}
	case "uniq", "split", "csplit", "patch", "rsync", "gzip", "bzip2", "xz", "zstd", "zip", "sed",
		"shuf", "iconv", "xxd", "openssl", "gpg", "gpg2", "ar", "cpio", "bsdtar", "7z", "7za", "7zr", "jar", "ed", "ex", "truncate":
		// programs that write a file an operand names (the output of uniq, a piece of split, a patched or compressed file): refused
		// when an operand they are given is a code path, or cannot be evaluated (read OUT <<< late.py; ... | uniq payload.txt "$OUT"), as
		// for cp, tee and sort (CRW-1187).
		for _, a := range e.Args {
			if !a.Known || moduleCodePath(a.Value) {
				return unreadablef("module import may be rewritten")
			}
		}
	case "sort":
		// sort writes the file of -o/--output, in any of the spellings getopt_long accepts (the reading the write gate uses).
		for _, d := range SortOutputDests(e.Args, moduleWordText, moduleUnknownWord) {
			if d == moduleUnknownWord || moduleCodePath(d) {
				return unreadablef("module import may be rewritten")
			}
		}
	}
	return nil
}

// moduleUnknownWord marks a word the reader cannot evaluate in a destination list; moduleWordText is a word's text or that mark.
const moduleUnknownWord = "\x00unknown"

func moduleWordText(w Word) string {
	if !w.Known || strings.Contains(w.Value, "%") {
		return moduleUnknownWord
	}
	return w.Value
}

// writersBesideModules refuses a record that runs at the same time as a module run and may write module imports: a background job or a
// coprocess (before or after the run in the text), a process substitution, or another stage of the pipeline the run is in (CRW-1187).
// The earlier records of the text are judged by moduleImportsClear; a record that runs after the module has ended is not judged.
func (w *walker) writersBesideModules() error {
	for _, m := range w.moduleRuns {
		mctx := w.out[m.first].Ctx
		for i, e := range w.out {
			if i >= m.first && i < m.end {
				continue
			}
			beside := e.Ctx.Background || e.Ctx.Coprocess || e.Ctx.ProcSubst || (mctx.pipeID != 0 && e.Ctx.pipeID == mctx.pipeID)
			if !beside {
				continue
			}
			if err := moduleWriterRefusal(e); err != nil {
				return unreadablef("%s: a command runs beside the module run (background, coprocess, process substitution or the same pipeline); run it before or after the module in a separate command", err.(*Unreadable).Reason)
			}
		}
	}
	return nil
}
