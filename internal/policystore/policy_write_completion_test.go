package policystore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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
