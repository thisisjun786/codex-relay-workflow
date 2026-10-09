package shellir

import (
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
	if module == "json.tool" {
		return true, nil
	}
	if len(files) == 0 && module == "py_compile" {
		return true, unreadablef("py_compile needs file operands")
	}
	if len(args[at:]) == 0 || len(files) == 0 {
		err := filepath.WalkDir(st.dir.Path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == ".crw" || d.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			n := d.Name()
			if strings.HasSuffix(n, ".py") && (strings.HasPrefix(n, "test") || strings.HasSuffix(n, "_test.py") || module == "pytest" && n == "conftest.py") {
				files = append(files, p)
				if len(files) > maxModuleFiles {
					return unreadablef("module discovery file limit exceeded")
				}
			}
			return nil
		})
		if err != nil {
			return true, unreadablef("module discovery refused")
		}
	}
	total := 0
	if len(files) > maxModuleFiles {
		return true, unreadablef("module file limit exceeded")
	}
	for _, file := range files {
		if !filepath.IsAbs(file) {
			file = filepath.Join(st.dir.Path, file)
		}
		if w.createdByText(file, st.dir) {
			return true, unreadablef("module source was rewritten by the command")
		}
		body, err := readModuleFile(file, maxModuleBytes-total)
		if err != nil {
			return true, unreadablef("module file read refused")
		}
		total += len(body)
		w.out = append(w.out, Exec{Kind: KindCommand, Program: prog, Name: programName(prog.Value), Dir: st.dir, Ctx: ctx, Inline: &Inline{Language: "python", Source: Word{Known: true, Value: body}}})
	}
	return true, nil
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
	for _, n := range strings.Fields("json argparse re shutil inspect unittest pytest py_compile os sys pathlib importlib typing collections contextlib traceback linecache warnings functools types enum dataclasses io tokenize token ast copy") {
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
		return strings.HasSuffix(v, ".py") || strings.HasSuffix(v, ".pyc") || strings.Contains(v, ".so") || strings.HasSuffix(v, ".pyd") || names[filepath.Base(v)]
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
		case "cp", "mv", "ln", "install", "tee", "dd", "curl", "wget":
			for _, a := range e.Args {
				if !a.Known || codePath(strings.TrimPrefix(a.Value, "of=")) {
					return unreadablef("module import may be rewritten")
				}
			}
		}
	}
	return nil
}
