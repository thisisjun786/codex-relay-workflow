package adapter

// subset ported for todo 28; todo 29 owns and extends service.read_worker_policy.
// Selectors are injected by the CLI; this reader never resolves host state from env.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

type WorkerObservation struct{ State, Socket, Scope, Authority, Installation string }

func readObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 65536 {
		return nil, errors.New("worker record exceeds byte limit")
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	err = decoder.Decode(&object)
	return object, err
}
func integer(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil
}
func processIdentity(pid int64) (int64, string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", err
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return 0, "", errors.New("process stat has no comm boundary")
	}
	parts := strings.Fields(string(raw)[end+2:])
	if len(parts) < 20 {
		return 0, "", errors.New("process stat is incomplete")
	}
	ticks, err := strconv.ParseInt(parts[19], 10, 64)
	return ticks, parts[0], err
}

// heldLock is service.py _existing_lock_held: contention alone is evidence that another open
// description holds path's flock, and a lock that cannot be opened, examined or tried for any
// other reason (a missing file included, which is never created) is an error the reader
// answers worker_policy_unreadable for, never "unheld".
func heldLock(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return false, err
	}
	if before.IsDir() {
		return false, &os.PathError{Op: "open", Path: path, Err: syscall.EISDIR}
	}
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return false, unix.Flock(int(file.Fd()), unix.LOCK_UN)
	}
	if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) {
		return false, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return os.SameFile(before, after), nil
}
func (o WorkerObservation) Read(ctx context.Context) (map[string]any, string) {
	record, err := readObject(filepath.Join(o.State, "daemon.json"))
	if err != nil {
		return nil, "worker_policy_unreadable"
	}
	receipt, err := readObject(filepath.Join(o.State, "worker-policy.json"))
	if err != nil {
		return nil, "worker_policy_unreadable"
	}
	if version, ok := integer(receipt["schemaVersion"]); !ok || version != 1 {
		return nil, "worker_policy_version_unknown"
	}
	worker, wok := receipt["worker"].(map[string]any)
	run, rok := receipt["service"].(map[string]any)
	policy, pok := receipt["policy"].(map[string]any)
	if !wok || !rok || !pok {
		return nil, "worker_policy_unreadable"
	}
	pid, pidOK := integer(record["pid"])
	ticks, ticksOK := integer(record["startTicks"])
	if !pidOK || pid <= 0 || !ticksOK || ticks < 0 {
		return nil, "worker_policy_process_mismatch"
	}
	if record["workerPid"] != nil {
		var ok bool
		pid, ok = integer(record["workerPid"])
		if !ok || pid <= 0 {
			return nil, "worker_policy_process_mismatch"
		}
		ticks, ticksOK = integer(record["workerStartTicks"])
	}
	wp, wpok := integer(worker["pid"])
	wt, wtok := integer(worker["startTicks"])
	if !ticksOK || ticks < 0 || !wpok || !wtok || wp != pid || wt != ticks {
		return nil, "worker_policy_process_mismatch"
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, "worker_policy_boot_mismatch"
	}
	if text(record["bootId"]) == "" || worker["bootId"] != record["bootId"] || record["bootId"] != strings.TrimSpace(string(boot)) {
		return nil, "worker_policy_boot_mismatch"
	}
	dbpath := filepath.Join(o.State, "relay.sqlite3")
	info, err := os.Stat(dbpath)
	if err != nil {
		return nil, "worker_policy_unreadable"
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity := map[string]any{}
	for _, key := range []string{"token", "pid", "startTicks", "installationId", "stateDir", "socketPath", "storeId", "scopeRoot", "scopeAuthority"} {
		identity[key] = record[key]
	}
	identity["dbDevice"] = json.Number(fmt.Sprint(stat.Dev))
	identity["dbInode"] = json.Number(fmt.Sprint(stat.Ino))
	selected, err := store.ResolvePath(o.State)
	if err != nil {
		return nil, "worker_policy_unreadable"
	}
	installation := sha256.Sum256([]byte(o.Installation + "\x00" + selected))
	currentID := ""
	selection := store.StateSelection{Path: o.State}
	read := store.ReadOnlyRows(ctx, selection, "SELECT value FROM schema_meta WHERE key='store_id'", nil, func(row store.RowScanner) error { return row.Scan(&currentID) })
	if !read.Readable || !reflect.DeepEqual(run, identity) || text(record["token"]) == "" || currentID == "" || record["storeId"] != currentID || record["installationId"] != fmt.Sprintf("%x", installation[:8]) || record["stateDir"] != o.State || record["socketPath"] != o.Socket || record["scopeRoot"] != o.Scope || record["scopeAuthority"] != o.Authority {
		return nil, "worker_policy_service_mismatch"
	}
	actualTicks, state, err := processIdentity(pid)
	if err != nil || actualTicks != ticks || strings.ContainsAny(state, "TtZXx") {
		return nil, "worker_policy_process_unavailable"
	}
	canonical, err := store.CanonicalSocket(o.Socket)
	if err != nil {
		return nil, "worker_policy_unreadable"
	}
	socketHash := sha256.Sum256([]byte(canonical))
	key := fmt.Sprintf("%x", socketHash[:8])
	if o.Authority != "production" {
		salt := sha256.Sum256([]byte(o.Scope))
		key = fmt.Sprintf("isolated-%x-%s", salt[:4], key)
	}
	scopePath := filepath.Join(o.Scope, key+".json")
	scope, err := readObject(scopePath)
	if err != nil {
		return nil, "worker_policy_scope_mismatch"
	}
	for _, key := range []string{"token", "pid", "startTicks", "bootId", "storeId", "installationId", "stateDir", "socketPath"} {
		if !reflect.DeepEqual(scope[key], record[key]) {
			return nil, "worker_policy_scope_mismatch"
		}
	}
	for _, lock := range []string{filepath.Join(o.State, "daemon.lock"), filepath.Join(o.Scope, key+".lock")} {
		held, err := heldLock(lock)
		if err != nil {
			return nil, "worker_policy_unreadable"
		}
		if !held {
			return nil, "worker_policy_lock_unheld"
		}
	}
	after, err := readObject(filepath.Join(o.State, "daemon.json"))
	if err != nil || !reflect.DeepEqual(record, after) {
		return nil, "worker_policy_observation_changed"
	}
	next, err := os.Stat(dbpath)
	if err != nil || !os.SameFile(info, next) {
		return nil, "worker_policy_observation_changed"
	}
	scopeAfter, err := readObject(scopePath)
	if err != nil || !reflect.DeepEqual(scope, scopeAfter) {
		return nil, "worker_policy_observation_changed"
	}
	nextTicks, nextState, err := processIdentity(pid)
	if err != nil || nextTicks != ticks || strings.ContainsAny(nextState, "TtZXx") {
		return nil, "worker_policy_observation_changed"
	}
	return policy, ""
}
func (o WorkerObservation) Ready(ctx context.Context, request map[string]any, policy registry.RolePolicy) (string, error) {
	worker, reason := o.Read(ctx)
	if reason != "" {
		return reason, nil
	}
	if worker["state"] != "declared" {
		return "worker_policy_unconfigured", nil
	}
	if !policy.Declared {
		return "caller_policy_unconfigured", nil
	}
	if worker["digest"] != policy.Digest() {
		return "worker_policy_digest_mismatch", nil
	}
	if dumps(ordered(worker), true) != dumps(policy.Summary(), true) {
		return "worker_policy_summary_mismatch", nil
	}
	roles, _ := worker["roles"].(map[string]any)
	for _, role := range []string{"parent", "child"} {
		endpoint, _ := request[role].(map[string]any)
		settings, _ := endpoint["settings"].(map[string]any)
		expected, _ := roles[role].(map[string]any)
		if expected["expectation"] != "pair" {
			return "worker_policy_role_unsupported", nil
		}
		if settings["model"] != expected["model"] || settings["reasoningEffort"] != expected["reasoningEffort"] {
			return "worker_policy_pair_mismatch", nil
		}
		finding := registry.CheckRecord(ordered(settings).(contract.OrderedObject), role, policy)
		if finding != nil {
			return text(field(finding, "code")), nil
		}
	}
	return "", nil
}
