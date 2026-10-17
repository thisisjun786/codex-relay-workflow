package dagsched

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/execgroup"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
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
	// VerifierKind, VerifierRules and GitHub are the additive remote-verifier members (CRW-1594): absent from
	// every local record, required and judged for runner "github-actions".
	VerifierKind        string               `json:"verifierKind,omitempty"`
	VerifierRules       string               `json:"verifierRules,omitempty"`
	GitHub              *GitHubVerification  `json:"github,omitempty"`
	CommandVerification *CommandVerification `json:"commandVerification,omitempty"`
	Digest              string               `json:"digest"`
	Raw                 json.RawMessage      `json:"-"`
}

const RepositoryCommandRunner = "repository-command"
const RepositoryCommandRules = "repository-command/1"

// CommandVerification records the actual command exit and the digest of both
// output streams. ExitCode is a pointer so a missing exit cannot mean success.
type CommandVerification struct {
	Command   string `json:"command"`
	ExitCode  *int   `json:"exitCode"`
	LogDigest string `json:"logDigest"`
}

// VerificationRecordResultPass is the one result word a reusable record carries (CRW-964 writes "pass").
const VerificationRecordResultPass = "pass"

// verificationDependencyFiles are the dependency files the record digests, by the names the record uses.
var verificationDependencyFiles = []string{"go.sum", "web/package-lock.json"}

// verificationCIFile is the workflow file whose digest the record carries.
const verificationCIFile = ".github/workflows/ci.yml"

// verificationRequired are the members a reusable record must carry. runner, jobs and headCommit are not in the
// list: the reuse keys do not decide on them (headCommit deliberately, since the same tree under another commit is
// the same verification). pins is in the list since CRW-1026: a record without them says nothing about its toolchain.
var verificationRequired = []string{"schema", "repository", "baseCommit", "treeHash", "ciDigest", "tools", "pins", "goFlags", "goEnv",
	"pinMismatch", "dependencies", "os", "arch", "result", "digest"}

// VerificationKeys are what a reusable record must agree with: the tree it names, the base the range covers, the
// ci.yml and dependency digests of that commit, and the platform the relay host runs on.
type VerificationKeys struct {
	RepositoryCommand string
	Tree              string
	Base              string
	CiDigest          string
	Dependencies      map[string]string
	OS, Arch          string
	// Pins are the tool versions the verified commit declares (DeclaredToolPins): the required pin set of a record.
	// CommitVerificationKeys reads them from the commit; they are never read from the local PATH.
	Pins map[string]string
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
	record.Raw = append(json.RawMessage(nil), raw...)
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
	// a record that claims remote provenance or the remote kind but is not a remote record is refused before
	// the local checks; a local record's verifierKind local/ci-local/1 continues through them unchanged.
	if record.Runner != GitHubRunner && (record.GitHub != nil || record.VerifierKind == GitHubVerifierKind) {
		return record, refuse(contract.RefusalDispositionConflict, "the verification record names runner %q with GitHub provenance or the github-actions kind: only a github-actions record carries them", record.Runner)
	}
	if record.Runner == GitHubRunner {
		if want.RepositoryCommand != "" || record.CommandVerification != nil {
			return record, refuse(contract.RefusalDispositionConflict, "a repository command requires its own verification record")
		}
		return judgeGitHubRecord(record, members, want)
	}
	if record.Runner == RepositoryCommandRunner || record.CommandVerification != nil || record.VerifierKind == RepositoryCommandRules || want.RepositoryCommand != "" {
		c := record.CommandVerification
		if record.Runner != RepositoryCommandRunner || record.VerifierKind != RepositoryCommandRules || record.VerifierRules != RepositoryCommandRules || c == nil || c.ExitCode == nil || *c.ExitCode != 0 || want.RepositoryCommand == "" || c.Command != want.RepositoryCommand {
			return record, refuse(contract.RefusalDispositionConflict, "the verification record does not prove the configured repository command %q exited zero", want.RepositoryCommand)
		}
		digest, err := hex.DecodeString(strings.TrimPrefix(c.LogDigest, "sha256:"))
		if !strings.HasPrefix(c.LogDigest, "sha256:") || err != nil || len(digest) != sha256.Size || record.HeadCommit == "" {
			return record, refuse(contract.RefusalDispositionConflict, "the repository command record needs its head and log digest")
		}
	} else if err := judgeRecordPins(record, members, want); err != nil {
		return record, err
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

// judgeRecordPins decides the record's pins (CRW-1026, d1). The record must carry the pins member (not null), every tool it
// pins must equal its own pin (CRW-965, parent decision D1 refined), and every
// tool the verified commit declares must be pinned and carried by the record at exactly the version the commit declares.
// The declaration is the commit's (go.mod, secrets.sh, ci.yml); the PATH of this host is never consulted. A missing pin is
// disposition_conflict and names the tool.
func judgeRecordPins(record VerificationRecord, members map[string]json.RawMessage, want VerificationKeys) error {
	if bytes.Equal(bytes.TrimSpace(members["pins"]), []byte("null")) {
		return refuse(contract.RefusalDispositionConflict, "the verification record's pins member is null: a record without the pins of its toolchain is not reusable")
	}
	// a tool that differs from its own pin makes the record internally inconsistent
	for _, name := range pinNames(record.Pins) {
		pinned := record.Pins[name]
		if tool, ok := record.Tools[name]; !ok || tool != pinned {
			return refuse(contract.RefusalDispositionConflict, "the verification record's tool %s is %q but its pin is %q: the record is not reusable", name, tool, pinned)
		}
	}
	if want.Pins == nil {
		return refuse(contract.RefusalDispositionConflict, "the pins the verified commit declares were not read, so the verification record's pins cannot be judged")
	}
	for _, name := range pinNames(want.Pins) {
		declared := want.Pins[name]
		pin, pinned := record.Pins[name]
		if !pinned {
			return refuse(contract.RefusalDispositionConflict, "the verification record has no pin for %s, which the verified commit declares as %q: the record is not reusable", name, declared)
		}
		if pin != declared {
			return refuse(contract.RefusalDispositionConflict, "the verification record pins %s at %q but the verified commit declares %q: the record is not reusable", name, pin, declared)
		}
		tool, ok := record.Tools[name]
		if !ok {
			return refuse(contract.RefusalDispositionConflict, "the verification record has no tool value for %s, which the verified commit declares as %q: the record is not reusable", name, declared)
		}
		if tool != declared {
			return refuse(contract.RefusalDispositionConflict, "the verification record's tool %s is %q but the verified commit declares %q: the record is not reusable", name, tool, declared)
		}
	}
	return nil
}

// pinNames are the tool names of a pin set in the order CRW-964's writer lists them (go, node, gitleaks, staticcheck), any
// other name after them, sorted, so a refusal names the same tool for the same record.
func pinNames(pins map[string]string) []string {
	known := []string{"go", "node", "gitleaks", "staticcheck"}
	var names []string
	for _, name := range known {
		if _, ok := pins[name]; ok {
			names = append(names, name)
		}
	}
	var rest []string
	for name := range pins {
		if !slices.Contains(known, name) {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(names, rest...)
}

// CommitVerificationKeys reads the keys for flow acceptance and DAG integration,
// including the repository command those paths run. The integration queue uses
// QueueVerificationKeys because its verifier is configured independently.
func CommitVerificationKeys(ctx context.Context, checkout, commit string) (VerificationKeys, error) {
	commands, err := crwconfig.LoadRepositoryCommands(ctx, os.Getenv, checkout)
	if err != nil {
		return VerificationKeys{}, err
	}
	keys, err := commitVerificationKeys(ctx, checkout, commit, commands.VerifyCommand == "")
	keys.RepositoryCommand = commands.VerifyCommand
	return keys, err
}

// QueueVerificationKeys reads the commit's tree, CI and dependency digests and
// declared tool pins without selecting flow's repository command. A file absent
// from the commit digests to "". Queue local/GitHub evidence stays bound to these
// keys even when the same repository has a flow command override.
func QueueVerificationKeys(ctx context.Context, checkout, commit string) (VerificationKeys, error) {
	return commitVerificationKeys(ctx, checkout, commit, true)
}

func commitVerificationKeys(ctx context.Context, checkout, commit string, withPins bool) (VerificationKeys, error) {
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
	keys.Pins = map[string]string{}
	if withPins {
		keys.Pins, err = DeclaredToolPins(func(path string) ([]byte, bool, error) { return commitFileBody(ctx, checkout, commit, path) })
		if err != nil {
			return keys, err
		}
	}
	return keys, nil
}

// VerifyRepositoryCommand runs the configured verifier in a clean detached
// worktree of head. CRW writes the receipt, including failures; the command need
// not know the receipt format. Output is hashed in full and only a bounded tail
// is retained. A failed process is a failed record, not a missing host tool.
func VerifyRepositoryCommand(ctx context.Context, checkout, head, base, command string) (VerificationRecord, error) {
	if strings.TrimSpace(command) == "" {
		return VerificationRecord{}, fmt.Errorf("empty repository verify command")
	}
	head, err := resolveIntegrationCommit(ctx, checkout, head)
	if err != nil {
		return VerificationRecord{}, err
	}
	if base == "" {
		base = head
	}
	base, err = resolveIntegrationCommit(ctx, checkout, base)
	if err != nil {
		return VerificationRecord{}, err
	}
	keys, err := CommitVerificationKeys(ctx, checkout, head)
	if err != nil {
		return VerificationRecord{}, err
	}
	if keys.RepositoryCommand != command {
		return VerificationRecord{}, fmt.Errorf("repository verify command changed from %q to %q", command, keys.RepositoryCommand)
	}
	tmp, err := tmploc.MkdirTemp("crw-command-verify-")
	if err != nil {
		return VerificationRecord{}, err
	}
	wt := filepath.Join(tmp, "tree")
	defer func() {
		_, _ = runGit(context.WithoutCancel(ctx), checkout, nil, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(tmp)
	}()
	if _, err := runGit(ctx, checkout, nil, "worktree", "add", "--detach", wt, head); err != nil {
		return VerificationRecord{}, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Env = wt, repositoryCommandEnv()
	logHash, tail := sha256.New(), &integrationOutputTail{}
	output := io.MultiWriter(logHash, tail)
	cmd.Stdout, cmd.Stderr = output, output
	execgroup.Bind(cmd, execgroup.KillGrace)
	err = cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if !(errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success()) {
			return VerificationRecord{}, err
		}
	}
	result, stepResult, reason := "pass", "passed", ""
	if code != 0 {
		result, stepResult, reason = "fail", "failed", fmt.Sprintf("exit %d: %s", code, tail.String())
	}
	// A command that rewrites the checkout did not verify the immutable tree.
	after, aerr := resolveIntegrationCommit(ctx, wt, "HEAD")
	status, serr := runGit(ctx, wt, nil, "status", "--porcelain", "--untracked-files=no")
	if aerr != nil || serr != nil || after != head || strings.TrimSpace(status) != "" {
		result, stepResult, reason = "fail", "failed", "the verification command changed HEAD or tracked files"
	}
	job, err := json.Marshal(map[string]any{"name": "repository-command", "result": stepResult, "steps": []any{map[string]string{"name": command, "result": stepResult, "reason": reason}}})
	if err != nil {
		return VerificationRecord{}, err
	}
	record := VerificationRecord{Runner: RepositoryCommandRunner, Repository: checkout, BaseCommit: base, HeadCommit: head,
		TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: map[string]string{}, Pins: map[string]string{}, PinMismatch: []string{}, Dependencies: keys.Dependencies,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Result: result, Jobs: []json.RawMessage{job}, VerifierKind: RepositoryCommandRules, VerifierRules: RepositoryCommandRules,
		CommandVerification: &CommandVerification{Command: command, ExitCode: &code, LogDigest: "sha256:" + hex.EncodeToString(logHash.Sum(nil))}}
	raw, err := SealVerificationRecord(record)
	if err != nil {
		return record, err
	}
	return DecodeVerificationRecord(raw)
}

// Git environment inherited from a caller must never redirect a repository command.
func repositoryCommandEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// commitFileBody is a file as a commit has it, and whether the commit has it at all.
func commitFileBody(ctx context.Context, checkout, commit, file string) ([]byte, bool, error) {
	code, _, err := runGitExit(ctx, checkout, nil, "cat-file", "-e", commit+":"+file)
	if err != nil && code < 0 {
		return nil, false, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s of %s: %v", file, commit, err)
	}
	if code != 0 {
		return nil, false, nil
	}
	body, err := runGit(ctx, checkout, nil, "show", commit+":"+file)
	if err != nil {
		return nil, false, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s of %s: %v", file, commit, err)
	}
	return []byte(body), true, nil
}

// commitFileDigest is sha256:<hex> of a file as a commit has it, or "" when the commit has no such file.
func commitFileDigest(ctx context.Context, checkout, commit, file string) (string, error) {
	body, found, err := commitFileBody(ctx, checkout, commit, file)
	if err != nil || !found {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
