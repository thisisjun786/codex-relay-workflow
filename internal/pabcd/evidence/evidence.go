// Package evidence is what the SubagentStop evidence gate keeps: an agent's attempt counter, the check that a receipt it claims
// lies inside the evidence directory, and the tombstone recorded in session state when its attempts run out. It is the Go form
// of CXC v0.2.40 pabcd-state/src/subagent-evidence.ts lines 1-325 (commit 3c1459ac) under the CRW names of
// contract/schema/cxc/name-substitution.json: .crw/evidence, .crw/evidence-attempts, and the tombstone in the session file.
// The unrecordable-verdict marker, the spent budget, resolution and the directives (lines 326-475) and the gate itself (476
// on) belong to other issues and import this package.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md). One defect is fixed: when the session
// file cannot be read, the oracle's second tier replaces it with a default state carrying the corruption sentinel, and the
// bytes it held are lost. Here nothing is written over an unreadable file; the verdict goes to the MarkerWriter instead.
//
// Differences that no recorded case or fixture shows:
//
//   - A Go string cannot hold a lone surrogate. The JSON decode of the hook input turns one into U+FFFD before an id reaches
//     this package, so two agent or turn ids that differ only in lone surrogates are one id here (one counter file, one
//     tombstone identity); the oracle keeps them apart. A counter file nested deeper than Go's JSON limit of 10,000 levels
//     reads as the cap, where JSON.parse accepts it.
//   - Payload is this package's value type, not harness.SubagentStop, so that the hook registration in package harness can
//     import this package. The hook issue converts; an absent field is "".
//   - RecordTombstone takes the last-resort marker writer as an argument. After the fix above it is the only durable denial
//     when the session file is unreadable, so a nil writer is not a supported production value; until the issue that ports the
//     writer and the gate's caller both land, that last resort does nothing.
//   - A temp file is named by the pid and a random string, where the oracle uses Date.now().
package evidence

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// MaxAttempts is the number of blocks an agent gets before the gate releases it and records a tombstone.
const MaxAttempts = 3

// The directories of the evidence under the state directory: receipts, and one counter file per (session, agent, turn).
const (
	Subdir         = "evidence"
	AttemptsSubdir = "evidence-attempts"
)

// Payload is the part of a SubagentStop payload these units read; an absent or non-string field is "" (the oracle reads
// agent_id ?? "" and turn_id ?? ""). It is never persisted as such.
type Payload struct{ AgentType, AgentID, TurnID, LastAssistantMessage string }

// GatedAgentTypes are the agent types the evidence gate applies to: implementers; read-only dispatches are explorers.
func GatedAgentTypes() []string { return []string{"executor", "worker"} }

// IsGatedAgentType reports whether the gate applies to agentType.
func IsGatedAgentType(agentType string) bool { return slices.Contains(GatedAgentTypes(), agentType) }

// contextPressureMarkers are the compaction markers of omo parity, matched case-insensitively in a transcript.
func contextPressureMarkers() []string {
	return []string{"context compacted", "context_length_exceeded", "skill descriptions were shortened", "context_too_large",
		"codex ran out of room in the model's context window", "your input exceeds the context window", "long threads and multiple compactions"}
}

// resolve is path.resolve(cwd, p): an absolute p stands alone, a relative one is taken from cwd, and the result is clean.
func resolve(cwd, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func evidenceRoot(cwd string) string { return resolve(cwd, filepath.Join(crwdir.DirName, Subdir)) }

// ExtractReceiptPath is the path after the first "EVIDENCE_RECORDED:" in message: JavaScript whitespace is skipped (the newline
// too) and the path is the run of characters that are not whitespace. A marker followed by nothing but whitespace gives none;
// no other marker can follow it. The oracle's regular expression takes the first match, not the last line its comment names.
func ExtractReceiptPath(message string) (string, bool) {
	const marker = "EVIDENCE_RECORDED:"
	i := strings.Index(message, marker)
	if i < 0 {
		return "", false
	}
	isSpace := func(r rune) bool { return text.Trim(string(r)) == "" }
	rest := strings.TrimLeftFunc(message[i+len(marker):], isSpace)
	if end := strings.IndexFunc(rest, isSpace); end >= 0 {
		rest = rest[:end]
	}
	return rest, rest != ""
}

// insideDirectory is isPathInsideDirectory: file lies below directory. A relative path that merely starts with ".." counts as
// outside, so a file named "..x" directly in the directory does too.
func insideDirectory(file, directory string) bool {
	rel, err := filepath.Rel(directory, file)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// realPathSafe is the path with its symbolic links resolved, or p itself when that fails.
func realPathSafe(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

// HasValidReceipt reports whether receiptPath (absolute, or relative to cwd) names a non-empty regular file inside the evidence
// directory that is not itself a symbolic link, lexically and after resolving the links of the root and of the file. The file
// is never opened. Any failure is false. A link that makes the evidence root itself point elsewhere is accepted, and a path
// swapped between these checks and its later use is not defended.
func HasValidReceipt(cwd, receiptPath string) bool {
	root := evidenceRoot(cwd)
	resolved := resolve(cwd, receiptPath)
	if !insideDirectory(resolved, root) {
		return false
	}
	if _, err := os.Stat(resolved); err != nil {
		return false
	}
	if info, err := os.Lstat(resolved); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		return false
	}
	if !insideDirectory(realPathSafe(resolved), realPathSafe(root)) {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// TranscriptHasContextPressure reports whether the file at agentTranscriptPath holds a compaction marker, in any case. The whole
// file is read; an empty path or any error is false. U+0130 lower-cases to "i" and U+0307 as in JavaScript, not to "i" alone.
func TranscriptHasContextPressure(agentTranscriptPath string) bool {
	if agentTranscriptPath == "" {
		return false
	}
	raw, err := os.ReadFile(agentTranscriptPath)
	if err != nil {
		return false
	}
	lower := strings.ToLower(strings.ReplaceAll(string(raw), "\u0130", "i\u0307"))
	return slices.ContainsFunc(contextPressureMarkers(), func(m string) bool { return strings.Contains(lower, m) })
}
