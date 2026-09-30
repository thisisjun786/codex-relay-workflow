package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRevision is MANIFEST-CANON-01: sorted byte-wise by path, "<path>:<sha256>" per entry,
// joined with LF, sha256 of the UTF-8 string.
func ManifestRevision(entries []ManifestEntry) (string, error) {
	payload, err := CanonicalPayload(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalPayload(entries []ManifestEntry) (string, error) {
	ordered := append([]ManifestEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	lines := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		declared, err := NormalizeDeclaredPath(entry.Path)
		if err != nil {
			return "", err
		}
		if !lowerDigest.MatchString(entry.SHA256) {
			return "", refuse(ReasonManifestUnverified, "entry %s has a digest that is not 64 lowercase hex characters: %s", PythonRepr(declared), PythonRepr(entry.SHA256))
		}
		lines = append(lines, declared+":"+entry.SHA256)
	}
	return strings.Join(lines, "\n"), nil
}

// BuildManifest reads each authorized artifact into a manifest entry (manifest.build).
func BuildManifest(paths, roots []string) ([]ManifestEntry, error) {
	entries := make([]ManifestEntry, 0, len(paths))
	for _, declared := range paths {
		digest, size, _, err := HashArtifact(declared, roots, false)
		if err != nil {
			return nil, err
		}
		entries = append(entries, ManifestEntry{Path: declared, SHA256: digest, Bytes: &size})
	}
	return entries, nil
}

// verifyAgainstDisk re-hashes every declared path and reports what disagrees, with the
// weakest binding achieved over the whole manifest.
func verifyAgainstDisk(entries []ManifestEntry, roots []string, allowLease bool) ([]string, PathBinding) {
	var problems []string
	weakest := LeaseEnforced
	for _, entry := range entries {
		digest, size, binding, err := HashArtifact(entry.Path, roots, allowLease)
		if err != nil {
			problems = append(problems, entry.Path+": "+err.Error())
			continue
		}
		if binding.Mode != LeaseEnforced {
			weakest = binding.Mode
		}
		switch {
		case digest != entry.SHA256:
			problems = append(problems, fmt.Sprintf("%s: bytes hash to %s but the manifest claims %s", entry.Path, digest, entry.SHA256))
		case entry.Bytes != nil && *entry.Bytes != size:
			problems = append(problems, fmt.Sprintf("%s: size %d but the manifest claims %d", entry.Path, size, *entry.Bytes))
		}
	}
	return problems, weakest
}

// FreezeManifest copies the bytes under their digests while keeping the original declared
// paths, so a later verification reproduces the same revision (manifest.freeze).
func FreezeManifest(entries []ManifestEntry, destination string) error {
	files := filepath.Join(destination, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return fmt.Errorf("frozen copy: %w", err)
	}
	for _, entry := range entries {
		// The digest names a file, so it is validated before it is ever joined to a path.
		if !lowerDigest.MatchString(entry.SHA256) {
			return refuse(ReasonManifestUnverified, "refusing to store bytes under a non-digest name %s", PythonRepr(entry.SHA256))
		}
		blob := filepath.Join(files, entry.SHA256)
		if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
			if err := copyAuthorized(entry.Path, blob); err != nil {
				return err
			}
		}
		// Never publish a manifest over bytes that were not re-read and confirmed, and re-read them
		// where _read_frozen_blob does: the blob and the files directory as Path.resolve() names
		// them, so a destination reached through a symlink or named relative to the working
		// directory is read from the place it was written to.
		copied, size, err := ReadFrozenBlob(context.Background(), files, entry.SHA256)
		if err != nil {
			return err
		}
		if copied != entry.SHA256 {
			return refuse(ReasonManifestUnverified, "frozen copy of %s hashes to %s, not %s", PythonRepr(entry.Path), copied, entry.SHA256)
		}
		if entry.Bytes != nil && *entry.Bytes != size {
			return refuse(ReasonManifestUnverified, "frozen copy of %s is %d bytes, not %d", PythonRepr(entry.Path), size, *entry.Bytes)
		}
	}
	revision, err := ManifestRevision(entries)
	if err != nil {
		return err
	}
	document, err := frozenDocument(entries, revision)
	if err != nil {
		return fmt.Errorf("frozen manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "MANIFEST.json"), document, 0o600); err != nil {
		return fmt.Errorf("frozen manifest: %w", err)
	}
	return nil
}

// afterFrozenCopy is a test seam between freeze's copy of a source and its stability check,
// where a writer can move the source; production leaves it nil.
var afterFrozenCopy func(source string)

func copyAuthorized(source, blob string) (err error) {
	handle, err := OpenAuthorized(source, []string{"/"}, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	data, err := io.ReadAll(handle.File)
	if err != nil {
		return refuse(ReasonManifestUnverified, "cannot freeze %q: %v", source, err)
	}
	if err := os.WriteFile(blob, data, 0o600); err != nil {
		return fmt.Errorf("frozen blob: %w", err)
	}
	if afterFrozenCopy != nil {
		afterFrozenCopy(source)
	}
	return handle.VerifyStable()
}

// ReadFrozenBlob is _read_frozen_blob: one stored blob hashed through the pinned traversal a
// live artifact would use, with the blob and its files directory resolved as Path.resolve()
// resolves them (resolveFrozenPath), within ctx's deadline.
func ReadFrozenBlob(ctx context.Context, files, digest string) (string, int64, error) {
	blob, err := resolveFrozenPath(filepath.Join(files, digest))
	if err != nil {
		return "", 0, err
	}
	root, err := resolveFrozenPath(files)
	if err != nil {
		return "", 0, err
	}
	hashed, size, _, err := HashArtifactContext(ctx, blob, []string{root}, false)
	return hashed, size, err
}

// VerifyFrozen is manifest.verify_frozen: VerifyFrozenDetailed with the access breakdown dropped,
// so every branch answers as the fence's two-value form does, including the exception it raises
// for a frozen copy that was reached and is not a manifest (FrozenException).
func VerifyFrozen(reference string, entries []ManifestEntry) ([]string, error) {
	_, problems, _, err := VerifyFrozenDetailed(reference, entries)
	return problems, err
}
