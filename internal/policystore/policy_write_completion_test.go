package policystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// This file holds the tests for the completion round's findings: the three the pre-merge evaluation
// raised against the first head (a double-encoded temporary path, a kept file named only in the
// warnings, and a trusted registration that never read the file) and the two the independent review
// raised (the publication's kept answer and its classification of a failed read-back).

// TestAPolicyInADirectoryWhoseNameIsNotUTF8CanBeWritten is the pre-merge evaluation's d1: the
// publication handed its own temporary path - already a kernel spelling - to the package reader,
// which fs-encoded it a second time. A directory whose name holds the bytes ED B2 80 (spelled in the
// record as the three lone surrogates U+DCED U+DCB2 U+DC80) is then read as a different path, so a
// publication that succeeded answered recovery_needed and skipped the registration. The temporary
// path must be read as the kernel spelled it.
func TestAPolicyInADirectoryWhoseNameIsNotUTF8CanBeWritten(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// The directory's own name holds three bytes that are not valid UTF-8.
	rawName := string([]byte{0xED, 0xB2, 0x80})
	dir := filepath.Join(root, "d"+rawName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 name: %v", err)
	}
	real := filepath.Join(dir, "execution-policy.json")
	if err := os.WriteFile(real, []byte(policyText), 0o644); err != nil {
		t.Fatal(err)
	}
	// The record carries those bytes the way a Python writer does: one lone surrogate escape each.
	writeEscapedRecord(t, codexHome, surrogateEscaped(real), digestOf(policyText))
	env := map[string]string{"HOME": root, "CODEX_HOME": codexHome}
	opts := WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		// The installer opens the path it is handed, so what it is handed must be the file on disk.
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the registration was handed %q, which is not the file on disk: %v", path, err)
		}
		writeEscapedRecord(t, codexHome, surrogateEscaped(path), digestOf(string(raw)))
		return answer("record_updated", 0)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v), want %q: the exchange succeeded and only its read-back failed", result.Kind, result.Errors, WriteStored)
	}
	after, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == policyText {
		t.Fatal("the policy was not replaced")
	}
}

// TestKeptBytesAreNamedRatherThanOnlyWarnedAbout is the pre-merge evaluation's d2: the publication
// kept a document another writer put there and reported it only through Warnings. The kept path is
// the only place those bytes exist, so it is a field of the answer in its own right, and the advice
// names it too.
func TestKeptBytesAreNamedRatherThanOnlyWarnedAbout(t *testing.T) {
	env, file := host(t, policyText, true)
	keptPath := filepath.Join(filepath.Dir(file), "kept-raced.json")
	if err := os.WriteFile(keptPath, []byte("a document another writer put there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		// The publication displaced a document another writer saved and could not put it back, so it
		// keeps those bytes at a path of its own instead of deleting them.
		return nil, keptPath, errPolicyMoved
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.Kept != keptPath {
		t.Fatalf("kept = %q, want %q: the only copy of those bytes is not named", result.Kept, keptPath)
	}
	if !strings.Contains(result.Recovery, keptPath) {
		t.Fatalf("the recovery advice does not name the kept file: %q", result.Recovery)
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Fatalf("the kept bytes are gone: %v", err)
	}
}

// TestAFailedReadBackAfterTheExchangeIsNotReportedAsNothingWritten is the independent review's
// blocker: a publication whose exchange ran and whose displaced bytes could not be read back
// answers with the bytes on disk and names the file it holds, rather than being collapsed into
// "nothing was written", which would skip the registration and misreport the durable effect.
func TestAFailedReadBackAfterTheExchangeIsNotReportedAsNothingWritten(t *testing.T) {
	env, file := host(t, policyText, true)
	keptPath := filepath.Join(filepath.Dir(file), "displaced.tmp")
	if err := os.WriteFile(keptPath, []byte(policyText), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		return nil, keptPath, fmt.Errorf("%w: the bytes it displaced could not be read", errExchangeHappened)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q: the exchange ran, so this is not a write that did nothing", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.Kept != keptPath {
		t.Fatalf("kept = %q, want %q", result.Kept, keptPath)
	}
	if result.FileDigest != digestOf(policyText) {
		t.Fatalf("fileDigest = %q, want the digest the file actually holds", result.FileDigest)
	}
	if result.RegisteredDigest != digestOf(policyText) {
		t.Fatalf("registeredDigest = %q, want the digest the record still names", result.RegisteredDigest)
	}
}

// TestATrustedRegistrationReportsTheBytesActuallyStored is the pre-merge evaluation's d3: a
// registration that answered record_updated was trusted, and the candidate's digest was reported as
// what is stored without the file ever being read. An editor that replaces the published candidate
// before the installer reads it leaves the record naming a third document, and the answer must
// report that document rather than the one this run tried to write.
func TestATrustedRegistrationReportsTheBytesActuallyStored(t *testing.T) {
	env, _ := host(t, policyText, true)
	third := "{\n  \"roles\": {},\n  \"allowed\": [],\n  \"exceptions\": {}\n}\n"
	opts := WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		// The editor's document is what the installer reads, verifies and registers.
		if err := os.WriteFile(path, []byte(third), 0o644); err != nil {
			t.Fatal(err)
		}
		rewriteRecord(t, env, path, digestOf(third))
		return answer("record_updated", 0)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v)", result.Kind, result.Errors)
	}
	if result.StoredDigest != digestOf(third) {
		t.Fatalf("stored = %q, want the digest of the bytes on disk (%q)", result.StoredDigest, digestOf(third))
	}
	if result.FileDigest != digestOf(third) {
		t.Fatalf("fileDigest = %q, want the digest of the bytes on disk", result.FileDigest)
	}
	if result.RegisteredDigest != digestOf(third) {
		t.Fatalf("registered = %q, want the digest the record names", result.RegisteredDigest)
	}
	if slices.Contains(result.Actions, AppliedActionReregister) {
		t.Fatalf("the answer asks for a re-registration although the file and the record agree: %v", result.Actions)
	}
}

// TestThePublicationKeepsBytesItDidNotCreate is the independent review's remaining blocker: the
// publication's undo guard decides whether the file it exchanged away holds this call's own
// candidate before it deletes it. A document that is not this call's candidate is kept and named,
// so no writer's bytes are lost to a decision made before it saved. The exchange is wrapped so the
// test reaches the instant between the undo and the read-back deterministically.
func TestThePublicationKeepsBytesItDidNotCreate(t *testing.T) {
	env, file := host(t, policyText, true)
	writerDoc := "a document a writer put at the temporary path\n"
	var keptPath string
	exchange := writeExchange
	calls := 0
	writeExchange = func(a, b string) error {
		calls++
		if calls == 1 {
			// A writer saved to the policy path after this run's locked read but before the
			// exchange, so what the exchange displaces is not the bytes this run authorized.
			if err := os.WriteFile(b, []byte("a document another writer saved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := exchange(a, b); err != nil {
			return err
		}
		if calls == 2 {
			// The undo put this run's candidate back at the temporary path; a writer then puts its
			// own document there before the read-back. Those bytes are not this call's to delete.
			keptPath = a
			if err := os.WriteFile(a, []byte(writerDoc), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Cleanup(func() { writeExchange = exchange })
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if keptPath == "" {
		t.Fatal("the undo exchange never ran, so the guard was not reached")
	}
	if result.Kept != keptPath {
		t.Fatalf("kept = %q, want %q: the bytes at the temporary path were deleted or unreported", result.Kept, keptPath)
	}
	kept, err := os.ReadFile(keptPath)
	if err != nil || string(kept) != writerDoc {
		t.Fatalf("the kept bytes are gone or changed: %v %q", err, string(kept))
	}
	// The writer that saved to the policy path keeps its document too.
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "a document another writer saved\n" {
		t.Fatalf("the writer's document at the policy path was replaced: %q", string(after))
	}
}

// surrogateEscaped spells a kernel path the way a Python writer spells a name whose bytes are not
// UTF-8: every byte that is not valid UTF-8 stands in the record as the lone surrogate U+DCxx, which
// the record carries as the JSON escape \udcXX.
func surrogateEscaped(path string) string {
	decoded := pyvalue.FSDecode(path)
	var out strings.Builder
	for i := 0; i < len(decoded); {
		if i+2 < len(decoded) && decoded[i] == 0xED && decoded[i+1] >= 0xA0 && decoded[i+1] <= 0xBF &&
			decoded[i+2] >= 0x80 && decoded[i+2] <= 0xBF {
			r := rune(decoded[i]&0x0f)<<12 | rune(decoded[i+1]&0x3f)<<6 | rune(decoded[i+2]&0x3f)
			if r >= 0xdc80 && r <= 0xdcff {
				fmt.Fprintf(&out, `\udc%02x`, r-0xdc00)
				i += 3
				continue
			}
		}
		out.WriteByte(decoded[i])
		i++
	}
	return out.String()
}

// writeEscapedRecord writes the wiring record naming a path whose spelling is already the JSON
// escape form, so a path with several non-UTF-8 bytes survives the record verbatim.
func writeEscapedRecord(t *testing.T, codexHome, escapedPath, digest string) {
	t.Helper()
	document := "{\n" +
		"  \"recordVersion\": 2,\n" +
		"  \"owner\": \"plugin\",\n" +
		"  \"serverName\": \"codex-thread-bridge\",\n" +
		"  \"bridgeExecutable\": \"/usr/local/bin/codex-thread-bridge\",\n" +
		"  \"args\": [],\n" +
		"  \"executionPolicy\": {\"path\": \"" + escapedPath + "\", \"digest\": \"" + digest + "\"}\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestATrustedRegistrationWhoseRecordDisagreesIsARecovery is the pre-merge evaluation's d3: a
// registration that answered record_updated is trusted, but the file it read back and the record it
// read back name different documents. A bridge refuses those bytes, so the answer is the (c)
// recovery, never a 200 that presents the disagreement as an applied policy.
func TestATrustedRegistrationWhoseRecordDisagreesIsARecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	third := "{\n  \"roles\": {},\n  \"allowed\": [],\n  \"exceptions\": {}\n}\n"
	opts := WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		// The installer registers the candidate, and an editor then saves another document at the same
		// path before the write reads the file back. The record still names the candidate.
		rewriteRecord(t, env, path, digestOfFile(t, path))
		if err := os.WriteFile(file, []byte(third), 0o644); err != nil {
			t.Fatal(err)
		}
		return answer("record_updated", 0)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q: the file and the record name different documents", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.FileDigest != digestOf(third) {
		t.Fatalf("fileDigest = %q, want the digest the file actually holds", result.FileDigest)
	}
	if result.RegisteredDigest == "" || result.RegisteredDigest == result.FileDigest {
		t.Fatalf("registeredDigest = %q, want the record's own digest, different from the file's", result.RegisteredDigest)
	}
	if len(result.Errors) == 0 {
		t.Fatal("the recovery does not say what disagreed")
	}
}

// TestARestoreThatCouldNotSyncItsUndoWarns is the pre-merge evaluation's d2: the mismatch branch's
// second exchange (the undo) and the removal of the temporary file are durable effects, but only the
// matching-byte success branch synced the directory. A refusal whose rollback was not made durable
// must say so rather than present a confirmed rollback.
func TestARestoreThatCouldNotSyncItsUndoWarns(t *testing.T) {
	env, file := host(t, policyText, true)
	// The editor durably saves its own document between this run's locked read and the publication's
	// exchange, so the compare-and-swap finds bytes it did not authorize and undoes its exchange. The
	// undo's directory sync then fails, and the refusal must say the rollback was not made durable.
	exchange := writeExchange
	first := true
	writeExchange = func(a, b string) error {
		if first {
			first = false
			if err := os.WriteFile(b, []byte("a document an editor saved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return exchange(a, b)
	}
	t.Cleanup(func() { writeExchange = exchange })
	sync := writeSync
	writeSync = func(string) error { return errors.New("the directory could not be synced") }
	t.Cleanup(func() { writeSync = sync })
	result := Write(context.Background(), envOf(env), WriteOptions{Register: neverRegisters(t)},
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	joined := strings.Join(result.Errors, " ")
	if !strings.Contains(joined, "undo") || !strings.Contains(joined, "power") {
		t.Fatalf("the refusal does not warn that its rollback was not made durable: %v", result.Errors)
	}
	// The editor's document is untouched: the undo ran.
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "a document an editor saved\n" {
		t.Fatalf("the editor's document was not put back: %q", string(after))
	}
}

// TestARestoreKeepsTheDisplacedDocumentOnEveryExit is the pre-merge evaluation's d1: once the
// restore's exchange kept a document it could not read, the later failure exits dropped that path.
// The path is the only place those bytes exist, so every exit names it.
func TestARestoreKeepsTheDisplacedDocumentOnEveryExit(t *testing.T) {
	env, file := host(t, policyText, true)
	keptPath := filepath.Join(filepath.Dir(file), "kept-displaced.tmp")
	if err := os.WriteFile(keptPath, []byte("a document the restore displaced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := WriteOptions{
		Register: func(_ context.Context, path string) RegisterAnswer {
			// The registration fails with the record still naming the original, so the (b) restore is
			// decided.
			return answer("record_absent", 1)
		},
		Swap: func(ctx context.Context, path string, expected, next []byte, mode os.FileMode) ([]byte, string, error) {
			calls++
			if calls == 1 {
				// The publication.
				return swapPolicy(ctx, path, expected, next, mode)
			}
			// The restore's exchange ran and what it displaced could not be read back, so the bytes it
			// moved out are kept at a path of its own. The record disappears at the same moment, so the
			// restore cannot be confirmed and the answer must still name those bytes.
			if err := os.Remove(recordOf(env)); err != nil {
				t.Fatal(err)
			}
			return nil, keptPath, fmt.Errorf("%w: the bytes it displaced could not be read", errExchangeHappened)
		},
	}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.Kept != keptPath {
		t.Fatalf("kept = %q, want %q: the only copy of those bytes is not named on this exit", result.Kept, keptPath)
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Fatalf("the kept bytes are gone: %v", err)
	}
}

// TestAKeptPathWhoseBytesAreNotUTF8SurvivesTheAnswer is the pre-merge evaluation's d1: the restore
// held the kept file in the kernel spelling and decoded it again where it was reported, so a name
// whose bytes are not UTF-8 went out as the surrogates of its own surrogate encoding and named no
// file. The path is decoded exactly once, so encoding the answer's spelling returns the kernel path.
func TestAKeptPathWhoseBytesAreNotUTF8SurvivesTheAnswer(t *testing.T) {
	env, file := host(t, policyText, true)
	// The kept file's name holds one byte that is not valid UTF-8.
	raw := append([]byte("kept"), 0x80)
	kernel := filepath.Join(filepath.Dir(file), string(raw))
	if err := os.WriteFile(kernel, []byte("a document the restore displaced\n"), 0o600); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 name: %v", err)
	}
	calls := 0
	opts := WriteOptions{
		Register: func(context.Context, string) RegisterAnswer { return answer("record_absent", 1) },
		Swap: func(ctx context.Context, path string, expected, next []byte, mode os.FileMode) ([]byte, string, error) {
			calls++
			if calls == 1 {
				return swapPolicy(ctx, path, expected, next, mode)
			}
			// The restore's exchange ran; what it displaced could not be read back, so the bytes are
			// kept at the kernel path above. The original bytes are put back, so the file and the
			// record agree again and this is the (b) outcome.
			if err := os.WriteFile(path, next, mode.Perm()); err != nil {
				t.Fatal(err)
			}
			return nil, kernel, fmt.Errorf("%w: the bytes it displaced could not be read", errExchangeHappened)
		},
	}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRegisterFailed || !result.Restored {
		t.Fatalf("kind = %q restored = %v (%v), want %q with restored", result.Kind, result.Restored, result.Errors, WriteRegisterFailed)
	}
	if result.Kept == "" {
		t.Fatal("the kept document is not named")
	}
	// The answer's spelling must encode back to the kernel path, not to the surrogates of it.
	back, ok := pyvalue.FSEncode(result.Kept)
	if !ok {
		t.Fatalf("the answer's kept spelling is not encodable: %q", result.Kept)
	}
	if back != kernel {
		t.Fatalf("kept decodes to %q, want the kernel path %q", back, kernel)
	}
	if _, err := os.Stat(back); err != nil {
		t.Fatalf("following the answer's kept path reaches no file: %v", err)
	}
	if !strings.Contains(result.Warnings[0], pyvalue.FSDecode(kernel)) {
		t.Fatalf("the warning does not name the kept file once: %v", result.Warnings)
	}
}

// TestAConfirmedRestoreWithKeptBytesIsNotAnUnresolvableRecovery is the pre-merge evaluation's d2:
// the restore left a document it could not read but put the file and the record back into agreement.
// Answering recovery_needed for that state left nothing for the next write to refuse, so the write
// after it must not be blocked by a phantom unresolved recovery.
func TestAConfirmedRestoreWithKeptBytesIsNotAnUnresolvableRecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	kept := filepath.Join(filepath.Dir(file), "kept-displaced.tmp")
	if err := os.WriteFile(kept, []byte("a document the restore displaced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := WriteOptions{
		Register: func(context.Context, string) RegisterAnswer { return answer("record_absent", 1) },
		Swap: func(ctx context.Context, path string, expected, next []byte, mode os.FileMode) ([]byte, string, error) {
			calls++
			if calls == 1 {
				return swapPolicy(ctx, path, expected, next, mode)
			}
			if err := os.WriteFile(path, next, mode.Perm()); err != nil {
				t.Fatal(err)
			}
			return nil, kept, fmt.Errorf("%w: the bytes it displaced could not be read", errExchangeHappened)
		},
	}
	first := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if first.Kind != WriteRegisterFailed || !first.Restored {
		t.Fatalf("kind = %q restored = %v (%v), want %q with restored", first.Kind, first.Restored, first.Errors, WriteRegisterFailed)
	}
	if first.Kept != kept {
		t.Fatalf("kept = %q, want %q", first.Kept, kept)
	}
	// The file and the record agree on the original bytes again, so the next write with that digest
	// is not refused by an unresolved recovery: it runs and stores.
	current, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	second := Write(context.Background(), envOf(env), WriteOptions{Register: updatingRegisters(t, env), Running: unavailableRunning()},
		WriteRequest{ExpectedDigest: digestOf(string(current)), Change: removeLegacy()})
	if second.Kind != WriteStored {
		t.Fatalf("the next write: kind = %q (%v), want %q", second.Kind, second.Errors, WriteStored)
	}
}

// TestARecordThatWentAwayAfterTheRegistrationIsARecovery is the pre-merge evaluation's finding that
// a registration reporting success was trusted even when the record could not be read back
// afterwards. The file and the record may then no longer describe one document, so the answer is the
// (c) recovery with the file's digest, the backup and the repair, never a 200 whose applied state
// was never established.
func TestARecordThatWentAwayAfterTheRegistrationIsARecovery(t *testing.T) {
	env, _ := host(t, policyText, true)
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, digestOfFile(t, path))
		// The record is removed after the registration reported success.
		if err := os.Remove(recordOf(env)); err != nil {
			t.Fatal(err)
		}
		return answer("record_updated", 0)
	}, Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q: the record no longer names the policy", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.FileDigest == "" {
		t.Fatal("the file's digest is not reported")
	}
	if result.RegisteredDigest != "" {
		t.Fatalf("registered = %q, want it unestablished", result.RegisteredDigest)
	}
	if result.Backup == "" || result.Recovery == "" {
		t.Fatalf("the recovery does not name the backup and the repair: backup=%q recovery=%q", result.Backup, result.Recovery)
	}
}

// TestARecordNamingAnotherPolicyIsARecovery is the pre-merge evaluation's finding that only the
// digest was compared when the record was read back. A record that names another policy file says
// nothing about the bytes this run replaced, even when the two digests happen to agree: a launcher
// reading that record opens the other file.
func TestARecordNamingAnotherPolicyIsARecovery(t *testing.T) {
	env, file := host(t, policyText, true)
	other := filepath.Join(filepath.Dir(file), "other-policy.json")
	opts := WriteOptions{Register: func(_ context.Context, path string) RegisterAnswer {
		rewriteRecord(t, env, path, digestOfFile(t, path))
		// The record is moved to another policy file that holds the same bytes.
		if err := os.WriteFile(other, []byte(policyText), 0o644); err != nil {
			t.Fatal(err)
		}
		rewriteRecord(t, env, other, digestOf(policyText))
		return answer("record_updated", 0)
	}, Running: unavailableRunning()}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q: the record names another policy file", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if !strings.Contains(strings.Join(result.Errors, " "), other) {
		t.Fatalf("the recovery does not name the policy the record now points at: %v", result.Errors)
	}
}

// TestARecordSpelledDifferentlyForTheSameFileIsNotADisagreement is the pre-merge evaluation's d1:
// the installer records the policy path through pathlib.Path(value).absolute()'s spelling, so a
// record written from a path carrying a "." component or a repeated slash names the same file with
// different text. Comparing the text alone answered a recovery for a write whose file and record in
// fact describe one document, and told the caller to restore a backup nothing needed.
func TestARecordSpelledDifferentlyForTheSameFileIsNotADisagreement(t *testing.T) {
	env, file := host(t, policyText, true)
	// The record spells the same file with a "." component and a repeated slash in the middle of the
	// path, which pathlib.Path(value).absolute() drops.
	dir := filepath.Dir(file)
	spelled := "/./" + strings.Replace(dir[1:], "/", "//", 1) + "/" + filepath.Base(file)
	rewriteRecord(t, env, spelled, digestOf(policyText))
	// The real installer records the path through pathlib.Path(value).absolute()'s spelling, so the
	// re-registration writes the same file back with that spelling rather than the one it read.
	opts := WriteOptions{Running: unavailableRunning(), Register: func(_ context.Context, path string) RegisterAnswer {
		kernel, ok := pyvalue.FSEncode(path)
		if !ok {
			t.Fatalf("the registration was handed an unencodable path: %q", path)
		}
		rewriteRecord(t, env, pyvalue.FSDecode(store.PathlibSpelling(kernel)), digestOfFile(t, path))
		return answer("record_updated", 0)
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteStored {
		t.Fatalf("kind = %q (%v), want %q: the record and the file name one document", result.Kind, result.Errors, WriteStored)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == policyText {
		t.Fatal("the policy was not replaced")
	}
}

// TestAKeptExchangeReportsTheRecordAsItStands is the pre-merge evaluation's d2: the publication's
// kept and moved branches answered the digest this run read under its lock without reading the
// record again. A re-registration run through the command line takes only the ownership lock, so the
// record can move while this write runs; the answer must name the record as it stands, and a state
// where the file and the record already agree must not be reported as a disagreement.
func TestAKeptExchangeReportsTheRecordAsItStands(t *testing.T) {
	env, file := host(t, policyText, true)
	// The publication displaces a document another writer saved, and the record is moved to name the
	// digest that writer's document has while the exchange runs.
	edited := "a document another writer saved\n"
	opts := WriteOptions{Register: neverRegisters(t), Swap: func(context.Context, string, []byte, []byte, os.FileMode) ([]byte, string, error) {
		rewriteRecord(t, env, file, digestOf(edited))
		return nil, "", errPolicyMoved
	}}
	result := Write(context.Background(), envOf(env), opts,
		WriteRequest{ExpectedDigest: digestOf(policyText), Change: removeLegacy()})
	if result.Kind != WriteRecoveryNeeded {
		t.Fatalf("kind = %q (%v), want %q", result.Kind, result.Errors, WriteRecoveryNeeded)
	}
	if result.RegisteredDigest != digestOf(edited) {
		t.Fatalf("registeredDigest = %q, want the digest the record now names (%q)", result.RegisteredDigest, digestOf(edited))
	}
	if result.RegisteredDigest == digestOf(policyText) {
		t.Fatal("the answer repeats the digest this run read under its lock, not the one the record now names")
	}
}
