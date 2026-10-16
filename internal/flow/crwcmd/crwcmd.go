// Package crwcmd is the crw command an owner line starts with (CRW-1572): the then of a session run's step, the hints of
// `crw flow next`, the next and loop of `crw flow start`, the details of `crw flow dispatched` and the owner's wake notice. The
// owner runs these lines as printed, in a shell whose PATH need not hold crw (the installer puts crw under its runtime pointer, not
// on PATH), so they name the crw that runs rather than the bare `crw`.
package crwcmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// deletedSuffix is what Linux adds to /proc/self/exe when the running binary was removed or replaced after it started.
const deletedSuffix = " (deleted)"

var (
	mu sync.Mutex
	// override is the word a test names (Override); nil: the order of Word.
	override *string
	// executable is this process's binary (os.Executable).
	executable = os.Executable
	// runtimePointer is the installer's runtime pointer as a shell word (host.Invocation, BinEnv having been read already).
	runtimePointer = func() (string, error) { return host.Invocation(os.LookupEnv) }
)

// Word is the crw command an emitted owner line starts with, one or more shell words:
//  1. CRW_BIN (host.BinEnv), trimmed, when it is not blank: as written, as host.Invocation uses it (an operator's override wins);
//  2. else this process's binary (os.Executable, a Linux " (deleted)" suffix stripped) when it is an absolute path of a regular
//     file, as one literal shell word (host.ShellWord: single-quoted, backtick-free, so it sits in a Markdown code span). The
//     running binary is runnable and is the build whose journal contract the run uses;
//  3. else the installer's runtime pointer (host.Invocation);
//  4. else the bare `crw`, which runs only where crw is on PATH; Warning says why.
//
// Nothing is cached: every call reads the environment and the binary again.
func Word() string {
	word, _ := resolve()
	return word
}

// Warning is why Word is the bare `crw` (no CRW_BIN, no path of the running binary, no runtime pointer), or "" when Word names a
// crw of its own.
func Warning() string {
	_, why := resolve()
	return why
}

func resolve() (string, string) {
	mu.Lock()
	named, exe, pointer := override, executable, runtimePointer
	mu.Unlock()
	if named != nil {
		return *named, ""
	}
	if value, _ := os.LookupEnv(host.BinEnv); text.Trim(value) != "" {
		return text.Trim(value), ""
	}
	path, exeErr := running(exe)
	if exeErr == nil {
		return host.ShellWord(path), ""
	}
	word, err := pointer()
	if err == nil && strings.TrimSpace(word) != "" {
		return word, ""
	}
	if err == nil {
		err = errors.New("the runtime pointer is empty")
	}
	return "crw", "the owner lines name the bare crw, which runs only where crw is on PATH: " + host.BinEnv + " is not set, the running crw's path is not known (" +
		exeErr.Error() + ") and " + err.Error()
}

// running is the path of this process's binary when it names a regular file.
func running(exe func() (string, error)) (string, error) {
	path, err := exe()
	if err != nil {
		return "", err
	}
	path = strings.TrimSuffix(path, deletedSuffix)
	if !filepath.IsAbs(path) {
		return "", errors.New("the executable path " + path + " is not absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New(path + " is not a regular file")
	}
	return path, nil
}

// Override makes Word answer word, and Warning nothing, until restore is called; "" removes an override (Word's order applies). It is
// a seam for tests: a test binary is never the crw an owner line names.
func Override(word string) (restore func()) {
	var next *string
	if word != "" {
		next = &word
	}
	mu.Lock()
	previous := override
	override = next
	mu.Unlock()
	return func() {
		mu.Lock()
		override = previous
		mu.Unlock()
	}
}

// UseExecutable makes path this process's binary for Word until restore is called; "" is a binary whose path is not known. It is a
// seam for tests.
func UseExecutable(path string) (restore func()) {
	exe := func() (string, error) { return path, nil }
	if path == "" {
		exe = func() (string, error) { return "", errors.New("os.Executable gives no path") }
	}
	mu.Lock()
	previous := executable
	executable = exe
	mu.Unlock()
	return func() {
		mu.Lock()
		executable = previous
		mu.Unlock()
	}
}

// UseRuntimePointer makes word and err the runtime pointer's answer for Word until restore is called. It is a seam for tests (a home
// with no absolute path cannot be made in-process: host.Invocation falls back to the account's home).
func UseRuntimePointer(word string, err error) (restore func()) {
	mu.Lock()
	previous := runtimePointer
	runtimePointer = func() (string, error) { return word, err }
	mu.Unlock()
	return func() {
		mu.Lock()
		runtimePointer = previous
		mu.Unlock()
	}
}
