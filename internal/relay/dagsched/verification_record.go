package dagsched

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// VerificationRecord is the verification-record/1 document CRW-964's writer (crw-dev ci local) leaves: the
// members below are the exact camelCase names that writer emits (internal/dev/ci/local_record.go). The digest is
// the sha256 of the record's canonical serialization: the document as JSON with sorted keys, no HTML escaping, the
// digest member removed and no trailing newline. The reader recomputes it from the record's own bytes.
type VerificationRecord struct {
	Schema       string            `json:"schema"`
	Runner       string            `json:"runner"`
	Repository   string            `json:"repository"`
	BaseCommit   string            `json:"baseCommit"`
	HeadCommit   string            `json:"headCommit"`
	TreeHash     string            `json:"treeHash"`
	CiDigest     string            `json:"ciDigest"`
	Tools        map[string]string `json:"tools"`
	Pins         map[string]string `json:"pins"`
	GoFlags      string            `json:"goFlags"`
	GoEnv        string            `json:"goEnv"`
	PinMismatch  []string          `json:"pinMismatch"`
	Dependencies map[string]string `json:"dependencies"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Result       string            `json:"result"`
	Jobs         []json.RawMessage `json:"jobs"`
	Digest       string            `json:"digest"`
}

// VerificationRecordResultPass is the one result word a reusable record carries (CRW-964 writes "pass").
const VerificationRecordResultPass = "pass"

// verificationDependencyFiles are the dependency files the record digests, by the names the record uses.
var verificationDependencyFiles = []string{"go.sum", "web/package-lock.json"}

// verificationCIFile is the workflow file whose digest the record carries.
const verificationCIFile = ".github/workflows/ci.yml"

// verificationRequired are the members a reusable record must carry. runner, pins, jobs and headCommit are
// not in the list: the reuse keys do not decide on them (headCommit deliberately, since the same tree under
// another commit is the same verification).
var verificationRequired = []string{"schema", "repository", "baseCommit", "treeHash", "ciDigest", "tools", "goFlags", "goEnv",
	"pinMismatch", "dependencies", "os", "arch", "result", "digest"}

// VerificationKeys are what a reusable record must agree with: the tree it names, the base the range covers, the
// ci.yml and dependency digests of that commit, and the platform the relay host runs on.
type VerificationKeys struct {
	Tree         string
	Base         string
	CiDigest     string
	Dependencies map[string]string
	OS, Arch     string
}

// DecodeVerificationRecord reads the document and refuses what is not a verification-record/1: a document that
// is not a JSON object, or whose schema is another. Whether it is reusable is JudgeVerificationRecord's question.
func DecodeVerificationRecord(raw []byte) (VerificationRecord, error) {
	var record VerificationRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&record); err != nil {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record is not a JSON document: %v", err)
	}
	if decoder.More() {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record is one JSON object")
	}
	if record.Schema != VerificationRecordSchema {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record names schema %q; this path reads %s", record.Schema, VerificationRecordSchema)
	}
	return record, nil
}

// canonicalVerificationRecord is the canonical serialization of a record's bytes: the object without its digest
// member, sorted keys (a map encodes sorted), no HTML escaping, no trailing newline.
func canonicalVerificationRecord(raw []byte) ([]byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "digest")
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// VerificationRecordDigest is the digest the record's bytes must carry: sha256:<hex> of their canonical form.
func VerificationRecordDigest(raw []byte) (string, error) {
	canonical, err := canonicalVerificationRecord(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SealVerificationRecord fills in a record's digest the way CRW-964's writer does and returns its bytes, for the
// writer's own tests and for fixtures. A record built by hand must be sealed before a reader accepts it.
func SealVerificationRecord(record VerificationRecord) ([]byte, error) {
	record.Schema = VerificationRecordSchema
	record.Digest = ""
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	digest, err := VerificationRecordDigest(raw)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["digest"] = digest
	return json.Marshal(fields)
}

// JudgeVerificationRecord decides whether a record may be reused for the keys given. Every failure is refused:
// a record of another tree under revision_mismatch, every other reason under disposition_conflict with its own
// detail. The checks run in the order a reader would meet them: presence, digest, result, pins, then the keys.
func JudgeVerificationRecord(raw []byte, want VerificationKeys) (VerificationRecord, error) {
	record, err := DecodeVerificationRecord(raw)
	if err != nil {
		return record, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record is not a JSON object: %v", err)
	}
	for _, name := range verificationRequired {
		if _, ok := members[name]; !ok {
			return record, refuse(contract.RefusalDispositionConflict, "the verification record has no %s member: a record without every reuse key is not reusable", name)
		}
	}
	digest, err := VerificationRecordDigest(raw)
	if err != nil {
		return record, refuse(contract.RefusalMalformedReceipt, "the verification record cannot be canonicalised: %v", err)
	}
	if record.Digest != digest {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record's digest %s does not match its contents (%s)", record.Digest, digest)
	}
	if record.Result != VerificationRecordResultPass {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record's result is %q: only %q is reused", record.Result, VerificationRecordResultPass)
	}
	if len(record.PinMismatch) > 0 {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record carries a tool pin mismatch (%s), so it is not reusable", strings.Join(record.PinMismatch, ", "))
	}
	// a tool that differs from its own pin makes the record internally inconsistent (CRW-965, parent decision D1 refined)
	for name, pinned := range record.Pins {
		if tool, ok := record.Tools[name]; !ok || tool != pinned {
			return record, refuse(contract.RefusalDispositionConflict, "the verification record's tool %s is %q but its pin is %q: the record is not reusable", name, tool, pinned)
		}
	}
	if !strings.EqualFold(record.TreeHash, want.Tree) {
		return record, refuse(contract.RefusalRevisionMismatch, "the verification record names tree %s and the tree judged is %s", record.TreeHash, want.Tree)
	}
	if record.BaseCommit != want.Base {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record covers base %s and the base is %s", record.BaseCommit, want.Base)
	}
	if record.CiDigest != want.CiDigest {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record's ci.yml digest %s is not the commit's %s", record.CiDigest, want.CiDigest)
	}
	for _, file := range verificationDependencyFiles {
		if record.Dependencies[file] != want.Dependencies[file] {
			return record, refuse(contract.RefusalDispositionConflict, "the verification record's %s digest %q is not the commit's %q", file, record.Dependencies[file], want.Dependencies[file])
		}
	}
	if record.OS != want.OS || record.Arch != want.Arch {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record ran on %s/%s and this host is %s/%s", record.OS, record.Arch, want.OS, want.Arch)
	}
	return record, nil
}

// CommitVerificationKeys reads, in a local checkout, what a record must agree with for one commit: its tree, the
// digest of its ci.yml, and the digests of its dependency files. A file the commit does not have digests to "".
func CommitVerificationKeys(ctx context.Context, checkout, commit string) (VerificationKeys, error) {
	var keys VerificationKeys
	tree, err := runGit(ctx, checkout, nil, "rev-parse", commit+"^{tree}")
	if err != nil {
		return keys, refuse(contract.RefusalMergeTargetUnreadable, "git could not read the tree of %s in %s: %v", commit, checkout, err)
	}
	keys.Tree = strings.TrimSpace(tree)
	keys.CiDigest, err = commitFileDigest(ctx, checkout, commit, verificationCIFile)
	if err != nil {
		return keys, err
	}
	keys.Dependencies = map[string]string{}
	for _, file := range verificationDependencyFiles {
		digest, err := commitFileDigest(ctx, checkout, commit, file)
		if err != nil {
			return keys, err
		}
		keys.Dependencies[file] = digest
	}
	return keys, nil
}

// commitFileDigest is sha256:<hex> of a file as a commit has it, or "" when the commit has no such file.
func commitFileDigest(ctx context.Context, checkout, commit, file string) (string, error) {
	code, _, err := runGitExit(ctx, checkout, nil, "cat-file", "-e", commit+":"+file)
	if err != nil && code < 0 {
		return "", refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s of %s: %v", file, commit, err)
	}
	if code != 0 {
		return "", nil
	}
	body, err := runGit(ctx, checkout, nil, "show", commit+":"+file)
	if err != nil {
		return "", refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s of %s: %v", file, commit, err)
	}
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
