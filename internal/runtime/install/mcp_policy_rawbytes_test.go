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

// launcherRecord is a record the launcher reads, with the serverName and the args value written
// as the caller spells them: the raw text reaches the document, so a test can put a value in it
// that no encoder would produce (a lone surrogate escape) beside one the launcher refuses.
func launcherRecord(executable, serverName, args, value string) string {
	return "{\n" +
		"  " + strconv.Quote("recordVersion") + ": 2,\n" +
		"  " + strconv.Quote("owner") + ": " + strconv.Quote("plugin") + ",\n" +
		"  " + strconv.Quote("serverName") + ": " + serverName + ",\n" +
		"  " + strconv.Quote("bridgeExecutable") + ": " + executable + ",\n" +
		"  " + strconv.Quote("args") + ": " + args + ",\n" +
		"  " + strconv.Quote("executionPolicy") + ": " + value + "\n" +
		"}\n"
}

// jsonText is s as a JSON string, the escaping a JSON reader accepts, so a value strconv.Quote
// would spell the Go way (a NUL as \x00) reaches a record as the escape its reader understands.
func jsonText(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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

// The launcher-parity rule of this issue. The refusal that went away was the one for a valid record in
// another spelling, so a record the launcher will not start must still be refused - as malformed, with
// its bytes unchanged and no backup beside it. bridgeComplaints judged version, owner, executable
// shape, args types and the policy reference, but not the serverName the package declares nor whether
// an exec can take the executable and the arguments, so a hand-edited record naming another server, or
// one holding a value no exec could be given, was answered record_updated although the launcher refuses
// it. The launcher's last record-dependent refusal is the one Bridge makes on the argument list it
// would exec - a list beginning with --plugin-launch would start this launcher again instead of the
// bridge - so a record carrying that list was answered record_updated too. The checks are the
// launcher's own (pluginwiring.RecordComplaints, ExecComplaints, ArgumentsStartTheLauncher), asked
// rather than copied.
func TestReRegisterPolicyRefusesARecordTheLauncherRefuses(t *testing.T) {
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, digest := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	executable := filepath.Join(h.dest, "current", "bin", "codex-thread-bridge")
	value := policyValue(policy, digest)
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)

	for _, c := range []struct {
		name     string
		document string
	}{
		{"a serverName this launcher does not declare",
			launcherRecord(jsonText(t, executable), jsonText(t, "bridge"), "[]", value)},
		{"a bridgeExecutable holding a NUL",
			launcherRecord(jsonText(t, executable+"\x00"), jsonText(t, install.ServerName), "[]", value)},
		{"an argument holding a lone surrogate outside U+DC80..U+DCFF",
			launcherRecord(jsonText(t, executable), jsonText(t, install.ServerName), `["\ud800"]`, value)},
		{"args beginning with the flag that would start the launcher again",
			launcherRecord(jsonText(t, executable), jsonText(t, install.ServerName), `["--plugin-launch"]`, value)},
	} {
		t.Run(c.name, func(t *testing.T) {
			write(t, recordPath, c.document)
			result, code, _ := h.updatePolicy(t, "--execution-policy", policy)
			if code != install.Refused || at(result, "outcome") != install.RecordMalformed {
				t.Fatalf("a record the launcher refuses: exit %d\n%s", code, golden.Canon(result))
			}
			if readFile(t, recordPath) != c.document {
				t.Fatalf("a refused record was rewritten:\n%s", readFile(t, recordPath))
			}
			if at(result, "applied") != false || at(result, "wrote") != false {
				t.Fatalf("a refused record was reported written:\n%s", golden.Canon(result))
			}
			if entries, err := os.ReadDir(h.codex); err != nil {
				t.Fatal(err)
			} else {
				for _, entry := range entries {
					if strings.Contains(entry.Name(), ".bak") {
						t.Fatalf("a refused record was backed up: %s", entry.Name())
					}
				}
			}
		})
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
	// The run signals the seam just before it waits for the lock, so the file's change below is
	// ordered after the run has reached that wait, whatever the scheduler does with the goroutine:
	// the answer must describe the file as it stands under the lock, not a reading taken before the
	// wait. Without the seam a loaded runner could change the file before the run starts, and then
	// even a pre-lock reading would pass.
	reached := make(chan struct{})
	restoreSeam := install.ReplaceOwnershipLockWait(func() { close(reached) })
	defer restoreSeam()
	done := make(chan answer, 1)
	go func() {
		result, code := install.UpdateRegisteredPolicy(context.Background(), h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true})
		done <- answer{result, code}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the ownership lock")
	}
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

// A record the launcher reads whose unrelated member holds a JSON constant. The record is read with
// the launcher's own reading, which accepts NaN and the infinities as json.loads does, so such a
// record is one the bridge starts from; replacing its executionPolicy must not fail over a value
// this path never touches.
func TestReRegisterPolicyKeepsARecordHoldingAJSONConstant(t *testing.T) {
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, digest := h.policy(t)
	executable := filepath.Join(h.dest, "current", "bin", "codex-thread-bridge")
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)

	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	old := policyValue(policy, digest)
	before := "{\n" +
		"  " + strconv.Quote("recordVersion") + ": 2,\n" +
		"  " + strconv.Quote("owner") + ": " + strconv.Quote("plugin") + ",\n" +
		"  " + strconv.Quote("serverName") + ": " + strconv.Quote(install.ServerName) + ",\n" +
		"  " + strconv.Quote("bridgeExecutable") + ": " + strconv.Quote(executable) + ",\n" +
		"  " + strconv.Quote("args") + ": [],\n" +
		"  " + strconv.Quote("installedBy") + ": NaN,\n" +
		"  " + strconv.Quote("executionPolicy") + ": " + old + "\n" +
		"}\n"
	write(t, recordPath, before)

	result, code, stderr := h.updatePolicy(t, "--execution-policy", policy)
	if code != install.OK || at(result, "outcome") != install.RecordUpdated {
		t.Fatalf("a record whose unrelated member is a JSON constant: exit %d stderr=%q\n%s", code, stderr, golden.Canon(result))
	}
	after := readFile(t, recordPath)
	// The record is the input with only the executionPolicy member's value replaced: the bytes
	// before the old value and the bytes after it are the bytes they were, the constant included.
	head, tail := before[:strings.Index(before, old)], before[strings.Index(before, old)+len(old):]
	if !strings.HasPrefix(after, head) || !strings.HasSuffix(after, tail) || !strings.Contains(after, "NaN") {
		t.Fatalf("the record is not the input with only executionPolicy replaced:\n%s", after)
	}
	want := record.Set(golden.Obj(mustDecode(t, before)), "executionPolicy",
		record.Object{{Key: "digest", Value: digestOf(policyTextChanged)}, {Key: "path", Value: policy}})
	if got, wantJSON := golden.Canon(golden.Obj(mustDecode(t, after))), golden.Canon(want); got != wantJSON {
		t.Fatalf("the record decodes to\n%s\nwant\n%s", got, wantJSON)
	}
	if backup := text(at(result, "backup")); backup == "" || readFile(t, backup) != before {
		t.Fatalf("the backup is not the record as it was: %q", backup)
	} else if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
}

// A policy another run is repairing while this one waits for the lock. The policy file is read under
// the lock, so a file that is malformed when the run starts and valid when it finally holds the lock
// is registered; refusing from a reading taken before the wait would decide on a file the wait
// invalidated, which is the same stale-read defect as answering record_unchanged from it.
func TestReRegisterPolicyWaitsForAPolicyRepair(t *testing.T) {
	// sequential: it lengthens record.LockTimeout for the whole process.
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, _ := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	before := readFile(t, recordPath)

	// The policy file is malformed, and another run holds the ownership lock while it replaces the
	// file with a valid one.
	write(t, policy, `{"allowed": "everything"}`)
	os.Chmod(policy, 0o644)
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
	// The run signals the seam just before it waits for the lock, so the repair below is ordered after
	// the run has reached that wait: what it must judge is the file as it stands under the lock.
	reached := make(chan struct{})
	restoreSeam := install.ReplaceOwnershipLockWait(func() { close(reached) })
	defer restoreSeam()
	done := make(chan answer, 1)
	go func() {
		result, code := install.UpdateRegisteredPolicy(context.Background(), h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true})
		done <- answer{result, code}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the ownership lock")
	}
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	held.Release()

	got := <-done
	if got.code != install.OK || at(got.result, "outcome") != install.RecordUpdated {
		t.Fatalf("a policy repaired while the run waited for the lock: exit %d\n%s", got.code, golden.Canon(got.result))
	}
	if at(got.result, "executionPolicy", "digest") != digestOf(policyTextChanged) {
		t.Fatalf("the answer does not name the repaired policy:\n%s", golden.Canon(got.result))
	}
	if backup := text(at(got.result, "backup")); backup == "" || readFile(t, backup) != before {
		t.Fatalf("the backup is not the record this run replaced: %q", backup)
	} else if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
}
