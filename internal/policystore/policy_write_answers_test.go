package policystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// This file holds CRW-1001: the answers a policy write gives when they disagreed with the state the
// write left. The files, the wiring record and the backups were never damaged; the classification
// and the path strings of the answer were wrong.

// TestAMovedFileThatTheRecordAlreadyNamesIsNotARecovery is d1. The API read the original bytes
// inside the lock and judged them; an editor then saved another valid document T and a
// re-registration registered T, so the exchange found T and undid itself. The file and the wiring
// record both name T, nothing needs repairing, and a recovery answer would send the person to settle
// a disagreement that does not exist. The answer is its own outcome: the change was not applied,
// and this is what the document is.
func TestAMovedFileThatTheRecordAlreadyNamesIsNotARecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	moved := strings.Replace(policyText, "devin/swe-2", "devin/swe-3", 1)
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		if err := os.WriteFile(file, []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		rewriteRecord(t, env, file, digestOf(moved))
		return nil, "", errPolicyMoved
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteNotApplied {
		t.Fatalf("kind = %q (%v), want %q: the file and the record name one document", result.Kind, result.Errors, WriteNotApplied)
	}
	if result.Recovery != "" {
		t.Fatalf("a state that needs no repair carries repair advice: %q", result.Recovery)
	}
	if result.FileDigest != digestOf(moved) || result.RegisteredDigest != digestOf(moved) || result.CurrentDigest != digestOf(moved) {
		t.Fatalf("the answer does not say what the document is: file %q registered %q current %q", result.FileDigest, result.RegisteredDigest, result.CurrentDigest)
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "not applied") {
		t.Fatalf("the answer does not say the change was not applied: %v", result.Errors)
	}
	// No recovery state is left behind: the next write, decided against the digest the answer
	// named, is not refused on a disagreement.
	next := Write(context.Background(), envOf(env), WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, file, digestOfFile(t, path))
		return answer("record_updated", 0)
	}}, WriteRequest{ExpectedDigest: digestOf(moved), Change: removeLegacy()})
	if next.Kind == WriteRecoveryNeeded {
		t.Fatalf("the next write is refused as a recovery after a not_applied answer: %+v", next)
	}
}

// TestANotAppliedAnswerCarriesTheUnsyncedUndoWarning pins what the web screen must show: when the
// exchange's undo ran but its directory entry was not synced, the not_applied answer still warns that
// a host that loses power now may find the candidate at the policy path (CRW-1001, verification round 1).
func TestANotAppliedAnswerCarriesTheUnsyncedUndoWarning(t *testing.T) {
	env, file := host(t, policyText, true)
	moved := strings.Replace(policyText, "devin/swe-2", "devin/swe-3", 1)
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		if err := os.WriteFile(file, []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		rewriteRecord(t, env, file, digestOf(moved))
		return nil, "", fmt.Errorf("%w: %w: the directory could not be synced", errPolicyMoved, errUndoSync)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteNotApplied {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteNotApplied)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "may find the candidate at the policy path") {
		t.Fatalf("the unsynced undo is not warned about: %v", result.Warnings)
	}
}

// TestAMovedFileTheRecordDoesNotNameStaysARecovery is d1's other half: when the file moved and the
// record names something else, the two do disagree, and the recovery stands with its advice.
func TestAMovedFileTheRecordDoesNotNameStaysARecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	moved := strings.Replace(policyText, "devin/swe-2", "devin/swe-3", 1)
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		if err := os.WriteFile(file, []byte(moved), 0o644); err != nil {
			t.Fatal(err)
		}
		return nil, "", errPolicyMoved
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded || result.Recovery == "" {
		t.Fatalf("kind = %q recovery %q (%v), want %q with its advice", result.Kind, result.Recovery, result.Errors, WriteRecoveryNeeded)
	}
	if result.FileDigest != digestOf(moved) || result.RegisteredDigest != digestOf(policyText) {
		t.Fatalf("the digests are not the two that disagree: file %q registered %q", result.FileDigest, result.RegisteredDigest)
	}
}

// TestASuccessfulPublicationSyncsItsDirectoryThroughTheSeam is the first of the review's three
// non-blocking findings: the success path called syncDirectory directly, so a test that replaces
// writeSync to fail a sync never reached the success path. The answer for a replacement whose
// directory could not be synced is still stored, with the warning.
func TestASuccessfulPublicationSyncsItsDirectoryThroughTheSeam(t *testing.T) {
	env, file := host(t, policyText, true)
	original := writeSync
	var synced []string
	writeSync = func(dir string) error {
		synced = append(synced, dir)
		return errors.New("the sync failed")
	}
	t.Cleanup(func() { writeSync = original })
	opts := WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, file, digestOfFile(t, path))
		return answer("record_updated", 0)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if len(synced) != 1 {
		t.Fatalf("the success path synced %v through the seam, want its one directory", synced)
	}
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v), want %q: the replacement stands", result.Kind, result.Errors, WriteStored)
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "could not be synced") {
		t.Fatalf("the unsynced replacement carries no warning: %v", result.Warnings)
	}
}

// TestAnExchangeFailureWhileTheContextEndsIsNotCancelled is the second finding: a failure of the
// exchange itself was classified as cancelled whenever ctx.Err() happened to be set, so an I/O error
// that coincided with a closed tab read as the tab's doing. Only an error that is the context's own
// is a cancellation.
func TestAnExchangeFailureWhileTheContextEndsIsNotCancelled(t *testing.T) {
	env, _ := host(t, policyText, true)
	ctx, cancel := context.WithCancel(context.Background())
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		cancel()
		return nil, "", errors.New("the exchange failed: input/output error")
	}}
	result := Write(ctx, envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteFailed {
		t.Fatalf("kind = %q (%v), want %q: the exchange failed for its own reason", result.Kind, result.Errors, WriteFailed)
	}
	if !strings.Contains(strings.Join(result.Errors, " "), "input/output error") {
		t.Fatalf("the failure does not name its cause: %v", result.Errors)
	}
}

// TestACancelledAnswerNamesItsCauseAndTheFile is the third finding: a cancelled write left
// FileDigest empty although the file had been read under the lock and was unchanged, and the cause
// of the cancellation was not in the answer. Every cancellation after that read carries the digest
// of the file it left alone and the cause.
func TestACancelledAnswerNamesItsCauseAndTheFile(t *testing.T) {
	cause := errors.New("the browser tab closed")
	for name, drive := range map[string]func(cancel context.CancelCauseFunc) WriteOptions{
		"publish refused by the context": func(cancel context.CancelCauseFunc) WriteOptions {
			return WriteOptions{Register: neverRegisters(t), Swap: func(ctx context.Context, _ string, _, _ []byte, _ os.FileMode) ([]byte, string, error) {
				cancel(cause)
				return nil, "", ctx.Err()
			}}
		},
		"cancelled between the backup and the publication": func(cancel context.CancelCauseFunc) WriteOptions {
			original := writeCandidate
			writeCandidate = func(raw []byte, change Change) ([]byte, error) {
				cancel(cause)
				return original(raw, change)
			}
			t.Cleanup(func() { writeCandidate = original })
			return WriteOptions{Register: neverRegisters(t)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, file := host(t, policyText, true)
			ctx, cancel := context.WithCancelCause(context.Background())
			result := Write(ctx, envOf(env), drive(cancel),
				WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
			if result.Kind != WriteCancelled {
				t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteCancelled)
			}
			if result.FileDigest != digestOf(policyText) {
				t.Fatalf("fileDigest = %q, want the digest of the file the write left alone", result.FileDigest)
			}
			if !strings.Contains(strings.Join(result.Errors, " "), cause.Error()) {
				t.Fatalf("the answer does not name the cause %q: %v", cause, result.Errors)
			}
			if after, _ := os.ReadFile(file); string(after) != policyText {
				t.Fatal("a cancelled write changed the file")
			}
		})
	}
}

// TestTheEnvelopeKeepsALoneSurrogateInTheBackupPath is d2 at the unit: the installer writes a path
// whose bytes are not UTF-8 as a lone surrogate escape, and reading the envelope with
// encoding/json folds it into U+FFFD, which no later step can turn back into the byte.
func TestTheEnvelopeKeepsALoneSurrogateInTheBackupPath(t *testing.T) {
	stdout := `{"outcome": "record_updated", "backup": "/work/d\udc80/.codex/record.backup-1", "restartRequired": "restart é the bridge"}`
	envelope, parsed := registrationEnvelopeOf(RegisterAnswer{Stdout: []byte(stdout)})
	if !parsed || envelope.Outcome != "record_updated" {
		t.Fatalf("envelope not read: %+v", envelope)
	}
	kernel, ok := pyvalue.FSEncode(envelope.Backup)
	if !ok || kernel != "/work/d\x80/.codex/record.backup-1" {
		t.Fatalf("backup %q does not encode to the installer's file name: %q %v", envelope.Backup, kernel, ok)
	}
	if envelope.RestartRequired != "restart é the bridge" {
		t.Fatalf("restartRequired = %q", envelope.RestartRequired)
	}
	// An answer that is not JSON, or names no outcome, is still untrusted.
	for _, bad := range []string{"", "{", `{"backup": "x"}`, `[]`, `{"outcome": 3}`} {
		if _, parsed := registrationEnvelopeOf(RegisterAnswer{Stdout: []byte(bad)}); parsed {
			t.Fatalf("%q was read as a trusted envelope", bad)
		}
	}
}

// TestTheRecordBackupNamesTheFileTheInstallerMadeWhenTheDirectoryIsNotUTF8 is d2 end to end with the
// real installer: the working directory has a byte 0x80 in its name and CODEX_HOME is relative, so
// the installer resolves the record's path against the working directory and its backup lies in a
// directory that is not UTF-8. The recordBackup the write answers must name the file that exists.
func TestTheRecordBackupNamesTheFileTheInstallerMadeWhenTheDirectoryIsNotUTF8(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "work\x80dir")
	if err := os.MkdirAll(filepath.Join(cwd, ".codex"), 0o755); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 name: %v", err)
	}
	t.Chdir(cwd)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", ".codex")
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CRW_HOME", filepath.Join(root, "crw-home"))
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(policyText), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"register-mcp", "--owner", "plugin", "--execution-policy", policy},
		scope.Env(os.Environ()), &stdout, &stderr); code != install.OK {
		t.Fatalf("register-mcp: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	env := map[string]string{"HOME": root, "CODEX_HOME": ".codex", "XDG_STATE_HOME": filepath.Join(root, "state")}
	result := Write(context.Background(), envOf(env), WriteOptions{Running: unavailableRunning()},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteStored)
	}
	if result.RecordBackup == "" {
		t.Fatal("the answer carries no record backup")
	}
	kernel, ok := pyvalue.FSEncode(result.RecordBackup)
	if !ok {
		t.Fatalf("the record backup %q does not name a file", result.RecordBackup)
	}
	if !strings.Contains(kernel, "work\x80dir") {
		t.Fatalf("the record backup %q lost the byte of the directory name", kernel)
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Fatalf("the record backup the answer names does not exist: %v", err)
	}
}
