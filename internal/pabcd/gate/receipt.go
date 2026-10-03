// Package gate ports two modules of CXC v0.2.40 pabcd-state (commit 3c1459ac): source-receipt.ts, which reads a source-bound test
// or QA receipt, and check-gate.ts, which decides whether the receipt a C>D attestation names may close a check
// (CHECK-BINDING-01). A receipt carries the source identity captured when its evidence was produced, so a gate can tell that the
// tree moved since, not that the evidence was produced: the writer fills in its own identity.
//
// A receipt is returned whole or refused with a ReceiptError (the oracle's {error} object: its isReceiptError is err != nil).
// Behaviour is ported as it is, defects included (docs/port-cxc/known-defects.md), bar two security fixes: every read opens its
// file without following a link at the last element, and the manifest bytes that were digested are the bytes parsed. Not literal:
// the text Node and V8 put in a message is Go's; createdAt is read in the standard spellings (known-defects names the differences
// from Date.parse); a lone surrogate escape reads as U+FFFD.
package gate

import (
	"fmt"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// ReceiptKind is what a receipt records: a test run or a QA verdict set.
type ReceiptKind string

const (
	ReceiptTest ReceiptKind = "test"
	ReceiptQA   ReceiptKind = "qa"
)

// ArtifactKind names a manifest entry: a verdict file or an artifact identity file.
type ArtifactKind string

const (
	ArtifactVerdict  ArtifactKind = "verdict"
	ArtifactIdentity ArtifactKind = "artifact-identity"
)

// ArtifactDigest is one validated manifest entry; CriterionIDs is nil when it names none.
type ArtifactDigest struct {
	Path         string       `json:"path"`
	SHA256       string       `json:"sha256"`
	Kind         ArtifactKind `json:"kind"`
	CriterionIDs []string     `json:"criterionIds,omitempty"`
}

// SourceBoundReceipt is a receipt that passed the parser. An optional field is nil when the file had none of the right type (a
// pointer keeps an empty command or session id, which the gate must see, apart from an absent one); ExitCode is a JavaScript
// number, Infinity included. CreatedAtProvided tells a usable createdAt from the epoch that CreatedAt falls back to.
type SourceBoundReceipt struct {
	Kind              ReceiptKind          `json:"kind"`
	SourceIdentity    state.SourceIdentity `json:"sourceIdentity"`
	Command           *string              `json:"command,omitempty"`
	ExitCode          *float64             `json:"exitCode,omitempty"`
	CreatedAt         string               `json:"createdAt"`
	OwnerSessionID    *string              `json:"ownerSessionId,omitempty"`
	CheckEpoch        *string              `json:"checkEpoch,omitempty"`
	GeneratedPaths    []string             `json:"generatedPaths,omitempty"`
	ArtifactManifest  []ArtifactDigest     `json:"artifactManifest,omitempty"`
	CreatedAtProvided bool                 `json:"createdAtProvided"`
}

// ReceiptError is why a receipt was refused, and the only error ParseSourceBoundReceipt returns.
type ReceiptError struct{ Message string }

func (e ReceiptError) Error() string { return e.Message }

func refuse(format string, args ...any) error {
	return ReceiptError{Message: fmt.Sprintf(format, args...)}
}

// epochISO is new Date(0).toISOString(), the time a receipt or identity gets when its file has no usable one.
const epochISO = "1970-01-01T00:00:00.000Z"

// optString is v when it is a string, else nil.
func optString(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// parseIdentity is a receipt's sourceIdentity, or false when it is not well formed: an object whose kind is resolved or
// unavailable, with a string commitSha and a boolean dirty, and a sourceRoot, when it has one, that is an absolute path.
func parseIdentity(raw any) (state.SourceIdentity, bool) {
	s, isObject := raw.(map[string]any)
	kind, _ := s["kind"].(string)
	commit, hasCommit := s["commitSha"].(string)
	dirty, hasDirty := s["dirty"].(bool)
	if !isObject || (kind != string(source.KindResolved) && kind != string(source.KindUnavailable)) || !hasCommit || !hasDirty {
		return state.SourceIdentity{}, false
	}
	id := state.SourceIdentity{Kind: source.Kind(kind), CommitSha: commit, Dirty: dirty, CapturedAt: epochISO, TreeHash: optString(s["treeHash"])}
	if at := optString(s["capturedAt"]); at != nil {
		id.CapturedAt = *at
	}
	if v, present := s["sourceRoot"]; present {
		if id.SourceRoot = optString(v); id.SourceRoot == nil || !filepath.IsAbs(*id.SourceRoot) {
			return state.SourceIdentity{}, false
		}
	}
	return id, true
}

// ParseSourceBoundReceipt reads the receipt at path (absolute, or relative to cwd) and checks it is of the kind the caller
// expects, which is a parameter so that a test receipt dropped at the QA receipt's path cannot satisfy the QA requirement. The
// path guards (inside the evidence directory, not a link, its real path still inside, a regular file, not empty) are
// evidence.HasValidReceipt's, which answers a boolean, so their five reasons are one message.
func ParseSourceBoundReceipt(path, cwd string, expected ReceiptKind) (*SourceBoundReceipt, error) {
	if path == "" {
		return nil, refuse("receipt path is empty")
	}
	if !evidence.HasValidReceipt(cwd, path) {
		return nil, refuse("receipt failed the evidence-root guard (outside %s/%s, symlink, not a regular file, or empty): %s", crwdir.DirName, evidence.Subdir, path)
	}
	abs := resolve(cwd, path)
	var parsed any
	data, err := readFile(abs, maxText)
	if err == nil {
		parsed, err = decodeJSON(data)
	}
	if err != nil {
		return nil, refuse("receipt is not valid JSON: %s (%v)", path, err)
	}
	r, isObject := parsed.(map[string]any)
	if !isObject {
		return nil, refuse("receipt must be a JSON object: %s", path)
	}
	kind, _ := r["kind"].(string)
	if kind != string(ReceiptTest) && kind != string(ReceiptQA) {
		return nil, refuse(`receipt kind must be "test" or "qa": %s`, path)
	}
	if ReceiptKind(kind) != expected {
		return nil, refuse(`receipt kind mismatch: expected "%s", found "%s" at %s`, expected, kind, path)
	}
	identity, ok := parseIdentity(r["sourceIdentity"])
	if !ok {
		return nil, refuse("receipt is missing a well-formed sourceIdentity: %s", path)
	}
	receipt := &SourceBoundReceipt{Kind: expected, SourceIdentity: identity, CreatedAt: epochISO,
		// Preserved, never required: the C>D gate decides what to do about them.
		Command: optString(r["command"]), OwnerSessionID: optString(r["ownerSessionId"]), CheckEpoch: optString(r["checkEpoch"])}
	if raw, present := r["artifactManifest"]; present {
		if expected != ReceiptQA {
			return nil, refuse("artifactManifest is only valid on a QA receipt")
		}
		if receipt.ArtifactManifest, err = parseArtifactManifest(raw, abs, cwd); err != nil {
			return nil, err
		}
	}
	if created := optString(r["createdAt"]); created != nil && dateParses(*created) {
		receipt.CreatedAt, receipt.CreatedAtProvided = *created, true
	}
	if n, ok := jsNumber(r["exitCode"]); ok {
		receipt.ExitCode = &n
	}
	// Only well-formed string entries survive; a malformed list degrades to "no exclusions declared" rather than rejecting an
	// otherwise valid receipt.
	list, _ := r["generatedPaths"].([]any)
	for _, p := range list {
		if s, ok := p.(string); ok && s != "" {
			receipt.GeneratedPaths = append(receipt.GeneratedPaths, s)
		}
	}
	return receipt, nil
}
