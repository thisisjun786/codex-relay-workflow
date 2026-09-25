package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRevision is MANIFEST-CANON-01: sorted byte-wise by path, "<path>:<sha256>" per entry,
// joined with LF, sha256 of the UTF-8 string.
func ManifestRevision(entries []ManifestEntry) (string, error) {
	ordered := append([]ManifestEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	lines := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		declared, err := NormalizeDeclaredPath(entry.Path)
		if err != nil {
			return "", err
		}
		if !lowerDigest.MatchString(entry.SHA256) {
			return "", refuse(ReasonManifestUnverified, "entry %q has a digest that is not 64 lowercase hex characters: %q", declared, entry.SHA256)
		}
		lines = append(lines, declared+":"+entry.SHA256)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
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

type frozenManifest struct {
	Serialization string          `json:"serialization"`
	RevisionHash  string          `json:"revisionHash"`
	Entries       []ManifestEntry `json:"entries"`
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
			return refuse(ReasonManifestUnverified, "refusing to store bytes under a non-digest name %q", entry.SHA256)
		}
		blob := filepath.Join(files, entry.SHA256)
		if _, err := os.Stat(blob); errors.Is(err, os.ErrNotExist) {
			if err := copyAuthorized(entry.Path, blob); err != nil {
				return err
			}
		}
		copied, size, _, err := HashArtifact(blob, []string{files}, false)
		if err != nil {
			return err
		}
		if copied != entry.SHA256 {
			return refuse(ReasonManifestUnverified, "frozen copy of %q hashes to %s, not %s", entry.Path, copied, entry.SHA256)
		}
		if entry.Bytes != nil && *entry.Bytes != size {
			return refuse(ReasonManifestUnverified, "frozen copy of %q is %d bytes, not %d", entry.Path, size, *entry.Bytes)
		}
	}
	revision, err := ManifestRevision(entries)
	if err != nil {
		return err
	}
	document, err := json.Marshal(frozenManifest{Serialization: "MANIFEST-CANON-01", RevisionHash: revision, Entries: entries})
	if err != nil {
		return fmt.Errorf("frozen manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "MANIFEST.json"), document, 0o600); err != nil {
		return fmt.Errorf("frozen manifest: %w", err)
	}
	return nil
}

func copyAuthorized(source, blob string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return refuse(ReasonManifestUnverified, "cannot freeze %q: %v", source, err)
	}
	if err := os.WriteFile(blob, data, 0o600); err != nil {
		return fmt.Errorf("frozen blob: %w", err)
	}
	return nil
}

// VerifyFrozen checks entries against a frozen copy instead of files that may have moved on,
// including each claimed byte count (manifest.verify_frozen).
func VerifyFrozen(reference string, entries []ManifestEntry) []string {
	data, err := os.ReadFile(filepath.Join(reference, "MANIFEST.json"))
	if err != nil {
		return []string{reference + ": no MANIFEST.json in the frozen copy"}
	}
	var frozen frozenManifest
	if err := json.Unmarshal(data, &frozen); err != nil {
		return []string{reference + ": the frozen manifest is not readable JSON"}
	}
	var problems []string
	claimed, stored := map[[2]string]bool{}, map[[2]string]bool{}
	sizes := map[string]*int64{}
	for _, entry := range entries {
		claimed[[2]string{entry.Path, entry.SHA256}] = true
	}
	for _, entry := range frozen.Entries {
		stored[[2]string{entry.Path, entry.SHA256}] = true
		sizes[entry.Path] = entry.Bytes
	}
	if len(claimed) != len(stored) || !containsAll(claimed, stored) {
		problems = append(problems, reference+": the frozen manifest does not describe the same deliverables")
	}
	for _, entry := range entries {
		if recorded := sizes[entry.Path]; entry.Bytes != nil && recorded != nil && *recorded != *entry.Bytes {
			problems = append(problems, fmt.Sprintf("%s: caller claims %d bytes but the frozen copy records %d", entry.Path, *entry.Bytes, *recorded))
		}
	}
	files := filepath.Join(reference, "files")
	for _, entry := range frozen.Entries {
		if !lowerDigest.MatchString(entry.SHA256) {
			problems = append(problems, fmt.Sprintf("%s: %q is not a digest", entry.Path, entry.SHA256))
			continue
		}
		resolved, err := filepath.EvalSymlinks(files)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: frozen bytes unreadable for %s: %v", entry.Path, entry.SHA256, err))
			continue
		}
		digest, size, _, err := HashArtifact(filepath.Join(resolved, entry.SHA256), []string{resolved}, false)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: frozen bytes unreadable for %s: %v", entry.Path, entry.SHA256, err))
		case digest != entry.SHA256:
			problems = append(problems, fmt.Sprintf("%s: frozen bytes do not match %s", entry.Path, entry.SHA256))
		case entry.Bytes != nil && *entry.Bytes != size:
			problems = append(problems, fmt.Sprintf("%s: frozen bytes are %d, not the claimed %d", entry.Path, size, *entry.Bytes))
		}
	}
	return problems
}

func containsAll(a, b map[[2]string]bool) bool {
	for key := range b {
		if !a[key] {
			return false
		}
	}
	return true
}
