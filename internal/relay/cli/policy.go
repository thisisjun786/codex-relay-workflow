package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// rolePolicy is rolepolicy.declared(): Unresolved (digest "") or Declared.
type rolePolicy struct {
	digest       string
	detail       string // Unresolved.detail
	publicDetail string // Unresolved.public_detail
	roles        contract.OrderedObject
}

func (p rolePolicy) declared() bool { return p.digest != "" }

// summary is Declared.summary() / Unresolved.summary().
func (p rolePolicy) summary() contract.OrderedObject {
	if !p.declared() {
		return contract.OrderedObject{
			{Key: "state", Value: "unresolved"}, {Key: "digest", Value: nil},
			{Key: "roles", Value: contract.OrderedObject{}}, {Key: "detail", Value: p.publicDetail},
		}
	}
	return contract.OrderedObject{
		{Key: "state", Value: "declared"}, {Key: "digest", Value: p.digest},
		{Key: "roles", Value: p.roles}, {Key: "detail", Value: nil},
	}
}

// declaredPolicy is rolepolicy._resolve for one configured value: the bridge's own parser
// (internal/bridge/execution, as rolepolicy imports codex_thread_bridge.execution). Only what
// doctor reports is taken from it: the digest and the public summary.
func declaredPolicy(configured string) (rolePolicy, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return rolePolicy{
			detail:       policyVariable + " is not set in this process, so no role policy can be read",
			publicDetail: "execution policy environment is not configured in this process",
		}, nil
	}
	policy, err := execution.FromFile(pathlibSpelling(configured))
	var unreadable *execution.PolicyError
	if errors.As(err, &unreadable) {
		return rolePolicy{detail: unreadable.Error(), publicDetail: "configured execution policy is unreadable or invalid"}, nil
	}
	if err != nil {
		return rolePolicy{}, err
	}
	summary := policy.Summary()
	declaredRoles, _ := summary["roles"].(map[string]any)
	if len(declaredRoles) == 0 {
		return rolePolicy{detail: "this host's execution policy declares no roles", publicDetail: "execution policy declares no roles"}, nil
	}
	digest, _ := summary["digest"].(string)
	return rolePolicy{digest: digest, roles: orderedRoles(declaredRoles)}, nil
}

// orderedRoles is the summary's roles as an object in roles.ROLES order. Only equality with a
// worker's published summary is asked of it, which ignores key order, as dict == does.
func orderedRoles(declared map[string]any) contract.OrderedObject {
	out := contract.OrderedObject{}
	for _, name := range []string{execution.Supervisor, execution.Parent, execution.Child} {
		receipt, ok := declared[name].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, contract.Field{Key: name, Value: contract.OrderedObject{
			{Key: "role", Value: receipt["role"]}, {Key: "expectation", Value: receipt["expectation"]},
			{Key: "model", Value: receipt["model"]}, {Key: "reasoningEffort", Value: receipt["reasoningEffort"]},
		}})
	}
	return out
}

// pathlibSpelling is str(Path(value)).
func pathlibSpelling(value string) string {
	absolute := strings.HasPrefix(value, "/")
	var parts []string
	for _, part := range strings.Split(value, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	joined := strings.Join(parts, "/")
	if absolute {
		return "/" + joined
	}
	if joined == "" {
		return "."
	}
	return joined
}

// rolePolicyReport is _role_policy_report.
func rolePolicyReport(policy rolePolicy) contract.OrderedObject {
	state, digest, detail := "unresolved", any(nil), any(policy.detail)
	if policy.declared() {
		state, digest, detail = "declared", policy.digest, nil
	}
	return contract.OrderedObject{
		{Key: "state", Value: state}, {Key: "digest", Value: digest}, {Key: "detail", Value: detail},
		{Key: "variable", Value: policyVariable}, {Key: "bridgeDigest", Value: nil},
		{Key: "agreement", Value: "not_observable_from_here"},
		{Key: "compareWith", Value: "codex-thread-bridge get_capabilities -> executionPolicy.digest, read " +
			"through the MCP client that owns that connection"},
	}
}

// launchRecord is LaunchPolicy.read().
type launchRecord struct {
	path, unreadable string
	declaredAt       any
	declaredBy       any
}

func readLaunchRecord(path string) launchRecord {
	unreadable := func(why string) launchRecord { return launchRecord{unreadable: why} }
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		if _, lerr := os.Lstat(path); lerr == nil {
			return unreadable("the launch declaration " + path + " is a link to nothing")
		}
		return launchRecord{}
	}
	if err != nil {
		return unreadable("the launch declaration could not be read: " + store.PythonOSErrorText(err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return unreadable("the launch declaration could not be read: " + store.PythonOSErrorText(err))
	}
	if !info.Mode().IsRegular() {
		return unreadable("the launch declaration " + path + " is not a regular file")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return unreadable("the launch declaration could not be read: " + store.PythonOSErrorText(err))
	}
	data, err := decodeJSON(raw)
	if err != nil {
		return unreadable("the launch declaration is not readable JSON: " + jsonErrorText(raw, err))
	}
	declared, ok := get(data, "path").(string)
	if !ok || strings.TrimSpace(declared) == "" {
		return launchRecord{unreadable: "the launch declaration names no execution policy file"}
	}
	return launchRecord{path: declared, declaredAt: get(data, "declaredAt"), declaredBy: get(data, "declaredBy")}
}

// canonicalPolicyPath is canonical_policy_path.
func canonicalPolicyPath(value string) string {
	expanded, err := store.ExpandUser(value)
	if err != nil {
		return value
	}
	resolved, err := store.ResolvePath(expanded)
	if err != nil {
		return value
	}
	return resolved
}

// resolveLaunchPolicy is RelayService.resolve_launch_policy over this process's environment.
func resolveLaunchPolicy(services Services) (contract.OrderedObject, error) {
	stated := strings.TrimSpace(os.Getenv(policyVariable))
	record := readLaunchRecord(filepath.Join(services.Selection.Path, "launch-policy.json"))
	answer := contract.OrderedObject{
		{Key: "variable", Value: policyVariable}, {Key: "path", Value: nil}, {Key: "source", Value: nil},
		{Key: "record", Value: nullableText(record.path)}, {Key: "environment", Value: nullableText(stated)},
		{Key: "declaredAt", Value: record.declaredAt}, {Key: "declaredBy", Value: record.declaredBy},
		{Key: "state", Value: nil}, {Key: "digest", Value: nil}, {Key: "persisted", Value: nil},
		{Key: "detail", Value: nil}, {Key: "hint", Value: nil},
	}
	set := func(key string, value any) { answer[fieldIndex(answer, key)].Value = value }
	switch {
	case record.unreadable != "":
		set("source", "unreadable_record")
		set("detail", record.unreadable)
		set("hint", "declare the file again with service declare --execution-policy, or drop the"+
			" record with service declare --forget-execution-policy. A launch does not"+
			" fall back to this process's environment to cover an unreadable record")
		return answer, nil
	case record.path != "" && stated != "" && canonicalPolicyPath(stated) != canonicalPolicyPath(record.path):
		set("source", "conflict")
		set("detail", fmt.Sprintf("this service declares %s and %s in this process names %s",
			store.PythonRepr(record.path), policyVariable, store.PythonRepr(stated)))
		set("hint", "two files are not a preference: unset the variable to launch on the"+
			" declaration, or declare that other file")
		return answer, nil
	case record.path != "":
		set("path", record.path)
		set("source", "record")
		set("persisted", true)
	case stated != "":
		set("path", stated)
		set("source", "environment")
		set("persisted", false)
		set("hint", "this launch takes the policy from this process's environment and nothing"+
			" records it, so a restart typed anywhere else loses it. Record it with"+
			" service declare --execution-policy")
	default:
		set("detail", "no execution policy is declared for this service and "+policyVariable+
			" is not set in this process, so a daemon launched from here can read none and withholds every role-bound delivery")
		return answer, nil
	}
	reading, err := declaredPolicy(get(answer, "path").(string))
	if err != nil {
		return nil, err
	}
	if reading.declared() {
		set("state", "declared")
		set("digest", reading.digest)
	} else {
		set("state", "unreadable")
		set("detail", reading.detail)
	}
	return answer, nil
}

// workerReadiness is rolepolicy.worker_readiness.
func workerReadiness(observation contract.OrderedObject, requirements any, caller rolePolicy) contract.OrderedObject {
	refused := func(reason any) contract.OrderedObject {
		return contract.OrderedObject{{Key: "ready", Value: false}, {Key: "reason", Value: reason}, {Key: "digest", Value: nil}}
	}
	items, ok := requirements.([]any)
	if !ok || len(items) == 0 {
		return refused("worker_policy_requirements_invalid")
	}
	for _, item := range items {
		object, ok := item.(contract.OrderedObject)
		if !ok || len(object) != 3 || !has(object, "role") || !has(object, "model") || !has(object, "reasoningEffort") {
			return refused("worker_policy_requirements_invalid")
		}
		for _, field := range object {
			text, ok := field.Value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return refused("worker_policy_requirements_invalid")
			}
		}
		if role := get(object, "role"); role != "parent" && role != "child" {
			return refused("worker_policy_role_unsupported")
		}
	}
	if !truthy(get(observation, "observed")) {
		reason := get(observation, "reason")
		if !truthy(reason) {
			reason = "worker_policy_unobserved"
		}
		return refused(reason)
	}
	worker, ok := get(observation, "policy").(contract.OrderedObject)
	if !ok || get(worker, "state") != "declared" {
		return refused("worker_policy_unconfigured")
	}
	if !caller.declared() {
		return refused("caller_policy_unconfigured")
	}
	if get(worker, "digest") != caller.digest {
		return refused("worker_policy_digest_mismatch")
	}
	if !pyEqual(worker, caller.summary()) {
		return refused("worker_policy_summary_mismatch")
	}
	for _, item := range items {
		expected, ok := get(caller.roles, get(item, "role").(string)).(contract.OrderedObject)
		if !ok || get(expected, "expectation") != execution.Pair {
			return refused("worker_policy_role_unsupported")
		}
		if get(item, "model") != get(expected, "model") || get(item, "reasoningEffort") != get(expected, "reasoningEffort") {
			return refused("worker_policy_pair_mismatch")
		}
	}
	return contract.OrderedObject{{Key: "ready", Value: true}, {Key: "reason", Value: nil}, {Key: "digest", Value: caller.digest}}
}

// readWorkerPolicy is RelayService.read_worker_policy up to the checks this build can make
// against a record another installation wrote. Every answer is an absence reason or an
// observation; nothing is repaired.
func readWorkerPolicy(services Services, loc store.Location) contract.OrderedObject {
	absent := func(reason string) contract.OrderedObject {
		return contract.OrderedObject{{Key: "observed", Value: false}, {Key: "reason", Value: reason}, {Key: "policy", Value: nil}}
	}
	record := readJSONFile(filepath.Join(services.Selection.Path, "daemon.json"))
	file, err := os.Open(filepath.Join(services.Selection.Path, "worker-policy.json"))
	if err != nil {
		return absent("worker_policy_unreadable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65536+1))
	_ = file.Close()
	if err != nil || len(raw) > 65536 {
		return absent("worker_policy_unreadable")
	}
	receipt, err := decodeJSON(raw)
	if err != nil {
		return absent("worker_policy_unreadable")
	}
	recordObject, recordOK := record.(contract.OrderedObject)
	receiptObject, receiptOK := receipt.(contract.OrderedObject)
	if !recordOK || !receiptOK {
		return absent("worker_policy_unreadable")
	}
	if version, ok := pyInt(get(receiptObject, "schemaVersion")); !ok || version != 1 {
		return absent("worker_policy_version_unknown")
	}
	worker, workerOK := get(receiptObject, "worker").(contract.OrderedObject)
	run, runOK := get(receiptObject, "service").(contract.OrderedObject)
	if !workerOK || !runOK {
		return absent("worker_policy_unreadable")
	}
	if _, ok := get(receiptObject, "policy").(contract.OrderedObject); !ok {
		return absent("worker_policy_unreadable")
	}
	pid, pidOK := pyInt(get(recordObject, "pid"))
	ticks, ticksOK := pyInt(get(recordObject, "startTicks"))
	workerPid := get(recordObject, "workerPid")
	if !pidOK || pid <= 0 || !ticksOK || ticks < 0 {
		return absent("worker_policy_process_mismatch")
	}
	if workerPid != nil {
		if value, ok := pyInt(workerPid); !ok || value <= 0 {
			return absent("worker_policy_process_mismatch")
		}
	}
	expectedPid, expectedTicks := get(recordObject, "pid"), get(recordObject, "startTicks")
	if truthy(workerPid) {
		expectedPid, expectedTicks = workerPid, get(recordObject, "workerStartTicks")
	}
	ep, epOK := pyInt(expectedPid)
	et, etOK := pyInt(expectedTicks)
	wp, wpOK := pyInt(get(worker, "pid"))
	wt, wtOK := pyInt(get(worker, "startTicks"))
	if !epOK || ep <= 0 || !etOK || et < 0 || !wpOK || !wtOK || wp != ep || wt != et {
		return absent("worker_policy_process_mismatch")
	}
	boot := get(recordObject, "bootId")
	if !truthy(boot) || !pyEqual(get(worker, "bootId"), boot) || !pyEqual(boot, nullableText(bootID())) {
		return absent("worker_policy_boot_mismatch")
	}
	identity := contract.OrderedObject{}
	for _, key := range []string{"token", "pid", "startTicks", "installationId", "stateDir", "socketPath", "storeId", "scopeRoot", "scopeAuthority"} {
		identity = append(identity, contract.Field{Key: key, Value: get(recordObject, key)})
	}
	var info syscall.Stat_t
	if err := syscall.Stat(services.Selection.DBPath(), &info); err != nil {
		return absent("worker_policy_unreadable")
	}
	identity = append(identity, contract.Field{Key: "dbDevice", Value: int64(info.Dev)}, contract.Field{Key: "dbInode", Value: int64(info.Ino)})
	root, authority := scopeRoot()
	if !pyEqual(run, identity) || !truthy(get(recordObject, "token")) || loc.StoreID == "" ||
		!pyEqual(get(recordObject, "storeId"), loc.StoreID) ||
		!pyEqual(get(recordObject, "installationId"), installationID(services.Selection.Path)) ||
		!pyEqual(get(recordObject, "stateDir"), services.Selection.Path) ||
		!pyEqual(get(recordObject, "socketPath"), nullableText(services.SocketPath)) ||
		!pyEqual(get(recordObject, "scopeRoot"), root) ||
		!pyEqual(get(recordObject, "scopeAuthority"), authority) {
		return absent("worker_policy_service_mismatch")
	}
	// A receipt that matches THIS installation's identity was published by a Go worker, and the
	// Go daemon is todo 29. Until it exists no such receipt can be current.
	return absent("worker_policy_process_unavailable")
}

func readJSONFile(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil
	}
	return value
}

func bootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// scopeRoot is resolve_scope_root: an override marks the registry isolated.
func scopeRoot() (string, string) {
	if override := os.Getenv("CODEX_SESSION_RELAY_SCOPE_DIR"); override != "" {
		expanded, err := store.ExpandUser(override)
		if err != nil {
			expanded = override
		}
		if !filepath.IsAbs(expanded) {
			cwd, _ := os.Getwd()
			expanded = cwd + "/" + expanded
		}
		return pathlibSpelling(expanded), "isolated"
	}
	home := ""
	if current, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil {
		home = current.HomeDir
	}
	return filepath.Join(home, ".codex-session-relay", "scopes"), "production"
}

// installationID is installation_id: this installation (the running executable's directory,
// where Python uses its package directory) plus this state directory.
func installationID(stateDir string) string {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		executable = ""
	}
	resolved, err := store.ResolvePath(stateDir)
	if err != nil {
		resolved = stateDir
	}
	var material bytes.Buffer
	material.WriteString(filepath.Dir(executable))
	material.WriteByte(0)
	material.WriteString(resolved)
	sum := sha256.Sum256(material.Bytes())
	return hex.EncodeToString(sum[:])[:16]
}
