package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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
		return "", refuse(ReasonScopeEscape, "path must be absolute: %s", pyvalue.StrRepr(declared))
	case strings.Contains(declared, "~"):
		return "", refuse(ReasonScopeEscape, "path must not contain '~': %s", pyvalue.StrRepr(declared))
	case declared != Normpath(declared):
		return "", refuse(ReasonScopeEscape, "path must already be normalized; %s normalizes to %s", pyvalue.StrRepr(declared), pyvalue.StrRepr(Normpath(declared)))
	}
	return declared, nil
}

// isWithin is containment by path component, so /a/b does not contain /a/bc.
func isWithin(root, candidate string) bool {
	root = Normpath(root)
	candidate = Normpath(candidate)
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
	quoted := make([]string, len(roots))
	for i, root := range roots {
		quoted[i] = pyvalue.StrRepr(root)
	}
	return "", refuse(ReasonScopeEscape, "%s lies outside every authorized root [%s]", pyvalue.StrRepr(candidate), strings.Join(quoted, ", "))
}

// ArtifactBinding records how strongly one artifact read was bound to its declared path.
type ArtifactBinding struct {
	Mode           PathBinding
	Detail         string
	Declared, Root string
}

// betweenPasses is manifest.hash_authorized's between_passes seam: tests mutate, move or
// re-baseline the artifact in the window between the two hashes. Nil in production.
var betweenPasses func(fd int, before *statSnapshot)

// HashArtifact reads one authorized artifact through a pinned descriptor, hashing it twice and
// re-checking the binding afterwards (manifest.hash_path). The lease is requested only when
// allowLease is set, because holding one stalls unrelated writers for the lease-break timeout.
func HashArtifact(declared string, roots []string, allowLease bool) (string, int64, ArtifactBinding, error) {
	return HashArtifactContext(context.Background(), declared, roots, allowLease)
}

// HashArtifactContext uses the caller's deadline for both hashing passes.
func HashArtifactContext(ctx context.Context, declared string, roots []string, allowLease bool) (string, int64, ArtifactBinding, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, ArtifactBinding{}, err
	}
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
		return "", 0, ArtifactBinding{}, refuse(ReasonPathRelocated, "descriptor for %s actually resolves to %s", pyvalue.StrRepr(declared), pyvalue.StrRepr(actual))
	}
	if _, err := assertWithin(actual, []string{root}); err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	before, mode, err := snapshotOf(fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, &RefusedError{Reason: ReasonScopeEscape, Detail: declared, cause: err}
	}
	if mode&syscall.S_IFMT != syscall.S_IFREG {
		return "", 0, ArtifactBinding{}, refuse(ReasonNotARegularFile, "%s is not a regular file", pyvalue.StrRepr(declared))
	}
	binding := ArtifactBinding{Mode: BestEffortDetection, Detail: "lease not attempted", Declared: declared, Root: root}
	if allowLease {
		held, detail := acquireReadLease(fd)
		binding.Detail = detail
		if held {
			binding.Mode = LeaseEnforced
			defer releaseLease(fd)
		}
	}
	countArtifactRead(ctx)
	first, size, err := hashDescriptorContext(ctx, fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	if betweenPasses != nil {
		betweenPasses(fd, &before)
	}
	second, sizeAgain, err := hashDescriptorContext(ctx, fd)
	if err != nil {
		return "", 0, ArtifactBinding{}, err
	}
	if first != second || size != sizeAgain {
		return "", 0, ArtifactBinding{}, refuse(ReasonArtifactMutated, "%s produced different bytes on two consecutive reads", pyvalue.StrRepr(declared))
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
		return refuse(ReasonPathRelocated, "%s moved to %s during the read", pyvalue.StrRepr(declared), pyvalue.StrRepr(actual))
	}
	after, _, err := snapshotOf(fd)
	if err != nil || after != before {
		return refuse(ReasonArtifactMutated, "%s changed size or timestamps during the read", pyvalue.StrRepr(declared))
	}
	if mode == LeaseEnforced && !leaseStillHeld(fd) {
		return refuse(ReasonArtifactLeaseBroken, "the read lease on %s was broken during the read", pyvalue.StrRepr(declared))
	}
	return nil
}

// hashDescriptor is the uncancellable form used by authorized reads.
func hashDescriptor(fd int) (string, int64, error) {
	return hashDescriptorContext(context.Background(), fd)
}

func hashDescriptorContext(ctx context.Context, fd int) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, io.NewSectionReader(contextDescriptorReader{ctx, descriptorReader(fd)}, 0, 1<<62))
	if err != nil {
		return "", 0, fmt.Errorf("read artifact: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

type contextDescriptorReader struct {
	ctx context.Context
	fd  descriptorReader
}

func (r contextDescriptorReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.fd.ReadAt(p, offset)
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
