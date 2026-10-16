//go:build dev

package ci

// The pass-record cache (CRW-1590): repository-local metadata under the git common directory,
// shared across linked worktrees, keyed by the reuse decision keys. An optimization only: it can
// never turn a failure into a pass.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// localCacheDirectory is the directory the record's reuse keys live under, below the repository's
// common git directory (shared by linked worktrees). Cache-owned components must be plain
// directories and the path must stay inside the repository.
func localCacheDirectory(opts localOptions, current verificationRecord) (string, error) {
	raw, err := runGit(opts.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := filepath.Clean(strings.TrimSpace(string(raw)))
	if !filepath.IsAbs(common) {
		return "", fmt.Errorf("git common directory is not absolute: %q", common)
	}
	key := verificationRecord{Schema: recordSchema, Repository: current.Repository, TreeHash: current.TreeHash,
		CiDigest: current.CiDigest, Tools: current.Tools, GoFlags: current.GoFlags, GoEnv: current.GoEnv,
		HeavyGate: current.HeavyGate, Dependencies: current.Dependencies, OS: current.OS, Arch: current.Arch,
		PlanDigest: current.PlanDigest}
	key.VerifierKind, key.VerifierRules = localVerifierIdentity(current)
	digest, err := recordDigest(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(common, "crw-ci", current.TreeHash, strings.TrimPrefix(digest, "sha256:"))
	if err := homeguard.RefuseUnlessWithin(opts.Root, dir); err != nil {
		return "", err
	}
	// Reject links/non-directories in cache-owned components before reading or writing.
	for p := dir; p != common; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("cache directory is not a plain directory: %s", p)
		}
	}
	return dir, nil
}

// localFindCached finds a validated source record under the directory the current keys name: the
// oldest reading is not trusted — every candidate still faces localTreeReuse, the plan validation
// and the recomputed pin check before it is accepted.
func localFindCached(opts localOptions, current verificationRecord, plan []localJob, stdout io.Writer) *localReuseSource {
	dir, err := localCacheDirectory(opts, current)
	if err != nil {
		fmt.Fprintf(stdout, "local: the pass-record cache is unavailable: %v\n", err)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(stdout, "local: the pass-record cache cannot be read: %v\n", err)
		}
		return nil
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		reused, err := readRecord(path)
		if err != nil {
			continue // corrupt or foreign record: not a candidate
		}
		ok, _ := localTreeReuse(reused, current)
		if ok {
			ok, _ = localValidateReuse(reused, plan)
		}
		if ok {
			ok, _ = localRecomputedReuse(opts, plan, current)
		}
		if ok {
			return &localReuseSource{path: path, record: reused}
		}
	}
	return nil
}

// localCachePass seals and publishes a passing record under its own cache keys, returning the
// stored path. A record that did not pass, a pin-mismatch record and a record the plan validation
// refuses are never cached.
func localCachePass(opts localOptions, record verificationRecord, plan []localJob, stdout io.Writer) string {
	if record.Result != localPass {
		return ""
	}
	if len(record.PinMismatch) > 0 {
		return ""
	}
	if ok, _ := localValidateReuse(record, plan); !ok {
		return ""
	}
	sealed, err := sealRecord(record)
	if err != nil {
		fmt.Fprintf(stdout, "local: the pass-record cache cannot seal the record: %v\n", err)
		return ""
	}
	dir, err := localCacheDirectory(opts, sealed)
	if err != nil {
		fmt.Fprintf(stdout, "local: the pass-record cache is unavailable: %v\n", err)
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stdout, "local: the pass-record cache cannot be made: %v\n", err)
		return ""
	}
	target := filepath.Join(dir, strings.TrimPrefix(sealed.Digest, "sha256:")+".json")
	// A record for these keys already stored under its own digest answers as it is; a different
	// body under the name is not trusted or overwritten.
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			fmt.Fprintf(stdout, "local: the cached record is not a plain file: %s\n", target)
			return ""
		}
		stored, err := readRecord(target)
		if err != nil || stored.Digest != sealed.Digest {
			fmt.Fprintf(stdout, "local: the cached record %s does not match\n", target)
			return ""
		}
		return target
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(stdout, "local: the pass-record cache cannot be checked: %v\n", err)
		return ""
	}
	if _, err := writeRecord(target, sealed); err != nil {
		fmt.Fprintf(stdout, "local: the pass-record cache cannot store the record: %v\n", err)
		return ""
	}
	return target
}
