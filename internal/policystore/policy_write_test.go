package policystore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// recordOf is the wiring record path of an isolated host.
func recordOf(env map[string]string) string {
	return filepath.Join(env["CODEX_HOME"], "crw-bridge-mcp.json")
}

// rewriteRecord writes the record naming path with digest, the way a successful re-registration
// does, so a fake registrar can leave the same durable effect install.Main leaves.
func rewriteRecord(t *testing.T, env map[string]string, path, digest string) {
	t.Helper()
	document := map[string]any{
		"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{},
		"executionPolicy": map[string]any{"path": path, "digest": digest},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordOf(env), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// answer is a RegisterAnswer carrying install's own JSON envelope.
func answer(outcome string, code int) RegisterAnswer {
	return RegisterAnswer{ExitCode: code, Stdout: []byte("{\n  \"outcome\": \"" + outcome + "\"\n}\n")}
}

// neverRegisters fails the test if the registration step is reached: no test in this package may
// run the real installer, and a test that did not expect the step is a wrong test.
func neverRegisters(t *testing.T) RegisterFunc {
	return func(context.Context, string) RegisterAnswer {
		t.Fatal("the registration step ran where the test did not expect it")
		return RegisterAnswer{}
	}
}

// digestOfFile is the digest of the bytes on disk.
func digestOfFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return digestOf(string(raw))
}

// listingOf is the sorted listing of a directory, for the "no new file appeared" assertions.
func listingOf(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// backupsOf is the backup files beside a policy: the only file a write may leave behind beside the
// lock file, which is created once and never unlinked.
func backupsOf(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for _, name := range listingOf(t, dir) {
		if strings.Contains(name, writeBackupPrefix) {
			names = append(names, name)
		}
	}
	return names
}

// updatingRegisters is the registration a successful re-registration performs: the record is
// rewritten to name the file's current digest and the outcome is record_updated.
func updatingRegisters(t *testing.T, env map[string]string) RegisterFunc {
	return func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, digestOfFile(t, path))
		return answer("record_updated", 0)
	}
}

// unavailableRunning is a running-digest reading that could not be made, so applied is
// unverifiable unless a test says otherwise.
func unavailableRunning() func(context.Context, LookupEnv) Running {
	return func(context.Context, LookupEnv) Running {
		return Running{State: RunningUnavailable, Reason: "worker_policy_unreadable"}
	}
}

// removeLegacy is a change every policy in this package accepts.
func removeLegacy() Change { return Change{Kind: KindRemoveException, ID: "legacy"} }

// TestWriteRefusesAStaleDigestAndChangesNothing is C1: the digest is compared under the lock and a
// mismatch is refused with the digest on disk, with no byte of any file changed.
func TestWriteRefusesAStaleDigestAndChangesNothing(t *testing.T) {
	env, file := host(t, policyText, true)
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: "0000", Change: removeLegacy()})
	if result.Kind != WriteStaleDigest {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteStaleDigest)
	}
	if result.CurrentDigest != digestOf(policyText) {
		t.Fatalf("currentDigest = %q, want %q", result.CurrentDigest, digestOf(policyText))
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != policyText {
		t.Fatal("a refused write changed the policy file")
	}
	if left := backupsOf(t, filepath.Dir(file)); len(left) != 0 {
		t.Fatalf("a refused write left a backup behind: %v", left)
	}
}

// TestTheSameRequestTwiceEndsStaleDigest is C1's other half: the second identical request no longer
// matches the file the first one wrote.
func TestTheSameRequestTwiceEndsStaleDigest(t *testing.T) {
	env, _ := host(t, policyText, true)
	opts := WriteOptions{Register: updatingRegisters(t, env), Running: unavailableRunning()}
	request := WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()}
	first := Write(context.Background(), envOf(env), opts, request)
	if first.Kind != WriteStored {
		t.Fatalf("the first write: kind = %q (%v)", first.Kind, first.Errors)
	}
	second := Write(context.Background(), envOf(env), opts, request)
	if second.Kind != WriteStaleDigest {
		t.Fatalf("the second write: kind = %q, want %q", second.Kind, WriteStaleDigest)
	}
}

// TestWriteRefusesAnInvalidChange is C2: a candidate CRW-134's check refuses is never stored, and
// nothing is written for it.
func TestWriteRefusesAnInvalidChange(t *testing.T) {
	env, file := host(t, policyText, true)
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText),
			Change: Change{Kind: KindSetAllowed, Model: "gpt-6.1-sol", Efforts: []string{"max"}}})
	if result.Kind != WriteInvalidPolicy || len(result.Errors) == 0 {
		t.Fatalf("kind = %q errors = %v, want %q with a reason", result.Kind, result.Errors, WriteInvalidPolicy)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("a refused candidate changed the policy file")
	}
	if left := backupsOf(t, filepath.Dir(file)); len(left) != 0 {
		t.Fatalf("a refused candidate left a backup behind: %v", left)
	}
}

// TestWriteRefusesWithoutARegisteredPolicy is the defensive answer for the state the screen shows
// read-only: there is no policy to write.
func TestWriteRefusesWithoutARegisteredPolicy(t *testing.T) {
	env, _ := host(t, policyText, false)
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteNotRegistered || len(result.Errors) == 0 {
		t.Fatalf("kind = %q errors = %v, want %q", result.Kind, result.Errors, WriteNotRegistered)
	}
}

// TestWriteStoresRegistersAndReportsAppliedSeparately is C4: the three fields come back apart, and
// a running relay that holds the old bytes is needs_user_action with the restart, not applied.
func TestWriteStoresRegistersAndReportsAppliedSeparately(t *testing.T) {
	env, file := host(t, policyText, true)
	old := digestOf(policyText)
	opts := WriteOptions{Register: updatingRegisters(t, env),
		Running: func(context.Context, LookupEnv) Running { return Running{State: RunningObserved, Digest: old} }}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: old, Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	newDigest := digestOfFile(t, file)
	if result.StoredDigest != newDigest || result.StoredDigest == old {
		t.Fatalf("stored = %q, want the new digest %q", result.StoredDigest, newDigest)
	}
	if result.RegisteredDigest != newDigest {
		t.Fatalf("registered = %q, want %q", result.RegisteredDigest, newDigest)
	}
	if result.Applied != AppliedNeedsAction {
		t.Fatalf("applied = %q, want %q", result.Applied, AppliedNeedsAction)
	}
	if len(result.Actions) != 1 || result.Actions[0] != AppliedActionRestart {
		t.Fatalf("actions = %v, want the restart", result.Actions)
	}
	if result.Backup == "" {
		t.Fatal("a stored write names no backup")
	}
}

// TestWriteReportsAppliedWhenTheServiceHoldsTheNewBytes is C4's other half.
func TestWriteReportsAppliedWhenTheServiceHoldsTheNewBytes(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: updatingRegisters(t, env),
		Running: func(context.Context, LookupEnv) Running {
			raw, _ := os.ReadFile(file)
			return Running{State: RunningObserved, Digest: digestOf(string(raw))}
		}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored || result.Applied != AppliedApplied {
		t.Fatalf("kind = %q applied = %q", result.Kind, result.Applied)
	}
	if len(result.Actions) != 0 {
		t.Fatalf("actions = %v, want none", result.Actions)
	}
}

// TestTheBackupHoldsTheOriginalBytesAndIsWrittenFirst is C3: the backup carries the bytes the
// decision was made from, so a later restore can put exactly them back.
func TestTheBackupHoldsTheOriginalBytesAndIsWrittenFirst(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: updatingRegisters(t, env), Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored || result.Backup == "" {
		t.Fatalf("kind = %q backup = %q", result.Kind, result.Backup)
	}
	backup, err := os.ReadFile(result.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != policyText {
		t.Fatal("the backup does not hold the original bytes")
	}
	after, _ := os.ReadFile(file)
	if string(after) == policyText {
		t.Fatal("the policy file was not replaced")
	}
}

// TestAFailedRegistrationRestoresTheOriginalBytes is C3's (b): a registration that left the record
// as it was puts the original bytes back and answers register_failed with restored.
func TestAFailedRegistrationRestoresTheOriginalBytes(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: func(context.Context, string) RegisterAnswer { return answer("record_absent", 1) }}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRegisterFailed || !result.Restored {
		t.Fatalf("kind = %q restored = %v (%v)", result.Kind, result.Restored, result.Errors)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("the original bytes were not restored")
	}
	if located := Locate(envOf(env)); located.RegisteredDigest != digestOf(policyText) {
		t.Fatalf("the record moved to %q", located.RegisteredDigest)
	}
	if result.Backup == "" {
		t.Fatal("the failed write names no backup")
	}
}

// TestALostResponseWithBothDigestsNewIsSuccess is C3's (a): the registration happened and only the
// answer was lost, so the error becomes a warning.
func TestALostResponseWithBothDigestsNewIsSuccess(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, digestOfFile(t, path))
		return RegisterAnswer{Err: errors.New("the registration response was lost")}
	}, Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("a write that lost its answer carries no warning")
	}
	if result.RegisteredDigest != digestOfFile(t, file) {
		t.Fatalf("registered = %q, want the file's digest", result.RegisteredDigest)
	}
}

// TestAnUnverifiedRecordIsDecidedByReReading is C3: an outcome the write must not trust is decided
// by reading the file and the record again, and here both name the new digest.
func TestAnUnverifiedRecordIsDecidedByReReading(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, digestOfFile(t, path))
		return answer("record_applied_unverified", 1)
	}, Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	if result.RegisteredDigest != digestOfFile(t, file) {
		t.Fatalf("registered = %q", result.RegisteredDigest)
	}
}

// TestALostResponseWithTheOldRecordRestores is C3's (b) reached through the distrust path.
func TestALostResponseWithTheOldRecordRestores(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: func(context.Context, string) RegisterAnswer {
		return RegisterAnswer{Err: errors.New("the registration timed out")}
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRegisterFailed || !result.Restored {
		t.Fatalf("kind = %q restored = %v (%v)", result.Kind, result.Restored, result.Errors)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("the original bytes were not restored")
	}
}

// TestAThirdDigestNeedsRecovery is C3's (c): the two digests no longer describe one document, so
// the write stops with both named and the recovery command, and it does not pretend to have
// restored anything.
func TestAThirdDigestNeedsRecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	third := digestOf("a document neither the file nor the record holds\n")
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, third)
		return RegisterAnswer{Err: errors.New("the registration response was lost")}
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.FileDigest == "" || result.RegisteredDigest != third || result.Backup == "" || result.Recovery == "" {
		t.Fatalf("a recovery answer is incomplete: %+v", result)
	}
	if result.Restored {
		t.Fatal("a recovery answer claims a restore")
	}
	after, _ := os.ReadFile(file)
	if string(after) == policyText {
		t.Fatal("the file was put back where the write could not reconcile it")
	}
}

// TestTheNextWriteRefusesAfterRecovery is C3's last clause: while the two digests disagree the next
// write refuses before any durable effect.
func TestTheNextWriteRefusesAfterRecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	third := digestOf("a document neither the file nor the record holds\n")
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, third)
		return RegisterAnswer{Err: errors.New("the registration response was lost")}
	}}
	first := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if first.Kind != WriteRecoveryNeeded {
		t.Fatalf("the first write: kind = %q", first.Kind)
	}
	before, _ := os.ReadFile(file)
	next := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(string(before)), Change: removeLegacy()})
	if next.Kind != WriteRecoveryNeeded {
		t.Fatalf("the next write: kind = %q, want %q", next.Kind, WriteRecoveryNeeded)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("the refused write changed the file")
	}
}

// TestWriteRefusesWhileTheFileAndTheRecordDisagree pins the recorded interpretation of the same
// clause for a disagreement the write did not itself create.
func TestWriteRefusesWhileTheFileAndTheRecordDisagree(t *testing.T) {
	env, file := host(t, policyText, true)
	rewriteRecord(t, env, file, digestOf("another document\n"))
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("the refused write changed the file")
	}
}

// TestAFailedRestoreNeedsRecovery is C3: when the restore itself cannot be made, the answer is
// recovery_needed with both digests and the backup, never a silent success.
func TestAFailedRestoreNeedsRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory mode does not refuse root")
	}
	env, file := host(t, policyText, true)
	dir := filepath.Dir(file)
	opts := WriteOptions{Register: func(context.Context, string) RegisterAnswer {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		return answer("record_absent", 1)
	}}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.Restored {
		t.Fatal("a failed restore is reported as restored")
	}
	if result.Backup == "" {
		t.Fatal("the recovery answer names no backup")
	}
}

// TestAFailedWriteLeavesTheFileByteIdentical is C3: a write that could not even back the file up
// changes nothing at all.
func TestAFailedWriteLeavesTheFileByteIdentical(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory mode does not refuse root")
	}
	env, file := host(t, policyText, true)
	dir := filepath.Dir(file)
	// The lock file is created before the directory stops being writable, so the run reaches the
	// backup step rather than failing to take its lock.
	if err := os.WriteFile(file+writeLockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteFailed {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteFailed)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("a failed write changed the file")
	}
}

// TestWriteRefusesASymlinkedPolicy is the path-identity guard: replacing the link path would turn
// the link into a regular file and leave its target naming the old bytes.
func TestWriteRefusesASymlinkedPolicy(t *testing.T) {
	env, file := host(t, policyText, true)
	target := filepath.Join(filepath.Dir(file), "real-policy.json")
	if err := os.Rename(file, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteSymlinked {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteSymlinked)
	}
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symbolic link was replaced by a regular file")
	}
}

// TestWriteStopsAtTheStartOnCancellation is the cancellation clause: a cancelled context starts no
// durable effect and names the step it reached.
func TestWriteStopsAtTheStartOnCancellation(t *testing.T) {
	env, file := host(t, policyText, true)
	before := backupsOf(t, filepath.Dir(file))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Write(ctx, envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteCancelled || result.Step != "start" {
		t.Fatalf("kind = %q step = %q, want %q at start", result.Kind, result.Step, WriteCancelled)
	}
	if !slices.Equal(before, backupsOf(t, filepath.Dir(file))) {
		t.Fatal("a cancelled write left a backup behind")
	}
}

// TestWriteStopsBeforeTheNextDurableEffectOnCancellation cancels the run between the backup and the
// replacement: the backup is there, the policy file is untouched, and the step is named.
func TestWriteStopsBeforeTheNextDurableEffectOnCancellation(t *testing.T) {
	env, file := host(t, policyText, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := WriteOptions{Register: neverRegisters(t), Now: func() time.Time {
		cancel()
		return time.Now()
	}}
	result := Write(ctx, envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteCancelled || result.Step != "backup" {
		t.Fatalf("kind = %q step = %q, want %q at backup", result.Kind, result.Step, WriteCancelled)
	}
	after, _ := os.ReadFile(file)
	if string(after) != policyText {
		t.Fatal("the policy file changed after the cancellation")
	}
	if result.Backup == "" {
		t.Fatal("the cancellation does not name the backup it had already written")
	}
}

// TestTheWrittenPolicyIsWhatTheConsumerReads is C5: the new pair is accepted and the old one
// refused when the file and the new registered digest are read through execution.FromEnvironment.
func TestTheWrittenPolicyIsWhatTheConsumerReads(t *testing.T) {
	env, file := host(t, policyText, true)
	opts := WriteOptions{Register: updatingRegisters(t, env), Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText),
			Change: Change{Kind: KindSetRolePairs, Role: execution.Child,
				Pairs: []Pair{{Model: "gpt-6.1-sol", Effort: "xhigh"}}}})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	policy, err := execution.FromEnvironment(map[string]string{
		execution.EnvPolicy: file, execution.EnvDigest: result.RegisteredDigest})
	if err != nil {
		t.Fatalf("the consumer refused the policy this write registered: %v", err)
	}
	if _, err := policy.Authorize(execution.Input{Model: "gpt-6.1-sol", Effort: "xhigh", Role: execution.Child}); err != nil {
		t.Fatalf("the new pair is not allowed: %v", err)
	}
	if _, err := policy.Authorize(execution.Input{Model: "anthropic/opus", Effort: "xhigh", Role: execution.Child}); err == nil {
		t.Fatal("the old pair is still allowed")
	}
	if _, err := execution.FromEnvironment(map[string]string{
		execution.EnvPolicy: file, execution.EnvDigest: digestOf(policyText)}); err == nil {
		t.Fatal("the consumer accepted the file against the digest it no longer has")
	}
}

// TestTheStoredBytesAreTheCandidateTheCheckJudged pins the source of the bytes on disk: they are
// the package's own encoding of the approved candidate, not a second application of the change.
func TestTheStoredBytesAreTheCandidateTheCheckJudged(t *testing.T) {
	env, file := host(t, policyText, true)
	change := removeLegacy()
	document, err := decode([]byte(policyText))
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := apply(document, change)
	if err != nil {
		t.Fatal(err)
	}
	want := encode(updated)
	opts := WriteOptions{Register: updatingRegisters(t, env), Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: change})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	stored, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != want {
		t.Fatalf("the file does not hold the candidate the check judged:\n%q\nwant\n%q", string(stored), want)
	}
	if result.StoredDigest != digestOf(want) {
		t.Fatalf("stored digest = %q, want the digest of the encoded candidate", result.StoredDigest)
	}
}
