package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
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

// declaredPolicy is rolepolicy.declared for one configured value: registry.ResolveRolePolicy,
// the relay's one reading of the policy file, taken apart into what doctor reports.
func declaredPolicy(configured string) rolePolicy {
	policy := registry.ResolveRolePolicy(map[string]string{policyVariable: configured})
	if !policy.Declared {
		return rolePolicy{detail: policy.Detail, publicDetail: policy.PublicDetail}
	}
	roles, _ := get(policy.Summary(), "roles").(contract.OrderedObject)
	return rolePolicy{digest: policy.Digest(), roles: roles}
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
			if !ok || pyvalue.Strip(text) == "" {
				return refused("worker_policy_requirements_invalid")
			}
		}
		if role := get(object, "role"); role != "parent" && role != "child" {
			return refused("worker_policy_role_unsupported")
		}
	}
	if !pyvalue.Truthy(get(observation, "observed")) {
		reason := get(observation, "reason")
		if !pyvalue.Truthy(reason) {
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
func readWorkerPolicy(services dispatch.Services, loc store.Location) contract.OrderedObject {
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
	if pyvalue.Truthy(workerPid) {
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
	if !pyvalue.Truthy(boot) || !pyEqual(get(worker, "bootId"), boot) || !pyEqual(boot, service.BootID()) {
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
	if !pyEqual(run, identity) || !pyvalue.Truthy(get(recordObject, "token")) || loc.StoreID == "" ||
		!pyEqual(get(recordObject, "storeId"), loc.StoreID) ||
		!pyEqual(get(recordObject, "installationId"), installationID(services.Selection.Path)) ||
		!pyEqual(get(recordObject, "stateDir"), services.Selection.Path) ||
		!pyEqual(get(recordObject, "socketPath"), nullableText(services.SocketPath)) ||
		!pyEqual(get(recordObject, "scopeRoot"), root) ||
		!pyEqual(get(recordObject, "scopeAuthority"), authority) {
		return absent("worker_policy_service_mismatch")
	}
	owner := &service.Service{Selection: services.Selection, Socket: services.SocketPath,
		StoreID: loc.StoreID, InstallationID: installationID(services.Selection.Path),
		Scope: &service.ScopeRegistry{Root: root, Authority: authority}}
	return owner.ReadWorkerPolicy(context.Background())
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

// scopeRoot is resolve_scope_root: an override marks the registry isolated. It is the root the
// owner resolves (service.ResolveScope), so the worker policy's recorded scopeRoot is compared
// with the same spelling: Path(override).expanduser().absolute() against the working directory
// the kernel names, or the passwd home's production root. A root nothing answers is "", which no
// record names.
func scopeRoot() (string, string) {
	scope, err := service.ResolveScope()
	if err != nil {
		return "", ""
	}
	return scope.Root, scope.Authority
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
