// Package source computes the source identity of a git working tree: the commit it is on and, when it holds
// uncommitted work, a hash of every non-clean entry. Comparing the identity captured with a piece of evidence
// against the tree as it is now says whether the tree moved since. It ports CXC v0.2.40
// pabcd-state/src/source-identity.ts (commit 3c1459ac) as it is, edge cases included, and enforces nothing itself.
package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

// Kind says whether git could answer; "unavailable" is never a guess.
type Kind string

const KindResolved, KindUnavailable Kind = "resolved", "unavailable"

// Identity is what source tree produced a receipt. The JSON names and order are the oracle's: it is stored in
// state files and receipts.
type Identity struct {
	Kind       Kind   `json:"kind"`
	CommitSha  string `json:"commitSha"` // empty when unavailable or before the first commit
	Dirty      bool   `json:"dirty"`
	TreeHash   string `json:"treeHash,omitempty"` // covers every non-clean entry; empty on a clean tree
	CapturedAt string `json:"capturedAt"`
	// SourceRoot is a bound session's canonical worktree root, nil on legacy captures; a pointer keeps absent apart from empty.
	SourceRoot *string `json:"sourceRoot,omitempty"`
}

// ComparisonKind is the three-way answer of Compare; a boolean would let "unavailable" read as "same".
type ComparisonKind string

const ComparisonSame, ComparisonDifferent, ComparisonUnavailable ComparisonKind = "same", "different", "unavailable"

// Comparison is the result of Compare.
type Comparison struct {
	Kind   ComparisonKind `json:"kind"`
	Detail string         `json:"detail,omitempty"` // why "different"
	Reason string         `json:"reason,omitempty"` // why "unavailable"
}

// Options adjusts a capture.
type Options struct {
	// ExcludeStateArtifacts drops the entries under .crw/ (the FSM's own writes).
	ExcludeStateArtifacts bool
	// GeneratedPaths are repository-relative paths a check command rewrites: an entry that equals or lies below one
	// is dropped; empty entries are ignored.
	GeneratedPaths []string
	// Now supplies the capture time; nil means time.Now.
	Now func() time.Time
}

const (
	stateDirPrefix = ".crw/"
	maxGitOutput   = 64 << 20 // the oracle's maxBuffer, for git's stdout and stderr together
)

// Capture takes the source identity of the working tree at cwd. A status git cannot answer makes the identity
// unavailable; a failing rev-parse (no commit yet) only leaves CommitSha empty, as that tree is worth hashing.
//
// Pass the repository (worktree) root. Git reports entry paths relative to the root and they are read under cwd, so
// below the root an entry has no content and an edit that keeps its status letters does not move the hash: an
// oracle defect that is kept.
func Capture(cwd string, o Options) Identity { return captureWithLimit(cwd, o, maxGitOutput) }

func captureWithLimit(cwd string, o Options, limit int) Identity {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	capturedAt := now().UTC().Format("2006-01-02T15:04:05.000Z")
	commitSha := ""
	if out, err := run(cwd, limit, "git", "rev-parse", "HEAD"); err == nil {
		commitSha = strings.TrimSpace(string(out))
	}
	status, err := run(cwd, limit, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Identity{Kind: KindUnavailable, CapturedAt: capturedAt}
	}
	return resolve(cwd, commitSha, capturedAt, status, o)
}

// resolve turns git's status output into the identity of a tree whose head is commitSha.
func resolve(cwd, commitSha, capturedAt string, status []byte, o Options) Identity {
	state := utf16.Encode([]rune(stateDirPrefix))
	var kept []statusRecord
	for _, r := range parseStatusZ(status) {
		if !(o.ExcludeStateArtifacts && hasPrefix(r.path, state)) && !isGenerated(r.path, o.GeneratedPaths) {
			kept = append(kept, r)
		}
	}
	id := Identity{Kind: KindResolved, CommitSha: commitSha, CapturedAt: capturedAt}
	if len(kept) > 0 {
		id.Dirty, id.TreeHash = true, hashRecords(cwd, kept)
	}
	return id
}

func isGenerated(path []uint16, generated []string) bool {
	for _, g := range generated {
		if u := utf16.Encode([]rune(g)); len(u) > 0 && (slices.Equal(path, u) || hasPrefix(path, append(u, '/'))) {
			return true
		}
	}
	return false
}

func hasPrefix(path, prefix []uint16) bool {
	return len(path) >= len(prefix) && slices.Equal(path[:len(prefix)], prefix)
}

// Compare says whether the tree moved between two identities; an unavailable side is never "different". The
// checks run in the oracle's order with its messages.
func Compare(a, b Identity) Comparison {
	different := func(detail string) Comparison { return Comparison{Kind: ComparisonDifferent, Detail: detail} }
	switch {
	case a.Kind == KindUnavailable || b.Kind == KindUnavailable:
		return Comparison{Kind: ComparisonUnavailable, Reason: "git could not resolve the source identity on at least one side"}
	case (a.SourceRoot == nil) != (b.SourceRoot == nil) || (a.SourceRoot != nil && *a.SourceRoot != *b.SourceRoot):
		return different("source root changed or binding is missing")
	case a.CommitSha != b.CommitSha:
		return different(fmt.Sprintf("commit %s -> %s", short(a.CommitSha), short(b.CommitSha)))
	case a.Dirty && !b.Dirty:
		return different("working tree went clean")
	case a.Dirty != b.Dirty:
		return different("working tree went dirty")
	case a.TreeHash != b.TreeHash:
		return different("uncommitted changes differ")
	}
	return Comparison{Kind: ComparisonSame}
}

// Describe is the short human-readable form used in deny messages.
func Describe(id Identity) string {
	if id.Kind == KindUnavailable {
		return "source unavailable (no git)"
	}
	sha := "no-commit"
	if id.CommitSha != "" {
		sha = short(id.CommitSha)
	}
	if id.Dirty {
		sha += "+dirty"
	}
	return sha
}

func short(sha string) string { return sha[:min(len(sha), 7)] }

// LooksLikeRepo reports whether cwd holds a .git entry: a directory, or the file of a linked worktree. Advisory.
func LooksLikeRepo(cwd string) bool {
	_, err := os.Stat(filepath.Join(cwd, ".git"))
	return err == nil
}

// AssertNever is the oracle's exhaustiveness guard, for the default case of a switch over a Comparison. The
// oracle throws; Go callers return the error. Its compile-time half has no Go equivalent.
func AssertNever(value any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return fmt.Errorf("unhandled case: %s", bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
