package shellir

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/syntax"
)

func quotedArgs(word *syntax.Word) bool {
	if len(word.Parts) != 1 {
		return false
	}
	quoted, ok := word.Parts[0].(*syntax.DblQuoted)
	if !ok || len(quoted.Parts) != 1 {
		return false
	}
	parameter, ok := quoted.Parts[0].(*syntax.ParamExp)
	return ok && parameter.Param != nil && parameter.Param.Value == "@" && parameter.Exp == nil && parameter.Slice == nil && parameter.Repl == nil && parameter.Index == nil && parameter.NestedParam == nil && !parameter.Length && !parameter.Width && !parameter.Excl && parameter.Names == 0 && len(parameter.Modifiers) == 0 && parameter.Flags == nil
}

func pathAssignmentPrefix(word *syntax.Word) string {
	parts := word.Parts
	if len(parts) == 1 {
		if quoted, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = quoted.Parts
		}
	}
	if len(parts) > 0 {
		if literal, ok := parts[0].(*syntax.Lit); ok && strings.HasPrefix(literal.Value, "PATH=") {
			return "PATH"
		}
	}
	return ""
}

func samePath(left, right *Word) bool {
	return left == right || left != nil && right != nil && *left == *right
}

func pathIndependent(name string, st *state) bool {
	if _, ok := st.funcs[name]; ok {
		return true
	}
	switch name {
	case ":", "echo", "printf", "true", "false", "test", "[", "cd", "chdir", "pushd", "popd", "exit", "return", "break", "continue", "eval", "source", ".", "trap", "read", "mapfile", "readarray", "getopts", "unset", "export", "declare", "typeset", "local", "readonly", "set", "shift", "let", "command", "builtin", "exec", "time":
		return true
	}
	return false
}

func systemProgram(program string) bool {
	directory, _ := filepath.Split(program)
	if directory != "/bin/" && directory != "/usr/bin/" && directory != "/usr/local/bin/" && directory != "/opt/homebrew/bin/" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return false
	}
	descriptor, err := syscall.Open(resolved, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(descriptor), resolved)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	head := make([]byte, 4096)
	count, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	return !bytes.HasPrefix(head[:count], []byte("#!")) && bytes.IndexByte(head[:count], 0) >= 0
}

func resolveProgram(program Word, st *state) (Word, error) {
	if st.dir.Unset {
		return program, nil
	}
	if !st.searchPath.Known {
		return Word{}, unreadablef("PATH is not known; the external program may run protected operations")
	}
	for _, directory := range strings.Split(st.searchPath.Value, ":") {
		if !filepath.IsAbs(directory) {
			if !st.dir.Known {
				return Word{}, unreadablef("PATH has a relative directory and the working directory is unknown")
			}
			directory = strings.TrimSuffix(st.dir.Path, "/") + "/" + directory
		}
		candidate := strings.TrimSuffix(directory, "/") + "/" + program.Value
		info, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Word{}, unreadablef("PATH program cannot be inspected")
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return Word{Known: true, Value: candidate}, nil
		}
	}
	return Word{}, unreadablef("PATH does not resolve the external program")
}
