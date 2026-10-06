package install_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// policyTextChanged is the registered file's bytes after an operator edits the policy: one more
// allowed pair, so both the bytes and the digest change.
const policyTextChanged = `{"allowed": [{"model": "gpt-5", "efforts": ["high"]}, {"model": "gpt-5-mini", "efforts": ["low"]}]}`

// reRegisterEnv is this host's environment with CRW_HOME pointed at a temporary directory as well,
// so no path a run can read or write leaves the test's own temporary tree.
func (h *host) reRegisterEnv() scope.Env {
	return append(append(scope.Env{}, h.env...), "CRW_HOME="+filepath.Join(h.home, "crw-home"))
}

// handEditedRecord is the record a person would write by hand: the same fields as
// install.BridgeDocument, in another spelling (one line per field, a different key order), so a test
// can pin what the re-registration path does with a record this installer did not write.
func handEditedRecord(executable, policy, digest string) string {
	return "{" + "\n" +
		"  " + strconv.Quote("recordVersion") + ": 2," + "\n" +
		"  " + strconv.Quote("owner") + ": " + strconv.Quote("plugin") + "," + "\n" +
		"  " + strconv.Quote("serverName") + ": " + strconv.Quote("bridge") + "," + "\n" +
		"  " + strconv.Quote("bridgeExecutable") + ": " + strconv.Quote(executable) + "," + "\n" +
		"  " + strconv.Quote("args") + ": []," + "\n" +
		"  " + strconv.Quote("installedBy") + ": " + strconv.Quote("CRW-158") + "," + "\n" +
		"  " + strconv.Quote("executionPolicy") + ": {" + strconv.Quote("path") + ": " + strconv.Quote(policy) + ", " + strconv.Quote("digest") + ": " + strconv.Quote(digest) + "}" + "\n" +
		"}" + "\n"
}

// realHomeListings is the listing of the real home's Codex and CRW directories. A test compares it
// before and after its own run and reports a difference; it never cleans one up.
func realHomeListings(t *testing.T) map[string]string {
	t.Helper()
	home := os.Getenv("HOME")
	out := map[string]string{}
	for _, name := range []string{".codex", ".crw"} {
		entries, err := os.ReadDir(filepath.Join(home, name))
		if err != nil {
			out[name] = err.Error()
			continue
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		out[name] = strings.Join(names, ",")
	}
	return out
}

// reportRealHomeDifference reports, and never repairs, a write that reached the real home.
func reportRealHomeDifference(t *testing.T, before map[string]string) {
	t.Helper()
	after := realHomeListings(t)
	for name, was := range before {
		if after[name] != was {
			t.Logf("REPORT: the real home's %s listing changed: %q -> %q; this task never cleans it up", name, was, after[name])
		}
	}
}

// registerPolicyForTest registers the policy through the command line, as an operator does.
func (h *host) registerPolicyForTest(t *testing.T, policy string) {
	t.Helper()
	var stdout, stderr strings.Builder
	args := []string{"register-mcp", "--owner", "plugin", "--execution-policy", policy}
	if code := install.Main(context.Background(), args, h.reRegisterEnv(), &stdout, &stderr); code != install.OK {
		t.Fatalf("register-mcp: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// updatePolicy runs the re-registration path and answers its decoded result and exit code.
func (h *host) updatePolicy(t *testing.T, extra ...string) (record.Object, int, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	args := append([]string{"register-mcp", "--re-register-policy"}, extra...)
	code := install.Main(context.Background(), args, h.reRegisterEnv(), &stdout, &stderr)
	var result record.Object
	if stdout.Len() > 0 {
		decoded, err := reading.Decode([]byte(stdout.String()))
		if err != nil {
			t.Fatalf("the result is not JSON: %v\n%s", err, stdout.String())
		}
		result = golden.Obj(decoded)
	}
	return result, code, stderr.String()
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// fieldsExcept answers the canonical JSON of every field of document except the named one, so a
// test can assert that a write changed nothing else.
func fieldsExcept(document record.Object, skip string) map[string]string {
	out := map[string]string{}
	for _, f := range document {
		if f.Key == skip {
			continue
		}
		out[f.Key] = golden.Canon(f.Value)
	}
	return out
}

// The re-registration path of this issue. Before it a changed policy file left the operator the
// hand procedure: the create path answers record_differs and writes nothing, which is asserted
// first and is the red the issue names. After it the same file is registered by replacing only the
// record's executionPolicy, the old record is backed up byte for byte, and the answer names the
// replaced field, the backup and the service restart the new policy needs.
func TestReRegisterPolicyReplacesOnlyTheExecutionPolicy(t *testing.T) {
	// sequential: it shortens record.LockTimeout for the whole process.
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, digest := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	before := readFile(t, recordPath)

	// The create path's contract, and the refusal the operator meets today: a record whose policy
	// file changed is not overwritten. Its answer, the repair included, is unchanged by this issue.
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	refused, code := install.RegisterMCP(context.Background(), h.options(), install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: policy})
	if code != install.Refused || at(refused, "outcome") != install.RecordDiffers || golden.Canon(at(refused, "differingFields")) != `["executionPolicy"]` {
		t.Fatalf("the create path against a changed policy: exit %d\n%s", code, golden.Canon(refused))
	}
	wantRepair := "move " + recordPath + " aside by hand, then run crw install register-mcp again." +
		" Threads started in between find no record and start no bridge; threads already running keep the bridge they spawned"
	if repair := text(at(refused, "repair")); repair != wantRepair {
		t.Fatalf("the create path's repair changed: %q", repair)
	}
	if readFile(t, recordPath) != before {
		t.Fatal("the create path's refusal changed the record")
	}

	// The re-registration path itself.
	result, code, stderr := h.updatePolicy(t, "--execution-policy", policy)
	if code != install.OK || at(result, "outcome") != install.RecordUpdated {
		t.Fatalf("re-register-policy: exit %d stderr=%q\n%s", code, stderr, golden.Canon(result))
	}
	if at(result, "replacedField") != "executionPolicy" || at(result, "executionPolicy", "digest") != digestOf(policyTextChanged) ||
		at(result, "executionPolicy", "mode") != "allowlist" || at(result, "applied") != true || at(result, "wrote") != true {
		t.Fatalf("the result does not name the replaced policy:\n%s", golden.Canon(result))
	}
	if !strings.Contains(text(at(result, "restartRequired")), "restart") {
		t.Fatalf("the result does not name the service restart:\n%s", golden.Canon(result))
	}

	// The backup holds the record exactly as it was, and the new record is the old one with one
	// field replaced and every other field's bytes kept.
	backup := text(at(result, "backup"))
	if backup == "" || !strings.HasPrefix(backup, recordPath+".crw-") || !strings.HasSuffix(backup, ".bak") {
		t.Fatalf("backup = %q, want a .bak beside %s", backup, recordPath)
	}
	if got := readFile(t, backup); got != before {
		t.Fatalf("the backup is not the record as it was:\n%s", got)
	}
	after := readFile(t, recordPath)
	if after == before {
		t.Fatal("the record was not replaced")
	}
	oldDocument, err := reading.Decode([]byte(before))
	if err != nil {
		t.Fatal(err)
	}
	newDocument, err := reading.Decode([]byte(after))
	if err != nil {
		t.Fatal(err)
	}
	want := record.Encode(record.Set(golden.Obj(oldDocument), "executionPolicy", record.Get(golden.Obj(newDocument), "executionPolicy")))
	if string(want) != after {
		t.Fatalf("the record is not the old one with only executionPolicy replaced:\n%s\nwant\n%s", after, want)
	}
	for key, value := range fieldsExcept(golden.Obj(oldDocument), "executionPolicy") {
		if golden.Canon(record.Get(golden.Obj(newDocument), key)) != value {
			t.Fatalf("field %q changed: %s -> %s", key, value, golden.Canon(record.Get(golden.Obj(newDocument), key)))
		}
	}
	if digest != digestOf(policyText) || at(result, "executionPolicy", "path") != policy {
		t.Fatalf("the new policy is not the one the flag named:\n%s", golden.Canon(result))
	}
}

// The controls of this issue: a policy that fails the bridge's own check, a policy file that is not
// there, no policy named at all, an unchanged policy, a lock another run holds, and a dry run all
// leave the record and CODEX_HOME as they were, and each names its own answer. A record that names
// no policy and a host with no record are refused too.
func TestReRegisterPolicyRefusesAndWritesNothing(t *testing.T) {
	// sequential: it shortens record.LockTimeout for the whole process.
	realHome := realHomeListings(t)
	defer reportRealHomeDifference(t, realHome)
	h := newHost(t)
	policy, _ := h.policy(t)
	h.registerPolicyForTest(t, policy)
	recordPath := filepath.Join(h.codex, install.BridgeRecordName)
	before := readFile(t, recordPath)
	entries := func() string {
		names, err := os.ReadDir(h.codex)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, entry := range names {
			out = append(out, entry.Name())
		}
		return strings.Join(out, ",")
	}
	beforeEntries := entries()

	// A policy the bridge's own parser refuses.
	bad := filepath.Join(h.home, "bad-policy.json")
	write(t, bad, `{"allowed": "everything"}`)
	if result, code, _ := h.updatePolicy(t, "--execution-policy", bad); code != install.Refused || at(result, "outcome") != install.PolicyUnreadable {
		t.Fatalf("a policy the bridge refuses: exit %d\n%s", code, golden.Canon(result))
	}
	// A policy file that is not there at all.
	if result, code, _ := h.updatePolicy(t, "--execution-policy", filepath.Join(h.home, "absent.json")); code != install.Refused || at(result, "outcome") != install.PolicyUnreadable {
		t.Fatalf("an absent policy: exit %d\n%s", code, golden.Canon(result))
	}
	// No policy named at all: a usage error that says what to name, and the command never reads the
	// record.
	if result, code, _ := h.updatePolicy(t); code != install.Usage || at(result, "outcome") != install.Conflict || !strings.Contains(text(at(result, "detail")), "--execution-policy") {
		t.Fatalf("no policy: exit %d\n%s", code, golden.Canon(result))
	}
	// The policy the record already names: nothing written, nothing backed up.
	if result, code, _ := h.updatePolicy(t, "--execution-policy", policy); code != install.OK || at(result, "outcome") != install.RecordUnchanged {
		t.Fatalf("an unchanged policy: exit %d\n%s", code, golden.Canon(result))
	}
	// Another run holds the ownership lock every writer of this record takes.
	saved := record.LockTimeout
	record.LockTimeout = 100 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	lockPath := filepath.Join(h.codex, install.OwnershipLockName+record.LockSuffix)
	write(t, lockPath, "4242")
	write(t, policy, policyTextChanged)
	if result, code, _ := h.updatePolicy(t, "--execution-policy", policy); code != install.Refused || at(result, "outcome") != install.Busy {
		t.Fatalf("a held ownership lock: exit %d\n%s", code, golden.Canon(result))
	}
	os.Remove(lockPath)
	// A cancelled run: the lock wait ends with the context and nothing is written.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if result, code := install.UpdateRegisteredPolicy(done, h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true}); code != install.Refused || at(result, "outcome") != install.Interrupted {
		t.Fatalf("a cancelled run: exit %d\n%s", code, golden.Canon(result))
	}
	// Two re-registrations inside one stamp's precision each keep their own backup.
	first, code := install.UpdateRegisteredPolicy(context.Background(), h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true})
	if code != install.OK {
		t.Fatalf("the first backup: exit %d\n%s", code, golden.Canon(first))
	}
	write(t, policy, policyText)
	os.Chmod(policy, 0o644)
	back, code := install.UpdateRegisteredPolicy(context.Background(), h.options(), install.PolicyUpdateOptions{ExecutionPolicy: policy, PolicyGiven: true})
	if code != install.OK || text(at(back, "backup")) == text(at(first, "backup")) {
		t.Fatalf("the second backup took the first one's name: exit %d\n%s", code, golden.Canon(back))
	}
	for _, result := range []record.Object{first, back} {
		if err := os.Remove(text(at(result, "backup"))); err != nil {
			t.Fatal(err)
		}
	}
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	write(t, recordPath, before)
	// A dry run decides and writes nothing.
	if result, code, _ := h.updatePolicy(t, "--execution-policy", policy, "--dry-run"); code != install.OK || at(result, "outcome") != install.RecordWouldUpdate || at(result, "wrote") != false {
		t.Fatalf("a dry run: exit %d\n%s", code, golden.Canon(result))
	}
	// Another writer replaces the record after the backup and before the replacement: the
	// replacement is refused rather than overwriting it, and the backup holds the document this
	// decision read.
	edited := string(record.Encode(install.BridgeDocument(filepath.Join(h.dest, "current", "bin", "codex-thread-bridge"), nil, install.ServerName, "CRW-158", record.Object{{Key: "path", Value: policy}, {Key: "digest", Value: digestOf(policyTextChanged)}})))
	restore := install.ReplaceBeforeWriteLock(func(path string) { write(t, path, edited) })
	changed, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	restore()
	if code != install.Refused || at(changed, "outcome") != install.RecordChangedUnderneath || readFile(t, recordPath) != edited {
		t.Fatalf("a record replaced underneath: exit %d\n%s", code, golden.Canon(changed))
	}
	if backup := text(at(changed, "backup")); backup == "" || readFile(t, backup) != before {
		t.Fatalf("the backup is not the document the decision read: %q", backup)
	} else if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	write(t, recordPath, before)
	// A record in a spelling this installer does not write: the fields this path does not replace
	// would be reserialized, so it is refused rather than rewritten.
	handEdited := handEditedRecord(filepath.Join(h.dest, "current", "bin", "codex-thread-bridge"), policy, digestOf(policyTextChanged))
	write(t, recordPath, handEdited)
	nonCanonical, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	if code != install.Refused || at(nonCanonical, "outcome") != install.RecordNotCanonical || readFile(t, recordPath) != handEdited {
		t.Fatalf("a record this installer did not write: exit %d\n%s", code, golden.Canon(nonCanonical))
	}
	write(t, recordPath, before)
	// A Codex configuration that also starts the bridge: the second owner the create path refuses.
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.bridge-by-hand]\ncommand = "+strconv.Quote("/opt/env/bin/codex-thread-bridge")+"\nargs = []\n")
	second, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	if code != install.Refused || at(second, "outcome") != install.Conflict || !strings.Contains(text(at(second, "detail")), "bridge-by-hand") || readFile(t, recordPath) != before {
		t.Fatalf("a second owner: exit %d\n%s", code, golden.Canon(second))
	}
	os.Remove(filepath.Join(h.codex, "config.toml"))
	// An editor rewrites the policy file while the record is being published: the record keeps the
	// digest this run read, and the answer says the policy has moved since rather than reporting a
	// clean update.
	restorePolicy := install.ReplaceBeforeWriteLock(func(path string) {
		write(t, policy, policyTextChanged+" ")
	})
	moved, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	restorePolicy()
	if code != install.Refused || at(moved, "outcome") != install.RecordPolicyChanged || at(moved, "applied") != true || at(moved, "wrote") != true {
		t.Fatalf("a policy rewritten during publication: exit %d\n%s", code, golden.Canon(moved))
	}
	if err := os.Remove(text(at(moved, "backup"))); err != nil {
		t.Fatal(err)
	}
	write(t, policy, policyTextChanged)
	os.Chmod(policy, 0o644)
	write(t, recordPath, before)
	// The directory sync after the rename fails: the record at that path is the new one, so the answer
	// is not "nothing was written".
	restoreSync := install.ReplaceDirectorySync(func(path string, real func(string) error) error {
		if filepath.Dir(recordPath) == path {
			return errors.New("injected directory sync failure")
		}
		return real(path)
	})
	unsynced, code, _ := h.updatePolicy(t, "--execution-policy", policy)
	restoreSync()
	if code != install.Refused || at(unsynced, "outcome") != install.RecordAppliedUnverified || at(unsynced, "applied") != true || at(unsynced, "wrote") != true {
		t.Fatalf("a directory sync failure: exit %d\n%s", code, golden.Canon(unsynced))
	}
	if err := os.Remove(text(at(unsynced, "backup"))); err != nil {
		t.Fatal(err)
	}
	write(t, recordPath, before)
	if readFile(t, recordPath) != before || entries() != beforeEntries {
		t.Fatalf("a refusal changed CODEX_HOME: %s", entries())
	}

	// A version-1 record names no policy, so only a fresh registration can give it one.
	other := newHost(t)
	write(t, filepath.Join(other.home, "policy.json"), policyText)
	write(t, filepath.Join(other.codex, install.BridgeRecordName), string(record.Encode(install.BridgeDocument(filepath.Join(other.dest, "current", "bin", "codex-thread-bridge"), nil, install.ServerName, "CRW-158", nil))))
	one, code := install.UpdateRegisteredPolicy(context.Background(), other.options(), install.PolicyUpdateOptions{ExecutionPolicy: filepath.Join(other.home, "policy.json"), PolicyGiven: true})
	if code != install.Refused || at(one, "outcome") != install.RecordDiffers {
		t.Fatalf("a version-1 record: exit %d\n%s", code, golden.Canon(one))
	}
	// A host with no record at all.
	empty := newHost(t)
	write(t, filepath.Join(empty.home, "policy.json"), policyText)
	if none, code := install.UpdateRegisteredPolicy(context.Background(), empty.options(), install.PolicyUpdateOptions{ExecutionPolicy: filepath.Join(empty.home, "policy.json"), PolicyGiven: true}); code != install.Refused || at(none, "outcome") != install.RecordAbsent {
		t.Fatalf("no record: exit %d\n%s", code, golden.Canon(none))
	}
}
