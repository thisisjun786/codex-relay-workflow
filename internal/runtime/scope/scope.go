// Package scope is scripts/crw_runtime/scope.py: the operating scope, read from the relay
// rather than rediscovered. One relay service and one store serve a whole scope (one host, one
// OS user, one App Server), so nothing here creates a daemon or a store.
//
// Every relay reading is a SUBPROCESS of the relay executable the caller selected - the Python
// console script until the cutover, the Go binary after it - because which store a selection
// resolves to is that relay's rule. The relay's doctor constructs no store.
package scope

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// StateEnv is the relay's state-directory override variable.
const StateEnv = "CODEX_SESSION_RELAY_STATE"

// The daemon's two established answers; reading.AccessError and reading.Unreadable are the
// other two.
const (
	Running = "RUNNING"
	Stopped = "STOPPED"
)

// Object is a decoded JSON object.
type Object = record.Object

// Env is an environment as a list of KEY=VALUE, the way exec takes it.
type Env []string

// Get is the value of key, or "".
func (e Env) Get(key string) string {
	for i := len(e) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(e[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// With is e with key set to value (and every earlier spelling of key removed).
func (e Env) With(key, value string) Env {
	return append(e.Without(key), key+"="+value)
}

// Without is e with key removed.
func (e Env) Without(key string) Env {
	out := Env{}
	for _, kv := range e {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return out
}

// ServiceState is scope.service_state: classify a service status reading, the invocation first.
// Being unable to ask is never being told no.
func ServiceState(envelope Object) Object {
	if envelope == nil || !evidence.Truthy(record.Get(envelope, "ok")) {
		detail := firstStated(envelope, "unreadable", "stderr")
		if detail == nil {
			detail = "the command failed"
		}
		return Object{{Key: "state", Value: reading.AccessError}, {Key: "running", Value: nil},
			{Key: "detail", Value: "the service could not be asked: " + PyStr(detail)}}
	}
	payload, ok := record.Get(envelope, "payload").(Object)
	if !ok {
		return Object{{Key: "state", Value: reading.Unreadable}, {Key: "running", Value: nil},
			{Key: "detail", Value: "the service answered with no readable status object"}}
	}
	held, ok := record.Get(payload, "running").(bool)
	if !ok {
		return Object{{Key: "state", Value: reading.Unreadable}, {Key: "running", Value: nil},
			{Key: "detail", Value: "the status carries no boolean 'running', found " + TypeName(record.Get(payload, "running"))}}
	}
	state, words := Stopped, "not running"
	if held {
		state, words = Running, "running"
	}
	return Object{{Key: "state", Value: state}, {Key: "running", Value: held},
		{Key: "detail", Value: "the service answered and reports itself " + words}}
}

// firstStated is `a or b` over two envelope keys: the first truthy value, else nil.
func firstStated(o Object, keys ...string) any {
	for _, key := range keys {
		if v := record.Get(o, key); evidence.Truthy(v) {
			return v
		}
	}
	return nil
}

// PyStr is str() of a decoded JSON value.
func PyStr(v any) string {
	switch value := v.(type) {
	case nil:
		return "None"
	case string:
		return value
	case bool:
		if value {
			return "True"
		}
		return "False"
	case float64:
		return evidence.Float(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case int:
		return strconv.Itoa(value)
	}
	return evidence.Dumps(v, false, false, false)
}

// TypeName is type(value).__name__ of a decoded JSON value.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		return "float"
	case Object:
		return "dict"
	case []any:
		return "list"
	}
	return "int"
}

// Timeout is scope.relay's default per-command budget.
var Timeout = 60 * time.Second

// WaitDelay bounds how long a command's output is still waited for once the command has exited
// or its deadline has killed it. A process it started that inherited its stdout or stderr holds
// the pipes open for as long as it lives, and without this bound Wait waits for it too, past any
// deadline. Python's subprocess.run kills and then waits for the process alone at its timeout.
var WaitDelay = 5 * time.Second

// Relay is scope.relay: run one relay command and return its parsed JSON with the invocation
// recorded. With discovery, CODEX_SESSION_RELAY_STATE is removed and no --state passed; with a
// state, both selectors are set together (OPS-3.3).
//
// A command the deadline ended is subprocess.run's TimeoutExpired: no exit status and nothing it
// printed kept as its answer. A command that exited while a process it left behind kept its
// output open past WaitDelay is not read either: what it printed may be incomplete, and Python,
// which keeps reading until the deadline, would have waited for that process.
func Relay(ctx context.Context, command []string, executable, socket, state string, env Env, discovery bool, timeout time.Duration) Object {
	if timeout == 0 {
		timeout = Timeout
	}
	argv := []string{executable}
	if socket != "" {
		argv = append(argv, "--socket", socket)
	}
	if discovery {
		env = env.Without(StateEnv)
	} else if state != "" {
		argv = append(argv, "--state", state)
		env = env.With(StateEnv, state)
	}
	argv = append(argv, command...)
	commandValue := strings2any(argv)
	unreadable := func(said string) Object {
		return Object{{Key: "ok", Value: false}, {Key: "command", Value: commandValue}, {Key: "unreadable", Value: said}}
	}
	run, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(run, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.WaitDelay = WaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err != nil && ctx.Err() != nil:
		return unreadable("the relay command was stopped before it finished: " + ctx.Err().Error())
	case err != nil && run.Err() != nil:
		return unreadable("TimeoutExpired: Command '" + evidence.Repr(commandValue) + "' timed out after " + seconds(timeout) + " seconds")
	case errors.Is(err, exec.ErrWaitDelay):
		return unreadable("the relay exited, but a process it left behind kept its output open " + WaitDelay.String() + " past that, so what it printed is not read")
	case err != nil && !errors.As(err, &exit):
		return unreadable(store.PythonOSError(err))
	}
	code := returnCode(cmd.ProcessState)
	var payload any
	if strings.TrimSpace(stdout.String()) != "" {
		if value, err := reading.Decode(stdout.Bytes()); err == nil {
			payload = value
		}
	}
	errText := strings.TrimSpace(stderr.String())
	if len(errText) > 2000 {
		errText = errText[:2000]
	}
	var unread any
	if payload == nil {
		unread = "the relay did not return JSON"
	}
	return Object{
		{Key: "ok", Value: code == 0},
		{Key: "exitCode", Value: int64(code)},
		{Key: "command", Value: commandValue},
		{Key: "payload", Value: payload},
		{Key: "stderr", Value: nullable(errText)},
		{Key: "unreadable", Value: unread},
	}
}

// returnCode is Popen.returncode: the exit status, or -N for a process signal N ended.
func returnCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return state.ExitCode()
}

// seconds is str() of a timeout in seconds as Python's callers spell it: an int when whole.
func seconds(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10)
	}
	return evidence.Float(d.Seconds())
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strings2any(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// DefaultStateRoot is scope.default_state_root.
func DefaultStateRoot(env Env) string {
	return filepath.Join(record.StateHome(env.Get), "codex-session-relay")
}

// Survey is scope.survey: three doctor readings, because one call cannot answer all three
// questions (discovery, the selected store, a database at the root of the state home).
func Survey(ctx context.Context, executable, socket, state string, env Env) Object {
	root := DefaultStateRoot(env)
	selection, via := state, any(nil)
	if state != "" {
		via = "flag"
	} else if env.Get(StateEnv) != "" {
		selection, via = env.Get(StateEnv), "environment"
	}
	readings := Object{{Key: "discovery", Value: Relay(ctx, []string{"doctor"}, executable, socket, "", env, true, 0)}}
	if selection != "" {
		readings = record.Set(readings, "selected", Relay(ctx, []string{"doctor"}, executable, socket, selection, env, false, 0))
	} else {
		readings = record.Set(readings, "selected", Object{{Key: "ok", Value: false}, {Key: "skipped", Value: "no store is explicitly selected: neither --state nor " + StateEnv + " is set"}})
	}
	readings = record.Set(readings, "selectedVia", via)
	if info, err := os.Stat(filepath.Join(root, "relay.sqlite3")); err == nil && info.Mode().IsRegular() {
		readings = record.Set(readings, "rootCandidate", Relay(ctx, []string{"doctor"}, executable, socket, root, env, false, 0))
	} else {
		readings = record.Set(readings, "rootCandidate", Object{{Key: "skipped", Value: "no relay.sqlite3 at " + root}})
	}
	return readings
}

func usable(readings Object, name string) Object {
	answer, _ := record.Get(readings, name).(Object)
	if !evidence.Truthy(record.Get(answer, "ok")) {
		return Object{}
	}
	payload, _ := record.Get(answer, "payload").(Object)
	if payload == nil {
		return Object{}
	}
	return payload
}

// StateDirectory is scope.state_directory across the shapes different builds emit.
func StateDirectory(payload Object) any {
	if v := record.Get(payload, "stateDirectory"); evidence.Truthy(v) {
		return v
	}
	selection, _ := record.Get(payload, "stateSelection").(Object)
	return record.Get(selection, "path")
}

// ScopeID is scope.scope_id.
func ScopeID(payload Object) any {
	selection, _ := record.Get(payload, "stateSelection").(Object)
	return record.Get(selection, "socketScope")
}

// SiblingReading is scope.sibling_reading: "not reported" is not "none found".
func SiblingReading(payload Object) string {
	siblings, has := record.Lookup(payload, "siblingStores")
	if !has || siblings == nil {
		return "not reported: this relay build does not emit siblingStores, so the conflict inventory could not be read from it"
	}
	o, ok := siblings.(Object)
	if !ok {
		// Python's sibling_reading raises on anything but an object; nothing in it was read.
		return "not readable: siblingStores is a " + TypeName(siblings) + ", not an object, so the conflict inventory could not be read from it"
	}
	if record.Get(o, "checked") == false {
		return "not checked: " + PyStr(record.Get(o, "reason"))
	}
	return "checked"
}

// StorePatterns are where a store can sit and what a store file is called.
var StorePatterns = [][2]string{
	{"relay.sqlite3", "relay store"},
	{"operations-*.sqlite3", "adapter operations ledger, selected differently from the store (OPS-3.3)"},
}

// FilesystemCandidates is scope.filesystem_candidates: every relay database and operations
// ledger visible on disk, listed and not interpreted.
func FilesystemCandidates(env Env) []Object {
	root := DefaultStateRoot(env)
	var found []Object
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return found
	}
	places := []string{root}
	if entries, err := os.ReadDir(root); err == nil {
		var dirs []string
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name())
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				dirs = append(dirs, path)
			}
		}
		sort.Strings(dirs)
		places = append(places, dirs...)
	}
	for _, place := range places {
		where := "in a scope directory"
		if place == root {
			where = "at the root of the state home"
		}
		for _, pattern := range StorePatterns {
			matches, err := filepath.Glob(filepath.Join(globEscape(place), pattern[0]))
			if err != nil {
				continue
			}
			sort.Strings(matches)
			for _, database := range matches {
				if info, err := os.Stat(database); err == nil && info.Mode().IsRegular() {
					found = append(found, Object{{Key: "path", Value: place}, {Key: "database", Value: database}, {Key: "kind", Value: pattern[1]}, {Key: "foundBy", Value: "listed on disk " + where}})
				}
			}
		}
	}
	return found
}

func globEscape(path string) string {
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// StoresSeen is scope.stores_seen: every store this survey saw, with how it was found. None
// is adopted.
func StoresSeen(readings Object, env Env) []any {
	root := DefaultStateRoot(env)
	var seen []Object
	discovery := usable(readings, "discovery")
	siblings, _ := record.Get(discovery, "siblingStores").(Object)
	selected := usable(readings, "selected")
	for _, one := range []struct {
		payload Object
		how     string
	}{{discovery, "discovery"}, {selected, "explicit --state"}} {
		if path := StateDirectory(one.payload); evidence.Truthy(path) {
			seen = append(seen, Object{{Key: "path", Value: path}, {Key: "foundBy", Value: one.how}})
		}
	}
	for _, key := range []struct{ name, how string }{{"withoutProvenance", "discovery: records no socket"}, {"claimingThisSocket", "discovery: claims this socket"}} {
		list, _ := record.Get(siblings, key.name).([]any)
		for _, path := range list {
			seen = append(seen, Object{{Key: "path", Value: path}, {Key: "foundBy", Value: key.how}})
		}
	}
	if len(usable(readings, "rootCandidate")) > 0 {
		seen = append(seen, Object{{Key: "path", Value: root}, {Key: "foundBy", Value: "targeted: a database at the root of the state home, which discovery never enumerates"}})
	}
	for _, candidate := range FilesystemCandidates(env) {
		seen = append(seen, Object{{Key: "path", Value: record.Get(candidate, "path")}, {Key: "database", Value: record.Get(candidate, "database")}, {Key: "kind", Value: record.Get(candidate, "kind")}, {Key: "foundBy", Value: record.Get(candidate, "foundBy")}})
	}
	var unique []Object
	for _, entry := range seen {
		match := -1
		for i, u := range unique {
			// A relay's doctor payload may carry any JSON value as a path, and Python compares
			// lists and objects by value where Go's == would panic on them.
			if evidence.Equal(record.Get(u, "path"), record.Get(entry, "path")) && evidence.Equal(record.Get(u, "database"), record.Get(entry, "database")) {
				match = i
				break
			}
		}
		if match < 0 {
			unique = append(unique, entry)
			continue
		}
		how, _ := record.Get(entry, "foundBy").(string)
		have, _ := record.Get(unique[match], "foundBy").(string)
		if !strings.Contains(have, how) {
			unique[match] = record.Set(unique[match], "foundBy", have+"; "+how)
		}
		for _, key := range []string{"database", "kind"} {
			if _, has := record.Lookup(unique[match], key); !has {
				unique[match] = record.Set(unique[match], key, record.Get(entry, key))
			}
		}
	}
	out := make([]any, len(unique))
	for i, u := range unique {
		out[i] = u
	}
	return out
}

// Summarise is scope.summarise without the Python-only assignment lookup: what the relay
// reported, kept honest about what was not checked.
func Summarise(readings Object, env Env, service any) Object {
	discovery := usable(readings, "discovery")
	selected := usable(readings, "selected")
	attempt, _ := record.Get(readings, "selected").(Object)
	attempted := len(attempt) > 0 && !evidence.Truthy(record.Get(attempt, "skipped"))
	var primary Object
	var answeredBy string
	answering := ""
	switch {
	case !attempted:
		primary, answeredBy, answering = discovery, "discovery, because no store is explicitly selected", "discovery"
	case len(selected) > 0:
		primary, answeredBy, answering = selected, "the explicitly selected store", "selected"
	default:
		primary, answeredBy = Object{}, "nothing: a store is explicitly selected and its doctor did not answer, and the discovered store is a different store. Every field below that describes an operating scope is therefore empty rather than borrowed"
	}
	siblings, _ := record.Get(discovery, "siblingStores").(Object)
	reachability, _ := record.Get(primary, "actorReachability").(Object)
	contents, _ := record.Get(primary, "contents").(Object)
	storeInfo, _ := record.Get(primary, "store").(Object)
	var scopeCommand any
	if answering != "" {
		reading, _ := record.Get(readings, answering).(Object)
		scopeCommand = record.Get(reading, "command")
	}
	databasePath := record.Get(storeInfo, "dbPath")
	if !evidence.Truthy(databasePath) {
		databasePath = record.Get(storeInfo, "realPath")
	}
	relationships, has := record.Lookup(contents, "relationships")
	if !has {
		relationships = record.Get(primary, "relationships")
	}
	return Object{
		{Key: "scopeId", Value: ScopeID(primary)},
		{Key: "scopeAnsweredBy", Value: answeredBy},
		{Key: "scopeCommand", Value: scopeCommand},
		{Key: "scopeMeaning", Value: "one host, one OS user, one App Server (OPS-3.1). Parents in different repositories and different Linear projects share this one scope."},
		{Key: "stateDirectory", Value: StateDirectory(primary)},
		{Key: "databasePath", Value: databasePath},
		{Key: "storeId", Value: record.Get(storeInfo, "storeId")},
		{Key: "ledger", Value: record.Get(primary, "ledger")},
		{Key: "socketConnect", Value: record.Get(reachability, "socketConnect")},
		{Key: "stateDirectoryWritable", Value: record.Get(reachability, "stateDirectoryWritable")},
		{Key: "serviceOwner", Value: service},
		{Key: "storesSeen", Value: StoresSeen(readings, env)},
		{Key: "siblingDiscovery", Value: SiblingReading(discovery)},
		{Key: "ambiguous", Value: record.Get(siblings, "ambiguous")},
		{Key: "relationships", Value: relationships},
		{Key: "relationshipsMeaning", Value: "a count only. OPS-3.4 proof is assignment-find --issue returning the expected relationship from each participating process: a nonzero count from a different populated database would satisfy a count check while proving nothing"},
		{Key: "perProjectDaemon", Value: "none created. One service and one store serve the whole operating scope (OPS-3.1), so a second repository or project reuses them rather than starting its own"},
	}
}
