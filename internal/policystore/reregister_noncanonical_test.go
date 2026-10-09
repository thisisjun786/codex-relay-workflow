package policystore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// nonCanonicalRecords are version 2 wiring records the launcher reads and Locate calls registered
// but that this installer would never have written: another key order, one field per line, an
// unknown member, a different indentation and a missing trailing newline. CRW-914 asks that the
// recovery guidance the policy API gives for such a record can be run.
func nonCanonicalRecords(executable, policy, digest string) map[string]string {
	q := strconv.Quote
	reference := "{" + q("path") + ": " + q(policy) + ", " + q("digest") + ": " + q(digest) + "}"
	return map[string]string{
		"hand-edited, reordered, one field per line": "{\n" +
			"  " + q("recordVersion") + ": 2,\n" +
			"  " + q("owner") + ": " + q("plugin") + ",\n" +
			"  " + q("serverName") + ": " + q("codex-thread-bridge") + ",\n" +
			"  " + q("bridgeExecutable") + ": " + q(executable) + ",\n" +
			"  " + q("args") + ": [],\n" +
			"  " + q("installedBy") + ": " + q("by-hand") + ",\n" +
			"  " + q("executionPolicy") + ": " + reference + "\n}\n",
		"policy first, tabs, no trailing newline, unknown member": "{\n" +
			"\t" + q("executionPolicy") + ":\t" + reference + ",\n" +
			"\t" + q("note") + ": " + q("kept as written") + ",\n" +
			"\t" + q("args") + ": [],\n" +
			"\t" + q("bridgeExecutable") + ": " + q(executable) + ",\n" +
			"\t" + q("owner") + ": " + q("plugin") + ",\n" +
			"\t" + q("recordVersion") + ": 2\n}",
	}
}

// TestTheRecoveryGuidanceRunsAgainstARecordThisInstallerDidNotWrite is CRW-914: Locate reads a
// meaningful version 2 record that is not in the installer's spelling as registered, the policy
// read reports needs_user_action with AppliedActionReregister once the policy file moves on, and
// running that guidance leaves the record naming the new digest with only that member changed. The
// refusal this used to meet (record_not_canonical) was removed by CRW-931, which made the installer
// judge the record by the launcher's own check. The two spellings below are read the same way by
// Locate and the installer because both decode the record through pluginwiring.ReadBridgeRecord;
// the two do not share one acceptance judgement (the installer also refuses arguments an exec
// cannot take and a list that starts with the plugin-launch flag, which Locate does not check),
// and this test pins only the spelling case.
func TestTheRecoveryGuidanceRunsAgainstARecordThisInstallerDidNotWrite(t *testing.T) {
	args, _ := reregisterArgs(t)
	for name := range nonCanonicalRecords("/x", "/y", "z") {
		t.Run(name, func(t *testing.T) {
			env, home := installHome(t)
			codexHome := filepath.Join(home, ".codex")
			if err := os.MkdirAll(codexHome, 0o755); err != nil {
				t.Fatal(err)
			}
			policy := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(policy, []byte(presenceOnly), 0o644); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(t.TempDir(), "codex-thread-bridge")
			recordPath := filepath.Join(codexHome, install.BridgeRecordName)
			written := nonCanonicalRecords(executable, policy, digestOf(presenceOnly))[name]
			if err := os.WriteFile(recordPath, []byte(written), 0o644); err != nil {
				t.Fatal(err)
			}
			lookup := func(key string) (string, bool) {
				for i := len(env) - 1; i >= 0; i-- {
					if k, v, _ := strings.Cut(env[i], "="); k == key {
						return v, true
					}
				}
				return "", false
			}
			// Registered before the policy moves.
			if located := Locate(lookup); located.State != Registered || located.RegisteredDigest != digestOf(presenceOnly) {
				t.Fatalf("Locate does not read the record as registered: %+v", located)
			}
			// The policy file moves on: the state the guidance exists for.
			if err := os.WriteFile(policy, []byte(oneAllowed), 0o644); err != nil {
				t.Fatal(err)
			}
			file := Read(Locate(lookup))
			running := Running{State: RunningObserved, Digest: file.Digest}
			applied := Applied(file, running)
			actions := AppliedActions(file, applied)
			if applied != AppliedNeedsAction || len(actions) != 1 || actions[0] != AppliedActionReregister {
				t.Fatalf("applied %q actions %v, want needs_user_action with the re-registration", applied, actions)
			}
			// The guidance, exactly as spelled, with its placeholder naming the changed file.
			run := append([]string(nil), args...)
			for i, arg := range run {
				if arg != "--re-register-policy" && !strings.HasPrefix(arg, "-") && arg != "register-mcp" {
					run[i] = policy
				}
			}
			var stdout, stderr strings.Builder
			code := install.Main(context.Background(), run, env, &stdout, &stderr)
			if code != install.OK || !strings.Contains(stdout.String(), "\"outcome\": \"record_updated\"") {
				t.Fatalf("the guidance did not re-register a record in this spelling: exit %d\n%s%s", code, stdout.String(), stderr.String())
			}
			after, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatal(err)
			}
			wantDigest, oldDigest := digestOf(oneAllowed), digestOf(presenceOnly)
			if !strings.Contains(string(after), wantDigest) || strings.Contains(string(after), oldDigest) {
				t.Fatalf("the record does not name the new digest only:\n%s", after)
			}
			// Only the executionPolicy member's value changed: every other byte of the record is the
			// record's own.
			if without(written) != without(string(after)) {
				t.Fatalf("bytes other than the executionPolicy value changed:\n--- before\n%s\n--- after\n%s", written, after)
			}
			// And the policy API now reads it as registered at that digest, with nothing left to do.
			file = Read(Locate(lookup))
			if file.State != Registered || file.RegisteredDigest != file.Digest {
				t.Fatalf("the record is not in step with the file after the repair: %+v", file)
			}
			if applied := Applied(file, Running{State: RunningObserved, Digest: file.Digest}); applied != AppliedApplied {
				t.Fatalf("applied = %q after the repair, want %q", applied, AppliedApplied)
			}
		})
	}
}

// without is the record with the value of its executionPolicy member cut out.
func without(record string) string {
	start := strings.Index(record, "\"executionPolicy\"")
	open := start + strings.Index(record[start:], "{")
	depth := 0
	for i := open; i < len(record); i++ {
		switch record[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return record[:open] + record[i+1:]
			}
		}
	}
	return record
}
