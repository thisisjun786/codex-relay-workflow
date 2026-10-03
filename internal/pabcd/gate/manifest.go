package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// criterionIDs is criterionIdsValid: the ids of a non-empty array of distinct c-N strings, else nil (so nil is also "absent").
func criterionIDs(v any) []string {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		id, ok := item.(string)
		if !ok || !isCriterionID(id) || slices.Contains(ids, id) {
			return nil
		}
		ids = append(ids, id)
	}
	return ids
}

// declaredIDs is what a verdict declares: the ids of a desktop artifact (desktopArtifact exactly true), nil when invalid or none.
func declaredIDs(verdict map[string]any) (ids []string, desktop bool) {
	if verdict["desktopArtifact"] != true {
		return nil, false
	}
	return criterionIDs(verdict["criterionIds"]), true
}

// verdictObject parses the bytes of a verdict, which must be a JSON object; notObject is the refusal for one that is not.
func verdictObject(data []byte, notObject string) (map[string]any, error) {
	parsed, err := decodeJSON(data)
	if err != nil {
		return nil, refuse("artifactManifest verdict cannot be parsed: %v", err)
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, refuse("%s", notObject)
	}
	return object, nil
}

// readArtifact is the file half of one manifest entry, in the oracle's order: no link at the file, a real path inside the evidence
// root, a regular file with bytes, and their sha256 as the entry says. It returns the digested bytes, which the later checks parse
// (the oracle reads a verdict again by path, so a file swapped after its digest passed was parsed undigested).
func readArtifact(abs, rootReal, label, want string) ([]byte, error) {
	unreadable := func(err error) error { return refuse("%s cannot be read: %v", label, err) }
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, unreadable(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, refuse("%s is a symlink", label)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, unreadable(err)
	}
	if !within(rootReal, real) {
		return nil, refuse("%s resolves outside the evidence root", label)
	}
	if info, err = os.Stat(abs); err != nil {
		return nil, unreadable(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, refuse("%s is empty or not a regular file", label)
	}
	data, err := readFile(abs, maxText)
	if err != nil {
		return nil, unreadable(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
		return nil, refuse("%s digest does not match", label)
	}
	return data, nil
}

// parseArtifactManifest validates a QA receipt's manifest (paths relative to the receipt's directory): each entry, then each
// identity against the verdict beside it, then each verdict against the entries that name it. The first failure is the refusal.
func parseArtifactManifest(raw any, receiptPath, cwd string) ([]ArtifactDigest, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, refuse("artifactManifest must be a non-empty array")
	}
	root := resolve(cwd, filepath.Join(crwdir.DirName, evidence.Subdir))
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, refuse("artifactManifest evidence root cannot be read: %v", err)
	}
	dir := filepath.Dir(receiptPath)
	var entries []ArtifactDigest
	verdicts, seen := map[string][]byte{}, map[string]bool{} // the digested bytes of each verdict, and the paths met, by absolute path
	for i, item := range items {
		label := fmt.Sprintf("artifactManifest[%d]", i)
		e, ok := item.(map[string]any)
		if !ok {
			return nil, refuse("%s must be an object", label)
		}
		path, _ := e["path"].(string)
		if path == "" || filepath.IsAbs(path) || hasDotDot(path) {
			return nil, refuse("%s.path must be a relative path without ..", label)
		}
		kind, _ := e["kind"].(string)
		if kind != string(ArtifactVerdict) && kind != string(ArtifactIdentity) {
			return nil, refuse("%s.kind is invalid", label)
		}
		if filepath.Base(path) != kind+".json" {
			return nil, refuse("%s.kind does not match path basename", label)
		}
		digest, _ := e["sha256"].(string)
		if !isSHA256(digest) {
			return nil, refuse("%s.sha256 must be a lowercase SHA-256 digest", label)
		}
		entry := ArtifactDigest{Path: path, SHA256: digest, Kind: ArtifactKind(kind)}
		if ids, present := e["criterionIds"]; present {
			if entry.CriterionIDs = criterionIDs(ids); entry.CriterionIDs == nil {
				return nil, refuse("%s.criterionIds must be unique c-N IDs", label)
			}
		}
		abs := resolve(dir, path)
		if !within(root, abs) {
			return nil, refuse("%s escapes the evidence root", label)
		}
		if seen[abs] {
			return nil, refuse("%s duplicates a manifest path", label)
		}
		seen[abs] = true
		data, err := readArtifact(abs, rootReal, label, digest)
		if err != nil {
			return nil, err
		}
		if entry.Kind == ArtifactVerdict {
			verdicts[abs] = data
		}
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		if entry.Kind != ArtifactIdentity {
			continue
		}
		identityAbs := resolve(dir, entry.Path)
		matching := slices.DeleteFunc(slices.Clone(entries), func(item ArtifactDigest) bool {
			return item.Kind != ArtifactVerdict || filepath.Dir(resolve(dir, item.Path)) != filepath.Dir(identityAbs)
		})
		if len(matching) != 1 {
			return nil, refuse("artifactManifest identity %s needs one same-directory verdict", entry.Path)
		}
		verdictEntry := matching[0]
		verdict, err := verdictObject(verdicts[resolve(dir, verdictEntry.Path)], "artifactManifest verdict must be an object")
		if err != nil {
			return nil, err
		}
		refs, _ := verdict["artifactRefs"].([]any)
		if !slices.ContainsFunc(refs, func(ref any) bool {
			name, ok := ref.(string)
			return ok && resolve(filepath.Dir(identityAbs), name) == identityAbs
		}) {
			return nil, refuse("artifactManifest verdict does not reference %s", entry.Path)
		}
		ids, desktop := declaredIDs(verdict)
		if desktop && ids == nil {
			return nil, refuse("artifactManifest desktop verdict has invalid criterionIds")
		}
		if !slices.Equal(entry.CriterionIDs, ids) || !slices.Equal(verdictEntry.CriterionIDs, ids) {
			return nil, refuse("artifactManifest criterionIds do not match verdict %s", verdictEntry.Path)
		}
	}
	for _, entry := range entries {
		if entry.Kind != ArtifactVerdict {
			continue
		}
		verdict, err := verdictObject(verdicts[resolve(dir, entry.Path)], fmt.Sprintf("artifactManifest verdict %s must be an object", entry.Path))
		if err != nil {
			return nil, err
		}
		ids, desktop := declaredIDs(verdict)
		if desktop && ids == nil {
			return nil, refuse("artifactManifest verdict %s has invalid criterionIds", entry.Path)
		}
		if !slices.Equal(entry.CriterionIDs, ids) {
			return nil, refuse("artifactManifest verdict %s criterionIds do not match", entry.Path)
		}
		// The identity is found by path.dirname of the entries' own spellings, not of their resolved paths.
		if desktop && !slices.ContainsFunc(entries, func(item ArtifactDigest) bool {
			return item.Kind == ArtifactIdentity && posixDirname(item.Path) == posixDirname(entry.Path)
		}) {
			return nil, refuse("artifactManifest desktop verdict %s has no identity", entry.Path)
		}
	}
	return entries, nil
}
