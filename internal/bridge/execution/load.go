package execution

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/pyerr"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// FromFile is ExecutionPolicy.from_file: ReadFile, then FromBytes over what was read.
func FromFile(path string) (Policy, error) {
	raw, err := ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	return FromBytes(raw, path)
}

// ReadFile is from_file's reading half: opened without blocking and judged on the descriptor
// that is read, so a FIFO is refused instead of hanging startup. Every failure is the
// PolicyError from_file raises for it, os.open's ValueError for an embedded NUL included.
func ReadFile(path string) ([]byte, error) {
	unreadable := func(err error) error {
		// str(OSError) as Python raises it: "[Errno 2] No such file or directory: '<path>'".
		if _, message, ok := pyerr.OSError(err); ok {
			return &PolicyError{fmt.Sprintf("cannot read %s: %s", path, message)}
		}
		return &PolicyError{fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	if strings.IndexByte(path, 0) >= 0 {
		// CPython's path converter refuses it before any system call.
		return nil, &PolicyError{fmt.Sprintf("cannot read %s: open: embedded null character in path", path)}
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, unreadable(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, unreadable(err)
	}
	if !info.Mode().IsRegular() {
		return nil, &PolicyError{fmt.Sprintf("cannot read %s: it is not a regular file", path)}
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, unreadable(err)
	}
	return raw, nil
}

// FromEnvironment is ExecutionPolicy.from_environment; the zero Policy is PRESENCE_ONLY.
func FromEnvironment(env map[string]string) (Policy, error) {
	configured := pyvalue.Strip(env[EnvPolicy])
	expected := pyvalue.Strip(env[EnvDigest])
	if configured == "" {
		if expected != "" {
			return Policy{}, &PolicyError{fmt.Sprintf("%s expects a policy with digest %s, and %s names no file", EnvDigest, expected, EnvPolicy)}
		}
		return Policy{}, nil
	}
	policy, err := FromFile(configured)
	if err != nil {
		return Policy{}, err
	}
	if expected != "" && policy.digest != expected {
		return Policy{}, &PolicyError{fmt.Sprintf("%s hashes to %s, and this process was started expecting %s; it changed after it was registered, so it is refused rather than enforced unregistered", configured, policy.digest, expected)}
	}
	return policy, nil
}
