//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Helpers the CI checks share: the git runner, Python's error texts and the argparse-shaped flag
// parser (they sat in scope.go until the selection check left in wave R1).

// isPythonInt reports whether json.loads would read the number as an int, not a float.
func isPythonInt(number json.Number) bool {
	return regexp.MustCompile(`^-?[0-9]+$`).MatchString(string(number))
}

// gitError is subprocess.CalledProcessError's text for a failed git command.
type gitError struct {
	args   []string
	status string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("Command '%s' %s", pyvalue.Repr(append([]string{"git"}, e.args...)), e.status)
}

func runGit(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status := exit.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			return nil, &gitError{args, fmt.Sprintf("died with <Signals.%s: %d>.", signalName(status.Signal()), int(status.Signal()))}
		}
		return nil, &gitError{args, fmt.Sprintf("returned non-zero exit status %d.", exit.ExitCode())}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("[Errno 2] No such file or directory: 'git'")
	}
	return nil, err
}

func signalName(sig syscall.Signal) string {
	names := map[syscall.Signal]string{syscall.SIGKILL: "SIGKILL", syscall.SIGTERM: "SIGTERM",
		syscall.SIGINT: "SIGINT", syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT", syscall.SIGPIPE: "SIGPIPE"}
	if name, ok := names[sig]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", int(sig))
}

var errNoValue = valueError{"ValueError"}

type valueError struct{ text string }

func (e valueError) Error() string        { return e.text }
func (e valueError) Is(target error) bool { return target == errNoValue }

func sortedSet(items []string) []string {
	var out []string
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// pyOSError is f"{type(error).__name__}: {error}" for an OSError.
func pyOSError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	class := map[syscall.Errno]string{syscall.ENOENT: "FileNotFoundError", syscall.EACCES: "PermissionError",
		syscall.EPERM: "PermissionError", syscall.EISDIR: "IsADirectoryError", syscall.ENOTDIR: "NotADirectoryError",
		syscall.EAGAIN: "BlockingIOError", syscall.EINTR: "InterruptedError"}[errno]
	if class == "" {
		class = "OSError"
	}
	return class + ": " + pyOSErrorText(err)
}

// pyOSErrorText is str(OSError) for a path error: "[Errno N] Text: 'path'".
func pyOSErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := errno.Error()
	text = fmt.Sprintf("[Errno %d] %s%s", int(errno), strings.ToUpper(text[:1]), text[1:])
	var path *os.PathError
	if errors.As(err, &path) {
		text += ": " + pyvalue.StrRepr(path.Path)
	}
	return text
}

// parseFlags reads argparse-style string options ("--name value" or "--name=value", unique
// prefixes allowed, default ""). code is -1 to continue, else the exit status to return.
func parseFlags(prog, description string, names []string, args []string, stdout, stderr io.Writer) (map[string]string, int) {
	values, _, code := parseOptions(prog, description, names, nil, args, stdout, stderr)
	return values, code
}

// parseOptions is parseFlags plus store_true switches; present names every option given.
func parseOptions(prog, description string, names, switches []string, args []string, stdout, stderr io.Writer) (map[string]string, map[string]bool, int) {
	usage := "usage: crw-dev ci " + prog + " [-h]"
	for _, name := range names {
		usage += fmt.Sprintf(" [--%s %s]", name, strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	}
	for _, name := range switches {
		usage += " [--" + name + "]"
	}
	fail := func(message string) (map[string]string, map[string]bool, int) {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "crw-dev ci %s: error: %s\n", prog, message)
		return nil, nil, 2
	}
	values := map[string]string{}
	present := map[string]bool{}
	for _, name := range names {
		values[name] = ""
	}
	all := append(slices.Clone(names), switches...)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-h" || arg == "--help" {
			fmt.Fprintf(stdout, "%s\n\n%s\n", usage, description)
			return nil, nil, 0
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return fail("unrecognized arguments: " + strings.Join(args[i:], " "))
		}
		key, value, inline := strings.Cut(arg[2:], "=")
		var match []string
		for _, name := range all {
			if name == key {
				match = []string{name}
				break
			}
			if strings.HasPrefix(name, key) {
				match = append(match, name)
			}
		}
		switch {
		case len(match) == 0:
			return fail("unrecognized arguments: " + arg)
		case len(match) > 1:
			return fail(fmt.Sprintf("ambiguous option: --%s could match --%s", key, strings.Join(match, ", --")))
		}
		present[match[0]] = true
		if slices.Contains(switches, match[0]) {
			if inline {
				return fail(fmt.Sprintf("argument --%s: ignored explicit argument %s", match[0], pyvalue.StrRepr(value)))
			}
			continue
		}
		if !inline {
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return fail(fmt.Sprintf("argument --%s: expected one argument", match[0]))
			}
			i++
			value = args[i]
		}
		values[match[0]] = value
	}
	return values, present, -1
}

// nonNil is items, or an empty list for none, so a JSON encoding says [] rather than null.
func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}
