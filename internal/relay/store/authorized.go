package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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

// OpenAuthorized opens a declared artifact for a caller that pins and hashes it itself (the manifest
// freeze and the dag copies). A relay store file this process holds or has open is refused, before
// the open where its name already shows it and again on the opened descriptor, and no store
// descriptor is closed here (CRW-967, I-563).
func OpenAuthorized(declared string, roots []string, allowLease bool) (*AuthorizedFile, error) {
	declared, err := NormalizeDeclaredPath(declared)
	if err != nil {
		return nil, err
	}
	root, err := assertWithin(declared, roots)
	if err != nil {
		return nil, err
	}
	if refused := refuseKnownStoreFile(declared); refused != nil {
		return nil, refused
	}
	fd, err := pinnedOpen(declared)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), declared)
	fail := func(err error) (*AuthorizedFile, error) { return nil, errors.Join(err, closeOrKeepStoreFile(file)) }
	actual, err := descriptorPath(fd)
	if err != nil {
		return fail(err)
	}
	if actual != declared {
		return fail(refuse(ReasonPathRelocated, "descriptor for %s actually resolves to %s", pyvalue.StrRepr(declared), pyvalue.StrRepr(actual)))
	}
	snapshot, mode, err := snapshotOf(fd)
	if err != nil {
		return fail(err)
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return fail(refuse(ReasonNotARegularFile, "%s is not a regular file", pyvalue.StrRepr(declared)))
	}
	if holdsStoreFileIdentity(uint64(snapshot.dev), uint64(snapshot.ino)) {
		return fail(refuseStoreFile(declared))
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

// refuseStoreFile is the refusal of a relay store file this process holds or has open. It is the
// refusal the artifact reader has always given for one (ReasonScopeEscape), so a caller reads one
// meaning for both paths.
func refuseStoreFile(path string) error {
	return refuse(ReasonScopeEscape, "%s is a relay store file this process holds or opened, so it is refused rather than read and closed", pyvalue.StrRepr(path))
}

// refuseKnownStoreFile is refuseStoreFile for a path whose name already resolves to a store file this
// process holds or has open. It stats the path and never opens it, so a refusal produces no
// descriptor. nil when the path is not known as one.
func refuseKnownStoreFile(path string) error {
	if key, known := knownStoreFileIdentityAt(path); known && holdsStoreFileIdentity(key.device, key.inode) {
		return refuseStoreFile(path)
	}
	return nil
}

// refuseKnownStoreTarget is refuseKnownStoreFile for a path that the open follows through symbolic
// links: it stats the path, so a link to an open store is refused before anything is opened and a
// repeated read of that link opens no descriptor (CRW-967). nil when the target is not known as a
// store file or is not a regular file.
func refuseKnownStoreTarget(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if holdsStoreFileIdentity(uint64(stat.Dev), uint64(stat.Ino)) {
		return refuseStoreFile(path)
	}
	return nil
}

// ReadIndependentFile reads the regular file at path, at most limit bytes, for an input that names an
// artifact rather than holding it (delivery's --independent-review). It follows a symbolic link as
// os.Open does, but a relay store file this process holds or has open is refused, before the open
// where the name shows it and again on the opened descriptor, and the descriptor is closed only
// through the registry (CRW-967, I-563). A path that is not a regular file is refused after a
// non-blocking open, so a FIFO cannot block the reader.
func ReadIndependentFile(path string, limit int64) ([]byte, error) {
	return readIndependentFile(path, limit, nil)
}

// readIndependentFile is ReadIndependentFile with a seam: between runs with the descriptor open, after
// the identity check and before the read, so a test can make a store open the file in that gap.
func readIndependentFile(path string, limit int64, between func(file *os.File)) ([]byte, error) {
	if refused := refuseKnownStoreTarget(path); refused != nil {
		return nil, refused
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	// The flag is cleared before the descriptor is wrapped, so reads on it block as usual.
	clearStoreFileNonblock(fd)
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, closeOrKeepStoreFile(file))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%s is not a regular file", pyvalue.StrRepr(path)), closeOrKeepStoreFile(file))
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && holdsStoreFileIdentity(uint64(stat.Dev), uint64(stat.Ino)) {
		return nil, errors.Join(refuseStoreFile(path), closeOrKeepStoreFile(file))
	}
	if between != nil {
		between(file)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	return raw, errors.Join(err, closeOrKeepStoreFile(file))
}
func (h *AuthorizedFile) VerifyStable() error {
	return verifyStable(int(h.File.Fd()), h.Binding.Declared, h.snapshot, h.Binding.Mode)
}

// Close releases the artifact's descriptor. A descriptor that became a store file's while it was held
// is kept reachable instead of closed (CRW-967, I-563).
func (h *AuthorizedFile) Close() error {
	if h.File == nil {
		return nil
	}
	if h.Binding.Mode == LeaseEnforced {
		releaseLease(int(h.File.Fd()))
	}
	err := closeOrKeepStoreFile(h.File)
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
	// An authorized read is bracketed by its caller's own checks and finishes whatever the context says.
	first, size, err := hashDescriptor(context.Background(), int(h.File.Fd()))
	if err != nil {
		return "", 0, err
	}
	if between != nil {
		if err := between(); err != nil {
			return "", 0, err
		}
	}
	second, again, err := hashDescriptor(context.Background(), int(h.File.Fd()))
	if err != nil {
		return "", 0, err
	}
	if first != second || size != again {
		return "", 0, refuse(ReasonArtifactMutated, "%s produced different bytes on two consecutive reads", pyvalue.StrRepr(h.Binding.Declared))
	}
	if err := h.VerifyStable(); err != nil {
		return "", 0, err
	}
	return first, size, nil
}
