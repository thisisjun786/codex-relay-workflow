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
	return Word{}, errPathNotFound
}

// errPathNotFound is resolveProgram's answer for a name no directory of a known PATH holds.
var errPathNotFound = unreadablef("PATH does not resolve the external program")

// prefixPath gives a PATH assignment in front of a command its effect: the command (and what it carries) finds programs on that
// PATH, and the returned function puts the PATH of the shell back once the command is read. Everything else the command does to
// the shell (cd, read, printf -v) stays, as it does when the assignment lasts for the command only. A POSIX special builtin keeps
// such an assignment after it in some shells and not in others, and a function call does in bash's POSIX mode, so these are
// refused rather than read one way (CRW-1283).
func (s *state) prefixPath(name string, words []Word, assigns []Assign, launched bool) (func(), error) {
	assigned := false
	for _, a := range assigns {
		assigned = assigned || a.Name == "PATH"
	}
	if !assigned {
		return nil, nil
	}
	_, function := s.funcs[name]
	if !launched && (function || specialBuiltin(name) && !(name == "exec" && execRunsProgram(words[1:]))) {
		return nil, unreadablef("an assignment to PATH in front of %s may outlast it in some shells; assign PATH in a statement of its own", name)
	}
	saved := s.searchPath
	savedValue, hadValue := s.vars["PATH"]
	for _, a := range assigns {
		if a.Name == "PATH" {
			s.setVar(a.Name, a.Value, a.Append)
		}
	}
	temporary := s.searchPath
	return func() {
		if s.searchPath != temporary {
			// The command itself changed what the walk knows of PATH (read, declare): the PATH after it is not known.
			s.searchPath = &Word{Reason: "PATH may have changed"}
			delete(s.vars, "PATH")
			return
		}
		s.searchPath = saved
		if hadValue {
			s.vars["PATH"] = savedValue
		} else {
			delete(s.vars, "PATH")
		}
	}, nil
}

// execRunsProgram is whether exec's operands name a program: exec with only redirections changes the shell itself.
func execRunsProgram(args []Word) bool {
	for _, a := range args {
		if !a.Known || !strings.HasPrefix(a.Value, "-") {
			return true
		}
	}
	return false
}

// specialBuiltin names the POSIX special builtins (and source, bash's spelling of .): an assignment in front of one outlasts it in
// a POSIX shell.
func specialBuiltin(name string) bool {
	switch name {
	case "break", ":", ".", "source", "continue", "eval", "exec", "exit", "export", "readonly", "return", "set", "shift", "times",
		"trap", "unset":
		return true
	}
	return false
}

// positionalChange is whether a builtin may change the positional parameters that a later "$@" expands to: set with an operand
// or with -- or -, shift, and the builtins that run text of their own (eval, source, trap) or redefine words (alias).
func positionalChange(name string, args []Word) bool {
	switch name {
	case "shift", "eval", "source", ".", "trap", "alias":
		return true
	case "set":
		return !setOptionsOnly(args)
	}
	return false
}

// setOptionsOnly is whether set's words are options alone (set -euo pipefail, set +x): each word a cluster of option letters, an
// o last in a cluster taking the next word as the name of an option.
func setOptionsOnly(args []Word) bool {
	for i := 0; i < len(args); i++ {
		if !args[i].Known {
			return false
		}
		v := args[i].Value
		if len(v) < 2 || v[0] != '-' && v[0] != '+' {
			return false
		}
		for k := 1; k < len(v); k++ {
			c := v[k]
			if c == 'o' {
				if k != len(v)-1 {
					return false
				}
				if i+1 < len(args) {
					i++
					if !args[i].Known || !optionName(args[i].Value) {
						return false
					}
				}
				continue
			}
			if !strings.ContainsRune("abefhkmnptuvxBCEHPT", rune(c)) {
				return false
			}
		}
	}
	return true
}

func optionName(v string) bool {
	if v == "" {
		return false
	}
	for _, c := range v {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// pathBinaryIdentified admits a program an assigned PATH finds outside the system directories. A text file (a script) is read by
// the consumers like a script run by its path. A binary is read by its name only, which says nothing of a copy or a link named
// otherwise, so it is admitted only when the PATH the shell started with (the environment of this process) runs the same file
// for the name: the assignment then does not change which program runs. Any other binary is refused (CRW-1283, Sion decision 5).
func pathBinaryIdentified(name, program string) error {
	binary, err := programIsBinary(program)
	if err != nil {
		return err
	}
	if !binary {
		return nil
	}
	if ambient, ok := ambientProgram(name); ok {
		left, leftErr := os.Stat(program)
		right, rightErr := os.Stat(ambient)
		if leftErr == nil && rightErr == nil && os.SameFile(left, right) {
			return nil
		}
	}
	return unreadablef("%s is a binary that the assigned PATH finds outside the system directories and the PATH the shell starts with does not run; run it by its absolute path", program)
}

// programIsBinary is whether an executable file is a binary: a file whose head has no #! line and a NUL, as the consumers read it.
func programIsBinary(program string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return false, unreadablef("PATH program cannot be inspected")
	}
	descriptor, err := syscall.Open(resolved, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, unreadablef("PATH program cannot be inspected")
	}
	file := os.NewFile(uintptr(descriptor), resolved)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, unreadablef("PATH program is not a regular file")
	}
	head := make([]byte, 4096)
	count, err := io.ReadFull(file, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, unreadablef("PATH program cannot be read")
	}
	return !bytes.HasPrefix(head[:count], []byte("#!")) && bytes.IndexByte(head[:count], 0) >= 0, nil
}

// ambientPath is the PATH the shell that runs the command starts with: the environment of this process (a hook runs in the
// environment of the session whose command it judges). A variable, so a test can give it.
var ambientPath = func() (string, bool) { return os.LookupEnv("PATH") }

// ambientProgram is the file the PATH the shell starts with runs for a name. A relative or empty directory before the match names
// a place the reader does not fix, so it answers none.
func ambientProgram(name string) (string, bool) {
	value, ok := ambientPath()
	if !ok {
		return "", false
	}
	for _, directory := range strings.Split(value, ":") {
		if !filepath.IsAbs(directory) {
			return "", false
		}
		candidate := strings.TrimSuffix(directory, "/") + "/" + name
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
