package install_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// policyValue is the executionPolicy member's value as a compact object naming one policy: a
// spelling the installer's own encoder does not produce, so a record carrying it is one this
// installer did not write.
func policyValue(policy, digest string) string {
	return "{" + strconv.Quote("digest") + ": " + strconv.Quote(digest) + ", " + strconv.Quote("path") + ": " + strconv.Quote(policy) + "}"
}

// fourSpaceRecord is the record the create path writes in another spelling: the same members in
// another order, one per line at four spaces, with the executionPolicy member's value written as
// value. It decodes to the document the create path writes.
func fourSpaceRecord(executable, value string) string {
	return "{\n" +
		"    " + strconv.Quote("recordVersion") + ": 2,\n" +
		"    " + strconv.Quote("owner") + ": " + strconv.Quote("plugin") + ",\n" +
		"    " + strconv.Quote("serverName") + ": " + strconv.Quote(install.ServerName) + ",\n" +
		"    " + strconv.Quote("bridgeExecutable") + ": " + strconv.Quote(executable) + ",\n" +
		"    " + strconv.Quote("args") + ": [],\n" +
		"    " + strconv.Quote("installedBy") + ": " + strconv.Quote("CRW-158") + ",\n" +
		"    " + strconv.Quote("executionPolicy") + ": " + value + "\n" +
		"}\n"
}

// mustDecode is a record as its decoded value, or a failure.
func mustDecode(t *testing.T, document string) any {
	t.Helper()
	value, err := reading.Decode([]byte(document))
	if err != nil {
		t.Fatalf("the record is not JSON: %v\n%s", err, document)
	}
	return value
}

// policyValueBounds is the byte range of a record's top-level executionPolicy value, found by a
// token walk of the document's own bytes: an independent reading of where the member's value stands,
// so a test can assert that every byte outside it is the byte it was.
func policyValueBounds(t *testing.T, document string) (int, int) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(document))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("the record is not a JSON object (%v):\n%s", err, document)
	}
	first, last := 0, 0
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name, ok := token.(string)
		if !ok {
			t.Fatalf("a member name is not a string: %v", token)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if name != "executionPolicy" {
			continue
		}
		end := int(decoder.InputOffset())
		begin := end - len(value)
		if begin < 0 || document[begin:end] != string(value) {
			t.Fatalf("the value the walk read is not the document's: %q", string(value))
		}
		first, last = begin, end
	}
	if first == last {
		t.Fatalf("the record names no executionPolicy member:\n%s", document)
	}
	return first, last
}

// assertOnlyThePolicyValueChanged checks that after is before with exactly the executionPolicy
// member's value bytes replaced - the prefix and the suffix around the value are compared byte for
// byte, so whitespace, key order and a trailing newline are pinned - and that the record decodes to
// want.
func assertOnlyThePolicyValueChanged(t *testing.T, before, after string, want record.Object) {
	t.Helper()
	beforeFirst, beforeLast := policyValueBounds(t, before)
	afterFirst, afterLast := policyValueBounds(t, after)
	if before[:beforeFirst] != after[:afterFirst] || before[beforeLast:] != after[afterLast:] {
		t.Fatalf("a byte outside executionPolicy changed:\n%s\n->\n%s", before, after)
	}
	if before[beforeFirst:beforeLast] == after[afterFirst:afterLast] {
		t.Fatal("the executionPolicy value was not replaced")
	}
	if got, wantJSON := golden.Canon(golden.Obj(mustDecode(t, after))), golden.Canon(want); got != wantJSON {
		t.Fatalf("the record decodes to\n%s\nwant\n%s", got, wantJSON)
	}
}

// The raw-byte rule of this issue. The re-registration replaces the value bytes of the record's
// executionPolicy member alone, so a record the launcher reads is re-registered whatever its
// spelling: before this the path refused every record whose bytes were not the installer's canonical
// form, so a record whose last newline was dropped, or one another tool wrote, could not be given a
// new policy at all.
func TestReRegisterPolicyKeepsEveryOtherByteOfTheRecord(t *testing.T) {
	// sequential: it shortens record.LockTimeout for the whole process.
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, digest := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	executable := filepath.Join(h.dest, "current", "bin", "codex-thread-bridge")
	installed := readFile(t, recordPath)

	// The policy file moves on, so the record names a digest the launcher refuses and there is
	// something for the re-registration to write.
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)

	cases := []struct {
		name     string
		document string
	}{
		{"the installer's own record without its trailing newline", strings.TrimSuffix(installed, "\n")},
		{"the installer's own record at another indentation", fourSpaceRecord(executable, policyValue(policy, digest))},
		{"a record one field per line in another key order", handEditedRecord(executable, policy, digest)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The document this case's bytes decode to, with only the policy it names replaced: the
			// re-registration must leave every other byte of this case's own spelling as it was, so the
			// expectation comes from the case rather than from the installer's form.
			want := record.Set(golden.Obj(mustDecode(t, c.document)), "executionPolicy",
				record.Object{{Key: "digest", Value: digestOf(policyTextChanged)}, {Key: "path", Value: policy}})
			write(t, recordPath, c.document)
			result, code, stderr := h.updatePolicy(t, "--execution-policy", policy)
			if code != install.OK || at(result, "outcome") != install.RecordUpdated {
				t.Fatalf("exit %d stderr=%q\n%s", code, stderr, golden.Canon(result))
			}
			if at(result, "executionPolicy", "digest") != digestOf(policyTextChanged) || at(result, "applied") != true {
				t.Fatalf("the answer does not name the replaced policy:\n%s", golden.Canon(result))
			}
			after := readFile(t, recordPath)
			assertOnlyThePolicyValueChanged(t, c.document, after, want)
			if strings.HasSuffix(c.document, "\n") != strings.HasSuffix(after, "\n") {
				t.Fatalf("the record's trailing newline changed:\n%q", after)
			}
			if backup := text(at(result, "backup")); backup == "" || readFile(t, backup) != c.document {
				t.Fatalf("the backup is not the record as it was: %q", backup)
			} else if err := os.Remove(backup); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The control of the raw-byte rule: the refusal that went away is the one for a valid record in
// another spelling, so a record the launcher refuses is still refused - as malformed - and nothing is
// written.
func TestReRegisterPolicyStillRefusesARecordTheLauncherRefuses(t *testing.T) {
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, _ := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	executable := filepath.Join(h.dest, "current", "bin", "codex-thread-bridge")
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)

	// A record the launcher refuses whatever its spelling: the digest it carries is not the shape the
	// launcher accepts.
	broken := fourSpaceRecord(executable, policyValue(policy, "not-a-digest"))
	write(t, recordPath, broken)
	result, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	if code != install.Refused || at(result, "outcome") != install.RecordMalformed {
		t.Fatalf("a record the launcher refuses: exit %d\n%s", code, golden.Canon(result))
	}
	if readFile(t, recordPath) != broken {
		t.Fatalf("a refused record was rewritten:\n%s", readFile(t, recordPath))
	}
}

// The in-lock rule of this issue. The policy file is read again under the ownership lock, so the
// digest this run decides from is the one the launcher would read now. Before this the file was read
// before the wait, and a record that already named that reading was answered record_unchanged
// although the file had moved on and the launcher refused the record's stale digest.
func TestReRegisterPolicyReadsThePolicyFileUnderTheLock(t *testing.T) {
	// sequential: it lengthens record.LockTimeout for the whole process.
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, _ := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	before := readFile(t, recordPath)

	// The control: the file where it was answers record_unchanged.
	if result, code, _ := h.updatePolicy(t, "--execution-policy", policy); code != install.OK || at(result, "outcome") != install.RecordUnchanged {
		t.Fatalf("an unchanged policy: exit %d\n%s", code, golden.Canon(result))
	}

	// Another run holds the ownership lock every writer of this record takes, so this
	// re-registration reads the record and waits for it. The policy file moves while it waits, and
	// the lock is released only after that, so the two are ordered whatever the scheduler does with
	// the goroutine: what the answer must describe is the file as it stands when the lock is held,
	// not the reading taken before the wait.
	saved := record.LockTimeout
	record.LockTimeout = 10 * time.Second
	defer func() { record.LockTimeout = saved }()
	held, err := record.Lock(context.Background(), filepath.Join(h.codex, install.OwnershipLockName), 0)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		result record.Object
		code   int
	}
	done := make(chan answer, 1)
	go func() {
		result, code := install.UpdateRegisteredPolicy(context.Background(), h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true})
		done <- answer{result, code}
	}()
	// Give the run the time to reach the lock it cannot take, then move the file and let it through.
	time.Sleep(150 * time.Millisecond)
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	held.Release()

	got := <-done
	if got.code != install.OK || at(got.result, "outcome") != install.RecordUpdated {
		t.Fatalf("a policy that moved while the run waited for the lock: exit %d\n%s", got.code, golden.Canon(got.result))
	}
	if at(got.result, "executionPolicy", "digest") != digestOf(policyTextChanged) {
		t.Fatalf("the answer does not name the policy the file holds now:\n%s", golden.Canon(got.result))
	}
	if after := readFile(t, recordPath); !strings.Contains(after, digestOf(policyTextChanged)) {
		t.Fatalf("the record does not name the new policy:\n%s", after)
	}
	if backup := text(at(got.result, "backup")); backup == "" || readFile(t, backup) != before {
		t.Fatalf("the backup is not the record this run replaced: %q", backup)
	} else if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
}
