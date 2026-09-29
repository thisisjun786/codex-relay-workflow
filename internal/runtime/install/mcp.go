package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// The plugin bridge record (scripts/crw_runtime/bridgerecord.py): what the packaged launcher
// reads to start the bridge a plugin declares, and the only place the host's execution policy
// can reach a bridge Codex starts with a bare environment.
const (
	BridgeRecordName    = "crw-bridge-mcp.json"
	OwnershipLockName   = "crw-mcp-ownership"
	BridgeRecordVersion = 1
	PolicyRecordVersion = 2
	OwnerPlugin         = "plugin"
	OwnerUser           = "user"
	ServerName          = definition.Bridge
)

// Outcomes of register-mcp (bridgerecord.*, runtime_install.py's register-mcp refusals).
const (
	RecordAbsent            = "record_absent"
	RecordMalformed         = "record_malformed"
	RecordUnchanged         = "record_unchanged"
	RecordDiffers           = "record_differs"
	RecordWouldCreate       = "record_would_create"
	RecordCreated           = "record_created"
	RecordChangedUnderneath = "record_changed_underneath"
	RecordAppliedUnverified = "record_applied_unverified"
	RecordPolicyChanged     = "record_policy_changed"
	Conflict                = "CONFLICT"
	Busy                    = "BUSY"
	PolicyUnreadable        = "execution_policy_unreadable"
)

var recordSettled = map[string]bool{RecordUnchanged: true, RecordCreated: true, RecordWouldCreate: true}

var policyDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BridgeDocument is bridgerecord.document for the plugin owner: built once so the writer and the
// launcher cannot disagree about its shape. policy is nil or {path, digest}.
func BridgeDocument(command string, args []string, name, issue string, policy Object) Object {
	var nameValue, issueValue any
	if name != "" {
		nameValue = name
	}
	if issue != "" {
		issueValue = issue
	}
	document := Object{field("recordVersion", int64(BridgeRecordVersion)), field("owner", OwnerPlugin), field("serverName", nameValue),
		field("bridgeExecutable", command), field("args", strs(args)), field("installedBy", issueValue)}
	if policy != nil {
		document = record.Set(document, "recordVersion", int64(PolicyRecordVersion))
		document = append(document, field("executionPolicy", Object{field("digest", record.Get(policy, "digest")), field("path", record.Get(policy, "path"))}))
	}
	return document
}

// pyStrip is str.strip().
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// policyPathComplaints is bridgerecord.policy_path_complaints.
func policyPathComplaints(path any) []string {
	text, ok := path.(string)
	if !ok || text == "" {
		return []string{"the execution policy path must be a non-empty string"}
	}
	var wrong []string
	if text != pyStrip(text) {
		wrong = append(wrong, "the execution policy path "+evidence.Repr(text)+" has leading or trailing whitespace, which the bridge would strip and so open a different file")
	}
	for _, r := range text {
		if r < 32 || r == 127 {
			wrong = append(wrong, "the execution policy path "+evidence.Repr(text)+" contains a control character")
			break
		}
	}
	if !filepath.IsAbs(text) {
		wrong = append(wrong, "the execution policy path "+evidence.Repr(text)+" must be absolute, because the packaged launcher runs from the installed package directory")
	}
	return wrong
}

// policyComplaints is bridgerecord.policy_complaints.
func policyComplaints(reference any) []string {
	o, ok := reference.(Object)
	if !ok {
		return []string{"executionPolicy must be an object naming path and digest"}
	}
	var keys []string
	for _, f := range o {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "digest,path" {
		return []string{"executionPolicy must have exactly the keys digest, path, found " + strings.Join(keys, ", ")}
	}
	wrong := policyPathComplaints(record.Get(o, "path"))
	if digest, ok := record.Get(o, "digest").(string); !ok || !policyDigest.MatchString(digest) {
		wrong = append(wrong, "executionPolicy digest must be 64 lowercase hexadecimal characters")
	}
	return wrong
}

// policyFileComplaints is bridgerecord.policy_file_complaints: the launcher's own two questions,
// asked before a record naming the policy is written or confirmed.
func policyFileComplaints(reference any) []string {
	if wrong := policyComplaints(reference); len(wrong) > 0 {
		return wrong
	}
	o := reference.(Object)
	path, recorded := record.Get(o, "path").(string), record.Get(o, "digest").(string)
	// The launcher opens os.fsencode(path): a byte the path held that is not UTF-8 is recorded
	// as its surrogate escape, and opened as that byte again.
	name, encodable := reading.FSEncode(path)
	if !encodable {
		return []string{"the execution policy path " + evidence.Repr(path) + " names a surrogate os.fsencode refuses, so no launcher can open it"}
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return []string{"the execution policy " + path + " could not be opened (" + store.PythonOSError(err) + ")"}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return []string{"the execution policy " + path + " is not a regular file"}
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return []string{"the execution policy " + path + " could not be read (" + store.PythonOSError(err) + ")"}
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != recorded {
		return []string{"the execution policy " + path + " now hashes to " + actual + ", not the recorded " + recorded}
	}
	return nil
}

// bridgeComplaints is bridgerecord.complaints.
func bridgeComplaints(found any) []string {
	o, ok := found.(Object)
	if !ok {
		return []string{"the record is a " + evidence.TypeName(found) + ", not an object"}
	}
	var wrong []string
	version := record.Get(o, "recordVersion")
	if version != int64(BridgeRecordVersion) && version != int64(PolicyRecordVersion) {
		wrong = append(wrong, "recordVersion must be one of 1, 2, found "+evidence.Repr(version))
	}
	owner := record.Get(o, "owner")
	if owner != OwnerUser && owner != OwnerPlugin {
		wrong = append(wrong, "owner must be one of user, plugin, found "+evidence.Repr(owner))
	}
	executable, ok := record.Get(o, "bridgeExecutable").(string)
	switch {
	case !ok || strings.TrimSpace(executable) == "":
		wrong = append(wrong, "bridgeExecutable must be a non-empty string")
	case owner == OwnerPlugin && !filepath.IsAbs(executable):
		wrong = append(wrong, "bridgeExecutable must be an absolute path when owner is plugin")
	}
	if args, has := record.Lookup(o, "args"); has && args != nil {
		list, ok := args.([]any)
		for _, word := range list {
			if _, isString := word.(string); !isString {
				ok = false
			}
		}
		if !ok {
			wrong = append(wrong, "args must be a list of strings when it is present at all")
		}
	}
	_, hasPolicy := record.Lookup(o, "executionPolicy")
	if version == int64(BridgeRecordVersion) && hasPolicy {
		wrong = append(wrong, "a version 1 record names no executionPolicy; a record that names one is version 2")
	}
	if version == int64(PolicyRecordVersion) {
		if owner != OwnerPlugin {
			wrong = append(wrong, "only a plugin-owned record names an execution policy")
		}
		wrong = append(wrong, policyComplaints(record.Get(o, "executionPolicy"))...)
	}
	return wrong
}

// readBridgeRecord is bridgerecord.read: absent, unreadable and unusable stay three answers.
func readBridgeRecord(path string) (Object, string, string) {
	found := reading.ReadJSON(path, "the bridge MCP record", nil, nil)
	switch {
	case found.State == reading.Absent:
		return nil, RecordAbsent, "no record at " + path
	case !found.OK():
		return nil, found.State, found.Detail
	}
	if wrong := bridgeComplaints(found.Value); len(wrong) > 0 {
		return nil, RecordMalformed, strings.Join(wrong, "; ")
	}
	return found.Value.(Object), "", ""
}

var bridgeIdentity = []string{"owner", "serverName", "bridgeExecutable", "args", "executionPolicy"}

func sameRegistration(found, wanted Object) bool {
	for _, key := range bridgeIdentity {
		if evidence.Dumps(record.Get(found, key), true, true, false) != evidence.Dumps(record.Get(wanted, key), true, true, false) {
			return false
		}
	}
	return true
}

const policyRepair = "move %s aside by hand, then run crw install register-mcp again. Threads started in between find no record and start no bridge; threads already running keep the bridge they spawned"

// bridgeWrite is bridgerecord.write: decided twice, acted on once, never over a record that
// says something else, and a record naming a policy is settled only against the policy file as
// it stands when the answer is given. The decision is made on a look taken before anything is
// read, and the record is written only while a look under the lock sees that same document.
func bridgeWrite(path string, wanted Object, apply bool) Object {
	answer := Object{field("record", path), field("outcome", ""), field("applied", false), field("wrote", false)}
	if wrong := bridgeComplaints(wanted); len(wrong) > 0 {
		return append(record.Set(answer, "outcome", RecordMalformed), field("detail", strings.Join(wrong, "; ")), field("complaints", strs(wrong)))
	}
	basis := lookAt(path)
	outcome, found := bridgeOutcome(path, wanted)
	answer = record.Set(answer, "outcome", outcome)
	policyNow := func() []string {
		if reference := record.Get(wanted, "executionPolicy"); reference != nil {
			return policyFileComplaints(reference)
		}
		return nil
	}
	switch outcome {
	case reading.Unreadable, reading.AccessError:
		return append(answer, field("detail", "the record could not be read, so nothing was written"))
	case RecordUnchanged:
		if stale := policyNow(); len(stale) > 0 {
			return append(record.Set(answer, "outcome", RecordPolicyChanged), field("detail", "this record is already installed, and "+strings.Join(stale, "; ")+", so the launcher refuses to start the bridge from it. The record was left as it was"), field("repair", strings.ReplaceAll(policyRepair, "%s", path)))
		}
		return append(answer, field("detail", "this record is already installed"))
	case RecordDiffers:
		answer = append(answer, field("detail", "a record is already installed and says something else; this command does not overwrite it"), field("differingFields", differing(found, wanted)))
		for _, key := range differingKeys(found, wanted) {
			if key == "executionPolicy" {
				answer = append(answer, field("repair", strings.ReplaceAll(policyRepair, "%s", path)))
			}
		}
		return answer
	}
	if !apply {
		return append(answer, field("detail", "would write this record; nothing was written"))
	}
	beforeWriteLock(path)
	lock, err := record.Lock(path, 0)
	if err != nil {
		return append(record.Set(answer, "outcome", Busy), field("detail", err.Error()))
	}
	defer lock.Release()
	if again, _ := bridgeOutcome(path, wanted); again != outcome || !lookAt(path).same(basis) {
		return append(record.Set(answer, "outcome", RecordChangedUnderneath), field("detail", "the record at "+path+" changed after it was read (another file, size, modification time or bytes), so nothing was written"),
			field("repair", "rerun to decide against the file as it now stands"))
	}
	if stale := policyNow(); len(stale) > 0 {
		return append(record.Set(answer, "outcome", RecordPolicyChanged), field("detail", "the execution policy changed after it was read: "+strings.Join(stale, "; ")+". A record naming it would start no bridge, so nothing was written"), field("repair", "run register-mcp again against the file as it now stands"))
	}
	if err := record.AtomicWrite(path, record.Encode(wanted)); err != nil {
		return append(answer, field("detail", "the record could not be written: "+store.PythonOSError(err)))
	}
	back := reading.ReadJSON(path, "the bridge MCP record", nil, nil)
	readBack := back.OK() && evidence.Dumps(back.Value, true, true, false) == evidence.Dumps(wanted, true, true, false)
	if stale := policyNow(); readBack && len(stale) > 0 {
		return append(record.Set(record.Set(record.Set(answer, "outcome", RecordPolicyChanged), "applied", true), "wrote", true),
			field("detail", "the execution policy changed while this record was being written: "+strings.Join(stale, "; ")+". This run removed nothing"), field("repair", strings.ReplaceAll(policyRepair, "%s", path)))
	}
	answer = append(record.Set(record.Set(record.Set(answer, "outcome", RecordCreated), "applied", true), "wrote", true), field("readBack", readBack))
	if !readBack {
		return append(record.Set(answer, "outcome", RecordAppliedUnverified), field("detail", "the record was written and could not be read back as written; the plugin should not be relied on to start the bridge until it can be"))
	}
	return answer
}

func bridgeOutcome(path string, wanted Object) (string, Object) {
	found := reading.ReadJSON(path, "the bridge MCP record", nil, nil)
	switch {
	case found.State == reading.Absent:
		return RecordWouldCreate, nil
	case !found.OK():
		return found.State, nil
	}
	value, _ := found.Value.(Object)
	if len(bridgeComplaints(found.Value)) > 0 || !sameRegistration(value, wanted) {
		return RecordDiffers, value
	}
	return RecordUnchanged, value
}

func differingKeys(found, wanted Object) []string {
	var out []string
	for _, v := range asList(differing(found, wanted)) {
		out = append(out, v.(string))
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// servers is codexconfig.scan over a real TOML reader: the command and args of every
// mcp_servers table, with their shapes validated before anything is compared. An error is the
// configuration going unread, which refuses rather than defaults.
type servers map[string]struct {
	Command string
	Args    []string
}

func readServers(codexHome string) (servers, string, string) {
	path := filepath.Join(codexHome, "config.toml")
	read := reading.ReadText(path, "the Codex configuration")
	if !read.Usable() {
		return nil, path, "the Codex configuration could not be read: " + read.Detail
	}
	text, _ := read.Value.(string)
	var document map[string]any
	if _, err := toml.Decode(text, &document); err != nil {
		return nil, path, "this file is not readable TOML: " + err.Error()
	}
	out := servers{}
	raw, has := document["mcp_servers"]
	if !has {
		return out, path, ""
	}
	tables, ok := raw.(map[string]any)
	if !ok {
		return nil, path, "mcp_servers is a table of servers, found " + typeName(raw)
	}
	for name, entry := range tables {
		table, ok := entry.(map[string]any)
		if !ok {
			return nil, path, "the registration for " + evidence.Repr(name) + " is a table, found " + typeName(entry)
		}
		var server struct {
			Command string
			Args    []string
		}
		if command, has := table["command"]; has {
			text, ok := command.(string)
			if !ok {
				return nil, path, evidence.Repr(name) + " has a command that is not a string, it is " + typeName(command)
			}
			server.Command = text
		}
		if args, has := table["args"]; has {
			list, ok := args.([]any)
			if !ok {
				return nil, path, evidence.Repr(name) + " has args that are not a list of strings, they are " + typeName(args)
			}
			for _, word := range list {
				text, ok := word.(string)
				if !ok {
					return nil, path, evidence.Repr(name) + " has args that are not a list of strings"
				}
				server.Args = append(server.Args, text)
			}
		}
		out[name] = server
	}
	return out, path, ""
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case []any, []map[string]any:
		return "list"
	case map[string]any:
		return "dict"
	case bool:
		return "bool"
	case int64:
		return "int"
	case float64:
		return "float"
	}
	return "value"
}

// startsThisBridge is runtime_install._starts_this_bridge: a table is recognised by the command
// it starts, because its name is free to choose.
func startsThisBridge(command string, args []string, executable string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	if executable != "" && command == executable {
		return true
	}
	base := filepath.Base(command)
	return base == definition.Bridge || (base == Binary && len(args) > 0 && args[0] == "bridge")
}

func bridgeTables(view servers, exclude, executable string) []string {
	var names []string
	for name, server := range view {
		if name != exclude && startsThisBridge(server.Command, server.Args, executable) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// secondOwners is secondOwnersFor a Go runtime reached through the pointer bridgeEntry lies in
// (<pointer>/bin/codex-thread-bridge), which is where every promotion moves it.
func secondOwners(codexHome, bridgeEntry string) (Object, string) {
	return secondOwnersFor(codexHome, bridgeEntry, filepath.Dir(filepath.Dir(bridgeEntry)), goProvides)
}

// secondOwnersFor is the one-owner-per-surface check a promotion makes on the reading it
// promotes on: the bridge is registered by the Codex configuration or by the plugin record,
// never both, and a configuration entry for it names the owned pointer rather than a runtime
// this install would not select; the Stop hook is registered by the user hook file or by
// plugin-owned settings, never both. A registration that reaches through the pointer something
// the runtime about to be named does not provide (target) is refused too: the user-owned
// registrations are retired with runtime_install.py, so such an entry is the second owner beside
// the plugin's declaration, and the swap would leave it running nothing. Any reading that failed
// refuses: registering on an unanswered question is how a second copy arrives.
func secondOwnersFor(codexHome, bridgeEntry, pointerPath string, target provides) (Object, string) {
	recordPath := filepath.Join(codexHome, BridgeRecordName)
	found, outcome, detail := readBridgeRecord(recordPath)
	if found == nil && outcome != RecordAbsent {
		return Object{field("record", recordPath)}, "the record at " + recordPath + " could not be acted on (" + detail + "), so who owns the bridge was not established"
	}
	view, configPath, unread := readServers(codexHome)
	if unread != "" {
		return Object{field("configuration", configPath)}, unread + ", so whether the bridge is already registered there was not established"
	}
	tables := bridgeTables(view, "", bridgeEntry)
	var owner any
	if found != nil {
		owner = record.Get(found, "owner")
	}
	settingsPath := filepath.Join(codexHome, SettingsName)
	settings := reading.ReadJSON(settingsPath, "the completion hook configuration", nil, nil)
	var settingsOwner any
	if value, ok := settings.Value.(Object); ok && settings.OK() {
		settingsOwner = record.Get(value, "owner")
		if settingsOwner == nil {
			settingsOwner = OwnerUser
		}
	}
	hookFile := filepath.Join(codexHome, "hooks.json")
	commands, readable := hook.AdapterCommands(hookFile, "Stop")
	registrations := make([]string, 0, len(commands))
	for _, command := range commands {
		registrations = append(registrations, command.Identity)
	}
	report := Object{
		field("bridge", Object{field("record", recordPath), field("recordOwner", owner), field("configuration", configPath), field("tablesStartingIt", strs(tables))}),
		field("stop", Object{field("settings", settingsPath), field("settingsOwner", settingsOwner), field("hookFile", hookFile), field("registrations", strs(registrations))}),
	}
	switch {
	case owner == OwnerPlugin && len(tables) > 0:
		return report, "the record at " + recordPath + " names the plugin as the bridge's owner and the Codex configuration also starts it as " + strings.Join(tables, ", ") + ", so the host runs two bridges; remove one owner first"
	}
	if server, ok := view[ServerName]; ok && server.Command != bridgeEntry {
		return report, "the Codex configuration registers " + ServerName + " as " + evidence.Repr(server.Command) + ", not through the owned pointer (" + bridgeEntry + "), so after this promotion a host would still start a runtime this install does not select"
	}
	names := make([]string, 0, len(view))
	for name := range view {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := view[name]
		for _, word := range append([]string{server.Command}, server.Args...) {
			if rel, through := throughPointer(word, pointerPath); through && !target(rel) {
				return report, "the Codex configuration starts " + evidence.Repr(name) + " with " + word + ", which it reaches through the owned pointer, and the runtime about to be named provides no " + rel + ": after the swap that server would start nothing. Remove or repoint that table in " + configPath + " first"
			}
		}
	}
	if !readable {
		return report, "the user hook file could not be read, so whether the Stop adapter is registered there as well was not established"
	}
	if settingsOwner == OwnerPlugin && len(registrations) > 0 {
		return report, "the Stop settings at " + settingsPath + " name the plugin as the owner, and the user hook file also registers this adapter as " + strings.Join(registrations, ", ") + ", so every Stop runs two copies; remove the hook file entry first"
	}
	for _, command := range commands {
		for _, word := range command.Words {
			if rel, through := throughPointer(word, pointerPath); through && !target(rel) {
				return report, "the user hook file registers the Stop adapter as " + command.Identity + " (" + evidence.Repr(command.Command) + "), which runs " + word + " through the owned pointer, and the runtime about to be named provides no " + rel + ": after the swap that registration would run nothing, and the host reads the failure as a hook error, never as a judged Stop. The user-owned registration is retired with runtime_install.py and the plugin package declares the Stop hook, so this entry is the second owner: remove it from " + hookFile + " by hand, move " + settingsPath + " aside and run crw install hook --owner plugin, then rerun"
			}
		}
	}
	return record.Set(report, "detail", "one owner per surface: no second registration of the bridge or the Stop adapter was found"), ""
}

// RegisterOptions are register-mcp's inputs.
type RegisterOptions struct {
	Owner, Name, BridgeCommand, ExecutionPolicy string
	BridgeArgs                                  []string
	DryRun                                      bool
}

// executionPolicyReading is runtime_install._execution_policy_reading with the Go bridge's own
// parser: the path as the record will name it (Path(value).expanduser().absolute(): expanded
// and absolute, never resolved, '..' kept) and the digest of the bytes the parser accepted.
func executionPolicyReading(value string) (Object, string) {
	if value == "" || value != pyStrip(value) || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, "the execution policy path " + evidence.Repr(value) + " is empty, padded with whitespace or contains a control character; the bridge strips the variable it reads, so such a path would be checked here as one file and opened there as another"
	}
	expanded, err := store.ExpandUser(value)
	if err != nil {
		return nil, "the execution policy path " + evidence.Repr(value) + " could not be expanded: " + err.Error()
	}
	candidate, err := pathlibAbsolute(expanded)
	if err != nil {
		return nil, "the execution policy path " + evidence.Repr(value) + " could not be made absolute: " + err.Error()
	}
	if wrong := policyPathComplaints(candidate); len(wrong) > 0 {
		return nil, strings.Join(wrong, "; ")
	}
	policy, err := execution.FromFile(candidate)
	if err != nil {
		return nil, err.Error()
	}
	summary := policy.Summary()
	// Recorded as os.fsdecode spells it: a byte that is not UTF-8 is its surrogate escape, which
	// the record carries as "\udcXX" and each launcher fs-encodes back to the byte.
	return Object{field("path", reading.FSDecode(candidate)), field("digest", summary["digest"]), field("mode", summary["mode"]), field("roles", ordered(summary["roles"])),
		field("parsedWith", "this crw binary's bridge policy parser; the installed runtime parses the file again at every start and decides for itself")}, ""
}

// pathlibAbsolute is str(pathlib.Path(value).absolute()) on POSIX: the working directory
// prefixed when value is relative, "." components and repeated or trailing slashes dropped, and
// ".." KEPT. Folding ".." by text (filepath.Abs) names another file than the kernel opens when a
// component before it is a symbolic link, and the record, the digest and the bridge have to name
// the one file the operator named.
func pathlibAbsolute(value string) (string, error) {
	if !strings.HasPrefix(value, "/") {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		value = cwd + "/" + value
	}
	root := "/"
	if strings.HasPrefix(value, "//") && !strings.HasPrefix(value, "///") {
		root = "//"
	}
	var parts []string
	for _, part := range strings.Split(value, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return root + strings.Join(parts, "/"), nil
}

// RegisterMCP is `crw install register-mcp --owner plugin`: the record the packaged launcher
// reads, never a Codex configuration entry (the user owner is retired). The ownership decision
// and the write it authorizes happen under the crw-mcp-ownership lock the Python writers take.
func RegisterMCP(_ context.Context, o Options, r RegisterOptions) (Object, int) {
	recordPath := filepath.Join(o.CodexHome, BridgeRecordName)
	base := Object{field("command", "register-mcp"), field("owner", r.Owner), field("record", recordPath)}
	if r.Owner != OwnerPlugin {
		return append(base, field("outcome", Conflict), field("detail", "only --owner plugin is supported: the user-owned registration (a config.toml [mcp_servers] table) is retired with runtime_install.py, and the plugin declares the server itself"), field("applied", false), field("wrote", false), field("note", "nothing was written")), Usage
	}
	lock, err := record.Lock(filepath.Join(o.CodexHome, OwnershipLockName), 0)
	if err != nil {
		return append(base, field("outcome", Busy), field("detail", err.Error()), field("applied", false), field("wrote", false), field("note", "nothing was written: another run holds the ownership lock")), Refused
	}
	defer lock.Release()
	var policy Object
	if r.ExecutionPolicy != "" {
		reading, why := executionPolicyReading(r.ExecutionPolicy)
		if reading == nil {
			return append(base, field("outcome", PolicyUnreadable), field("detail", why), field("applied", false), field("wrote", false), field("note", "nothing was written: a record naming a policy the bridge would refuse is a bridge that never starts")), Refused
		}
		policy = reading
	}
	command := r.BridgeCommand
	if command == "" {
		command = filepath.Join(o.Dest, "current", "bin", definition.Bridge)
	}
	name := r.Name
	if name == "" {
		name = ServerName
	}
	if !filepath.IsAbs(command) {
		return append(base, field("outcome", Conflict), field("detail", "the bridge executable must be an absolute path when owner is plugin, because the packaged launcher runs from the installed package directory"), field("applied", false), field("wrote", false), field("note", "nothing was written")), Usage
	}
	wanted := BridgeDocument(command, r.BridgeArgs, name, o.Issue, policy)
	if conflict := pluginOwnership(o.CodexHome, recordPath, name, wanted); conflict != "" {
		return append(base, field("outcome", Conflict), field("detail", conflict), field("applied", false), field("wrote", false), field("note", "nothing was written. One owner registers this server; the other is reported with its evidence rather than joined.")), Refused
	}
	written := bridgeWrite(recordPath, wanted, !r.DryRun)
	outcome, _ := record.Get(written, "outcome").(string)
	out := append(base, field("outcome", outcome), field("detail", record.Get(written, "detail")), field("applied", record.Get(written, "applied")), field("wrote", record.Get(written, "wrote")))
	for _, key := range []string{"differingFields", "repair", "readBack"} {
		if value, has := record.Lookup(written, key); has {
			out = append(out, field(key, value))
		}
	}
	var policyValue any
	if policy != nil {
		policyValue = policy
	}
	out = append(out, field("executionPolicy", policyValue), field("preservedHow", "the Codex configuration was read and not written"),
		field("note", "No MCP server was registered: the plugin package declares the server, so installing that package registers it. Recorded, registered and a tool actually called stay three claims."))
	if recordSettled[outcome] {
		return out, OK
	}
	return out, Refused
}

// pluginOwnership is runtime_install._mcp_ownership for the plugin owner.
func pluginOwnership(codexHome, recordPath, name string, wanted Object) string {
	if name != ServerName {
		return "the plugin owner registers the server the package declares, which is " + evidence.Repr(ServerName) + ", not " + evidence.Repr(name)
	}
	if found, outcome, detail := readBridgeRecord(recordPath); found == nil && outcome != RecordAbsent {
		return "the record at " + recordPath + " could not be acted on (" + detail + "), so who owns this server was not established"
	}
	view, _, unread := readServers(codexHome)
	if unread != "" {
		return unread + ", so whether this server is already registered was not established"
	}
	executable, _ := record.Get(wanted, "bridgeExecutable").(string)
	if aliased := bridgeTables(view, "", executable); len(aliased) > 0 {
		return "the Codex configuration already starts this bridge as " + strings.Join(aliased, ", ") + ", which is a user-owned registration carrying no ownership record; a plugin declaration beside it would run a second bridge. Remove that entry first"
	}
	if _, ok := view[name]; ok {
		return "the Codex configuration already registers " + evidence.Repr(name) + ", which is the user-owned registration; a plugin declaration beside it would run a second bridge. Remove that entry first"
	}
	return ""
}

// ordered is a decoded Go value with every map turned into an object in sorted key order, so a
// report can carry it.
func ordered(v any) any {
	switch value := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := Object{}
		for _, key := range keys {
			out = append(out, field(key, ordered(value[key])))
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = ordered(item)
		}
		return out
	case []string:
		return strs(value)
	case int:
		return int64(value)
	}
	return v
}
