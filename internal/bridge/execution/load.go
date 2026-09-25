package execution

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// FromFile is ExecutionPolicy.from_file: opened without blocking and judged on the descriptor
// that is read, so a FIFO is refused instead of hanging startup.
func FromFile(path string) (Policy, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Policy{}, &PolicyError{fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Policy{}, &PolicyError{fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	if !info.Mode().IsRegular() {
		return Policy{}, &PolicyError{fmt.Sprintf("cannot read %s: it is not a regular file", path)}
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return Policy{}, &PolicyError{fmt.Sprintf("cannot read %s: %v", path, err)}
	}
	return FromBytes(raw, path)
}

// FromEnvironment is ExecutionPolicy.from_environment; the zero Policy is PRESENCE_ONLY.
func FromEnvironment(env map[string]string) (Policy, error) {
	configured := strings.TrimSpace(env[EnvPolicy])
	expected := strings.TrimSpace(env[EnvDigest])
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
