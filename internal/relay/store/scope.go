package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
	"syscall"
)

// NormalizeDeclaredPath is MANIFEST-CANON-01: absolute, normalized POSIX, no '~', no trailing
// slash. Symlinks are deliberately not resolved: the declared string enters the digest.
func NormalizeDeclaredPath(declared string) (string, error) {
	switch {
	case declared == "":
		return "", refuse(ReasonScopeEscape, "path must be a non-empty string")
	case strings.ContainsRune(declared, 0):
		return "", refuse(ReasonScopeEscape, "path must not contain NUL")
	case !strings.HasPrefix(declared, "/"):
		return "", refuse(ReasonScopeEscape, "path must be absolute: %q", declared)
	case strings.Contains(declared, "~"):
		return "", refuse(ReasonScopeEscape, "path must not contain '~': %q", declared)
	case declared != pythonNormpath(declared):
		return "", refuse(ReasonScopeEscape, "path must already be normalized; %q normalizes to %q", declared, pythonNormpath(declared))
	}
	return declared, nil
}

// pythonNormpath is posixpath.normpath, which keeps a leading "//" that path.Clean drops.
func pythonNormpath(p string) string {
	cleaned := path.Clean(p)
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "/" + cleaned
	}
	return cleaned
}

// isWithin is containment by path component, so /a/b does not contain /a/bc.
func isWithin(root, candidate string) bool {
	root = pythonNormpath(root)
	candidate = pythonNormpath(candidate)
	if root == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func assertWithin(candidate string, roots []string) (string, error) {
	for _, root := range roots {
		if isWithin(root, candidate) {
			return root, nil
		}
	}
	return "", refuse(ReasonScopeEscape, "%q lies outside every authorized root %q", candidate, roots)
}

// ArtifactBinding records how strongly one artifact read was bound to its declared path.
type ArtifactBinding struct {
	Mode   PathBinding
	Detail string
}

// betweenPasses is manifest.hash_authorized's between_passes seam: tests mutate, move or
// re-baseline the artifact in the window between the two hashes. Nil in production.
var betweenPasses func(fd int, before *statSnapshot)

// HashArtifact reads one authorized artifact through a pinned descriptor, hashing it twice and
// re-checking the binding afterwards (manifest.hash_path). The lease is requested only when
// allowLease is set, because holding one stalls unrelated writers for the lease-break timeout.
func HashArtifact(declared string, roots []string, allowLease bool) (string, int64, ArtifactBinding, error) {
	declared, err := NormalizeDeclaredPath(declared)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	root, err := assertWithin(declared, roots)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	fd, err := pinnedOpen(declared)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	defer func() { _ = syscall.Close(fd) }()
	actual, err := descriptorPath(fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	if actual != declared {
		return "", 0, ArtifactBinding{}, refuse(ReasonPathRelocated, "descriptor for %q actually resolves to %q", declared, actual)
	}
	if _, err := assertWithin(actual, []string{root}); err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	before, mode, err := snapshotOf(fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, &RefusedError{Reason: ReasonScopeEscape, Detail: declared, cause: err}
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return "", 0, ArtifactBinding{}, refuse(ReasonNotARegularFile, "%q is not a regular file", declared)
	}
	binding := ArtifactBinding{Mode: BestEffortDetection, Detail: "lease not attempted"}
	if allowLease {
		held, detail := acquireReadLease(fd)
		binding.Detail = detail
		if held {
			binding.Mode = LeaseEnforced
			defer releaseLease(fd)
		}
	}
	first, size, err := hashDescriptor(fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	if betweenPasses != nil {
		betweenPasses(fd, &before)
	}
	second, sizeAgain, err := hashDescriptor(fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	if first != second || size != sizeAgain {
		return "", 0, ArtifactBinding{}, refuse(ReasonArtifactMutated, "%q produced different bytes on two consecutive reads", declared)
	}
	if err := verifyStable(fd, declared, before, binding.Mode); err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	return first, size, binding, nil
}

func verifyStable(fd int, declared string, before statSnapshot, mode PathBinding) error {
	actual, err := descriptorPath(fd)
	if err != nil {
		return err
	}
	if actual != declared {
		return refuse(ReasonPathRelocated, "%q moved to %q during the read", declared, actual)
	}
	after, _, err := snapshotOf(fd)
	if err != nil || after != before {
		return refuse(ReasonArtifactMutated, "%q changed size or timestamps during the read", declared)
	}
	if mode == LeaseEnforced && !leaseStillHeld(fd) {
		return refuse(ReasonArtifactLeaseBroken, "the read lease on %q was broken during the read", declared)
	}
	return nil
}

func hashDescriptor(fd int) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, io.NewSectionReader(descriptorReader(fd), 0, 1<<62))
	if err != nil {
		return "", 0, fmt.Errorf("read artifact: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

type descriptorReader int

func (d descriptorReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := syscall.Pread(int(d), p, offset)
	if err != nil {
		return 0, err
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}
