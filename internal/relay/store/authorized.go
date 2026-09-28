package store

import (
	"errors"
	"os"
	"syscall"
)

// IsWithin exposes scope.py's component containment to the adapter without another
// implementation. Existing callers retain their internal entry point.
func IsWithin(root, candidate string) bool { return isWithin(root, candidate) }
func (b ArtifactBinding) Record() map[string]any {
	return map[string]any{"pathBindingMode": string(b.Mode), "leaseDetail": b.Detail}
}

// AuthorizedFile adds the descriptor lifetime API used by manifest.freeze and
// adapter callers. Traversal, snapshots, hashes and lease checks are the existing
// store implementation, shared with receipt intake.
type AuthorizedFile struct {
	File     *os.File
	Binding  ArtifactBinding
	snapshot statSnapshot
}

func OpenAuthorized(declared string, roots []string, allowLease bool) (*AuthorizedFile, error) {
	declared, err := NormalizeDeclaredPath(declared)
	if err != nil {
		return nil, err
	}
	root, err := assertWithin(declared, roots)
	if err != nil {
		return nil, err
	}
	fd, err := pinnedOpen(declared)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), declared)
	fail := func(err error) (*AuthorizedFile, error) { return nil, errors.Join(err, file.Close()) }
	actual, err := descriptorPath(fd)
	if err != nil {
		return fail(err)
	}
	if actual != declared {
		return fail(refuse(ReasonPathRelocated, "descriptor for %s actually resolves to %s", PythonRepr(declared), PythonRepr(actual)))
	}
	snapshot, mode, err := snapshotOf(fd)
	if err != nil {
		return fail(err)
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return fail(refuse(ReasonNotARegularFile, "%s is not a regular file", PythonRepr(declared)))
	}
	binding := ArtifactBinding{Mode: BestEffortDetection, Detail: "lease not attempted", Declared: declared, Root: root}
	if allowLease {
		held, detail := acquireReadLease(fd)
		binding.Detail = detail
		if held {
			binding.Mode = LeaseEnforced
		}
	}
	return &AuthorizedFile{File: file, Binding: binding, snapshot: snapshot}, nil
}
func (h *AuthorizedFile) VerifyStable() error {
	return verifyStable(int(h.File.Fd()), h.Binding.Declared, h.snapshot, h.Binding.Mode)
}
func (h *AuthorizedFile) Close() error {
	if h.File == nil {
		return nil
	}
	if h.Binding.Mode == LeaseEnforced {
		releaseLease(int(h.File.Fd()))
	}
	err := h.File.Close()
	h.File = nil
	return err
}
func (h *AuthorizedFile) Rebaseline() error {
	snapshot, _, err := snapshotOf(int(h.File.Fd()))
	if err == nil {
		h.snapshot = snapshot
	}
	return err
}
func HashAuthorized(h *AuthorizedFile, between func() error) (string, int64, error) {
	first, size, err := hashDescriptor(int(h.File.Fd()))
	if err != nil {
		return "", 0, err
	}
	if between != nil {
		if err := between(); err != nil {
			return "", 0, err
		}
	}
	second, again, err := hashDescriptor(int(h.File.Fd()))
	if err != nil {
		return "", 0, err
	}
	if first != second || size != again {
		return "", 0, refuse(ReasonArtifactMutated, "%s produced different bytes on two consecutive reads", PythonRepr(h.Binding.Declared))
	}
	if err := h.VerifyStable(); err != nil {
		return "", 0, err
	}
	return first, size, nil
}
