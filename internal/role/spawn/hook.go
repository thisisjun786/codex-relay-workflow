package spawn

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the first half of CXC v0.2.40's runSpawnAttachHook (subagent-config/src/spawn-attach-hook.ts:849-987: reading the
// payload, the recursion deny, the managed dispatch loop :892-907 and the message assembly) and runtimeSkillsDir (:54-73), with the
// CRW names of contract/schema/cxc/name-substitution.json. It stops where the oracle starts to apply role routing (:987). The
// managed dispatch sources, the loop and the key filter live in hook_managed.go. Nothing here registers, or is, a hook:
// RunSpawnAttachHook (hook_route.go) finishes the answer (promptOverride, trust prefix, ciphertext restore, item re-assembly, the
// output envelope) from the assembly. Differences from the oracle, each recorded in docs/port-cxc/known-defects.md:
//   - the skills directory: CRW_SKILLS_DIR, then <PLUGIN_ROOT>/skills where the oracle has the module-relative plugin directory;
//   - an unusable store or role, or a store path that cannot be resolved, denies the spawn with the store's error (CRW-1119); an
//     unknown role stops with empty output, as the oracle's throw does;
//   - a subagent spawn whose grant scope cannot be resolved is denied, where the oracle's throw allows it (a security fix);
//   - the working directory is read with the kernel call (syscall.Getwd), like process.cwd().

// spawnHookAssembly is every value the oracle's runSpawnAttachHook holds when it reaches :987, each named after its oracle variable.
// JSON key order is kept (pyjson.Object): the oracle answers {...toolInput, items: updatedItems}, which keeps each key where the
// caller wrote it, and a Go map would not.
type spawnHookAssembly struct {
	v2Spawn            bool                        // v2Spawn: a collaboration hook name or v2 payload markers (:863)
	toolInput          pyjson.Object               // toolInput: obj.tool_input (:859)
	itemInput          []any                       // itemInput: tool_input.items of a v1 spawn without message, nil for the oracle's null (:874); records once valid
	validItems         bool                        // validItems: non-empty, every item an object with a string type and, for text, a string text (:876)
	textItems          []pyjson.Object             // textItems: the text items (:878)
	mappedItems        []pyjson.Object             // mappedItems: itemInput with each text item stripped of control markers and normalized, nil for null (:913)
	firstText          int                         // firstText: index of the first text item in mappedItems, -1 without one (:920)
	itemBlocks         []string                    // itemBlocks: the skill bodies to append after the text items (:919)
	message            string                      // message: the caller's text, the text items joined for items (:885)
	encryptedV2Message bool                        // encryptedV2Message: a v2 message of Fernet shape; RunSpawnAttachHook keeps its bytes (:887)
	cwd                string                      // cwd: obj.cwd, else the process working directory, unresolved (:888)
	role               role.RoleName               // role: InferRole over the item scan or the normalized message (:925)
	resolution         role.SpawnResolution        // resolution: ResolveSpawnConfig for role (:926)
	trustPrefix        string                      // trustPrefix: always empty, the trust warning went with the project layer (:927)
	guard              string                      // guard: the surface's guard block and the grant instruction (:979)
	updatedMessage     string                      // updatedMessage: the start value of the oracle's evidenceExemptMessage (:984, :987)
	managed            *role.ManagedSpawnSelection // managed: the managed dispatch resolution, nil for a direct spawn (:892)
	dispatchSource     string                      // dispatchSource: the source line that resolved (:893)
	sessionID          string                      // sessionID: obj.session_id when it is a string, else "" (:899)
	toolUseID          *string                     // toolUseID: obj.tool_use_id when it is a string, else nil (:1096)
	commit             *spawnHookCommit            // what this run has committed, shared with RunSpawnAttachHook
	grant              *spawnGrantClaim            // a subagent's checked grant, spent by finish
	record             func(answer string)         // records the answer of the call that spends grant, before the grant is spent, or nil
	inputText          string                      // tool_input as JSON.stringify writes it, for the event's replay record
	replay             func(answer string)         // records the answer of an event that minted a grant, or nil
	settings           role.SettingsSnapshot       // the event's one read of the helper role settings
	evidenceAssignment *evidence.Assignment        // CRW-1115: the evidence assignment the packet asked for, written once the spawn is allowed
	supersedes         *evidence.Assignment        // this call's open assignment for an input it no longer carries: removed once the edited input is allowed
	evidenceRecorded   bool                        // evidenceAssignment is the record an earlier delivery of this event wrote: kept as it is
	promptForms        []string                    // the forms the role's prompt override can have in a reapplied message: as written, and as the hook's own normalization leaves it (CRW-1121)
	guardReapplied     bool                        // guardReapplied: the message already starts with this surface's guard, so the hook runs over its own output (:983)
}

// spawnHookAssemble reads one PreToolUse payload in the oracle's order. The third result is true when the answer is already known:
// the second result is then the deny envelope, or empty for the oracle's empty output (a no-op, or a throw its outer catch turns
// into nothing), and the assembly is the zero value. Otherwise the assembly is complete. A Fernet-shaped message stops nothing, and an
// exact guard reapplication assembles: only its later output is empty.
//
// obj is a decoded payload. tool_input and its items may be an ordered pyjson.Object or a plain map; a plain map is read with sorted
// keys, which is a compatibility path, because a payload decoded into maps would otherwise read as no object and allow the spawn.
func spawnHookAssemble(obj map[string]any, env host.LookupEnv) (spawnHookAssembly, string, bool) {
	return spawnHookAssembleWith(obj, env, &spawnHookCommit{})
}

// spawnHookAssembleWith is spawnHookAssemble recording what it commits in commit. An answer that allows the spawn untouched
// still spends a subagent's grant (spawnHookAssembly.finish), so a one-time grant never survives a spawn it let through.
func spawnHookAssembleWith(obj map[string]any, env host.LookupEnv, commit *spawnHookCommit) (spawnHookAssembly, string, bool) {
	var grant *spawnGrantClaim
	var record func(answer string) // set once the event's input is known: the subagent's spend of a grant is recorded with its answer
	stop := func(deny string) (spawnHookAssembly, string, bool) {
		if deny == "" && grant != nil {
			deny = spawnHookAssembly{grant: grant, commit: commit, record: record}.finish("", env)
		}
		return spawnHookAssembly{}, deny, true
	}
	tmpRoot, now := spawnHookTmpDir(env), time.Now()
	if event, _ := obj["hook_event_name"].(string); event != "PreToolUse" || !IsSpawnToolName(obj["tool_name"]) {
		return stop("")
	}
	toolInput, ok := spawnHookRecord(obj["tool_input"])
	if !ok {
		return stop("")
	}
	a := spawnHookAssembly{toolInput: toolInput, firstText: -1, commit: commit}
	a.v2Spawn = IsCollaborationToolName(obj["tool_name"]) || IsV2SpawnInput(spawnHookView(toolInput))
	if session, ok := obj["session_id"].(string); ok {
		a.sessionID = session
	}
	if id, ok := obj["tool_use_id"].(string); ok {
		a.toolUseID = &id
	}
	// The same event applied again to its own input or to the input it answered with gets its recorded answer, so a minted grant is
	// kept and none is minted again, and a subagent's spent grant answers the call that spent it, never another input (CRW-1121,
	// CRW-1118). A tool_input too deep to digest has no record.
	if a.toolUseID != nil && *a.toolUseID != "" && !spawnHookRouteDeep(toolInput) {
		a.inputText = spawnHookRouteStringify(toolInput)
		tool := *a.toolUseID
		// The record holds the managed binding the event was issued under (root, record, role, candidate and native call), read when
		// the answer is recorded, so a replay is checked against it (CRW-1122).
		record = func(answer string) {
			spawnHookReplayRecord(obj, tmpRoot, tool, spawnHookReplayBinding(a.managed, a.dispatchSource, tool), a.inputText, answer)
		}
		if replayed, ok := spawnHookReplayLookup(obj, tmpRoot, tool, a.inputText); ok {
			return stop(spawnHookReplayCurrent(replayed, a.sessionID, spawnHookReplayCwd(obj), a.v2Spawn))
		}
	}

	// Project only the caller's text, never attachment metadata (:870-880). A null message is no message, so the items are read
	// (CRW-1114; the oracle counted a null member as present and left valid items unguarded).
	var records []pyjson.Object
	a.itemInput, records, a.validItems = spawnHookPacketItems(toolInput, a.v2Spawn)
	message, isString := toolInput.Get("message").(string)
	outgoing := message
	if a.validItems {
		var texts []string
		for _, item := range records {
			if item.Get("type") == "text" {
				a.textItems = append(a.textItems, item)
				texts = append(texts, item.Get("text").(string))
			}
		}
		outgoing = strings.Join(texts, "\n\n")
	}

	// D1: a spawn by a subagent needs a minted grant, and is denied before the message no-op below (:881-882). Where the oracle
	// throws for a grant scope it cannot resolve (:396) and its outer catch prints nothing, which allows the spawn, the port denies:
	// a failed grant check never lets a subagent recurse (known-defects, security). The grant is only checked here and spent with
	// the answer, after every other refusal, so a refused spawn leaves it to the corrected retry (CRW-1118; the oracle spent it
	// first).
	spawnedBySubagent := IsSubagentSpawner(obj)
	if spawnedBySubagent {
		commit.subagent = true
		tool := ""
		if a.toolUseID != nil {
			tool = *a.toolUseID
		}
		// Deliveries of one call are serialized from here to the end of the answer, so of two that carry the grant one spends it and
		// the other finds the record of that spend.
		if _, marked := spawnGrantOnlyMarker(outgoing); marked && a.inputText != "" {
			release, err := spawnHookEventLock(obj, tmpRoot, tool, false)
			if errors.Is(err, errSpawnHookEventBusy) {
				return stop(DenyEnvelope(RecurseDenyReason))
			}
			if release != nil {
				commit.unlock = release
				if replayed, ok := spawnHookReplayLookup(obj, tmpRoot, tool, a.inputText); ok {
					return stop(spawnHookReplayCurrent(replayed, a.sessionID, spawnHookReplayCwd(obj), a.v2Spawn))
				}
			}
		}
		claim, ok := spawnGrantCheck(obj, outgoing, tmpRoot, os.Getuid(), now, tool, a.inputText)
		if !ok {
			return stop(DenyEnvelope(RecurseDenyReason))
		}
		grant, a.grant, a.record = claim, claim, record
	}

	if a.validItems {
		message, isString = outgoing, true
	}
	if !isString || (!a.validItems && text.Trim(message) == "") {
		return stop("")
	}
	a.message = message
	a.encryptedV2Message = a.v2Spawn && IsFernetTokenShape(message)
	if cwd, _ := obj["cwd"].(string); cwd != "" {
		a.cwd = cwd
	} else if wd, err := syscall.Getwd(); err == nil {
		a.cwd = wd
	} else {
		return stop("")
	}
	dispatchScan := "" // the role is read from the raw item text without its closed skill blocks (:889)
	for i, item := range a.textItems {
		_, scanned := spawnInlineScanBlocks(item.Get("text").(string))
		dispatchScan += strings.Repeat("\n\n", min(i, 1)) + scanned
	}

	// A root spawn that asks for recursion mints a one-time grant (:908-909). The oracle resolves the scope first (:377), and it
	// creates a missing temp root with mode 0700 (recursive mkdir), which the port does too, only for a scope that resolves.
	//
	// The managed dispatch loop (:892-907) runs before the mint, as the oracle's deny does. Its source text is the first raw text
	// item, or the message for a single-message spawn; a deny here returns before any grant is minted.
	dispatchText := message
	if a.validItems {
		dispatchText = ""
		if len(a.textItems) > 0 {
			dispatchText = a.textItems[0].Get("text").(string)
		}
	}
	// One settings snapshot serves the whole event: the dispatch sources, the role resolution, the prompt and the notice (CRW-1124;
	// the oracle read the store up to three times, so a concurrent edit could reach one step and not another). The next event reads
	// the store again.
	var snapshot *role.SettingsSnapshot
	settings := func() role.SettingsSnapshot {
		if snapshot == nil {
			s := spawnHookSettings(env)
			snapshot = &s
		}
		return *snapshot
	}
	sources, err := spawnDispatchSourcesWith(dispatchText, settings)
	if err != nil {
		return stop(spawnHookSettingsDeny(err)) // the oracle's readSettings throw, caught by its outer catch
	}
	if deny, stopped := spawnHookManaged(&a, sources); stopped {
		return stop(deny)
	}
	var minted string
	if _, _, resolved := spawnGrantScope(obj); resolved && !spawnedBySubagent && strings.Contains(message, SubspawnToken) {
		_ = os.MkdirAll(tmpRoot, 0o700)
		// Deliveries of one event that passed the lookup together are serialized here: the lock is held until the answer is recorded, and
		// the delivery that gets it second finds the first's record, so one event mints one grant (CRW-1121).
		if a.inputText != "" {
			release, err := spawnHookEventLock(obj, tmpRoot, *a.toolUseID, true)
			if errors.Is(err, errSpawnHookEventBusy) {
				return stop(DenyEnvelope(spawnHookBusyReason))
			}
			if release != nil {
				commit.unlock = release
				if replayed, ok := spawnHookReplayLookup(obj, tmpRoot, *a.toolUseID, a.inputText); ok {
					return stop(spawnHookReplayCurrent(replayed, a.sessionID, spawnHookReplayCwd(obj), a.v2Spawn))
				}
			}
		}
		minted, _ = MintRecursionGrant(obj, tmpRoot, now)
	}
	if minted != "" && a.inputText != "" {
		a.replay = record
	}

	// Each text item is normalized on its own, so an attachment boundary never joins fences, links or mentions (:913-918).
	skillsDir := spawnHookSkillsDir(env)
	var texts []string
	if a.validItems {
		for _, item := range records {
			if item.Get("type") == "text" {
				item = slices.Clone(item).Set("text", NormalizeSkillMentions(StripControlMarkers(item.Get("text").(string), true), skillsDir))
				texts = append(texts, item.Get("text").(string))
			}
			a.mappedItems = append(a.mappedItems, item)
		}
		if skillsDir != "" {
			a.itemBlocks = SkillBlocks(texts, skillsDir)
		}
		a.firstText = slices.IndexFunc(a.mappedItems, func(item pyjson.Object) bool { return item.Get("type") == "text" })
	}
	controlled := StripControlMarkers(message, false)
	if a.validItems {
		controlled = ""
		if a.firstText >= 0 {
			controlled = a.mappedItems[a.firstText].Get("text").(string)
		}
	}
	normalized := controlled
	if !a.validItems && skillsDir != "" {
		normalized = NormalizeSkillMentions(controlled, skillsDir)
	}
	roleSource := normalized
	if a.validItems {
		roleSource = dispatchScan
	}
	if a.managed != nil {
		a.role = a.managed.Role // the managed resolution decides the role (:925)
	} else {
		a.role = InferRole(toolInput.Get("agent_type"), roleSource)
	}
	a.settings = settings()
	resolution, err := a.settings.Resolve(a.role)
	if err != nil {
		return stop(spawnHookSettingsDeny(err)) // the oracle's throw, caught by its outer catch
	}
	a.resolution = resolution
	a.promptForms = spawnHookPromptForms(resolution.PromptOverride, skillsDir)

	// A single message carries the bodies of the skills it mentions (:943); a v2 message that got none, within the size cap, gets the
	// self-load instruction instead (:956-964). Text inside a closed inlined block never counts as a marker (:951).
	inlined := normalized
	if !a.validItems && skillsDir != "" {
		inlined = InlineSkillBodies(normalized, skillsDir)
	}
	_, markerScan := spawnInlineScanBlocks(inlined)
	affordance := inlined
	if a.v2Spawn && skillsDir != "" && inlined == normalized && !strings.Contains(markerScan, SkillAffordanceMarker) {
		if candidate := inlined + "\n\n" + SkillAffordanceBlock(skillsDir); spawnInlineUTF16Units(candidate) <= spawnNormalizeMaxLength {
			affordance = candidate
		}
	}

	// D2: the surface's guard (:972-979); an owned guard already in front is replaced below, so a second pass is idempotent.
	switch {
	case a.v2Spawn && minted != "":
		a.guard = LeafGuardBlockCoordinator
	case a.v2Spawn:
		a.guard = LeafGuardBlock
	case minted != "":
		a.guard = V1ScopeBlockCoordinator
	default:
		a.guard = V1ScopeBlock
	}
	if minted != "" {
		a.guard += spawnGrantInstruction + "[CRW-SUBSPAWN-GRANT:" + minted + "]"
	}
	if a.trustPrefix != "" {
		affordance = strings.TrimPrefix(affordance, a.trustPrefix)
	}
	// A guard the hook writes, of any surface or authority, is replaced by this event's guard instead of stacked under it
	// (CRW-1121; the oracle kept only an exact match of this event's guard, :983-985). A coordinator guard the caller wrote is
	// replaced the same way, so its text authorizes nothing: only a grant this event mints does.
	rest, owned := spawnHookOwnedGuard(affordance)
	switch {
	case owned && rest == "":
		a.updatedMessage, a.guardReapplied = a.guard, true
	case owned:
		a.updatedMessage, a.guardReapplied = a.guard+"\n\n"+rest, true
	case a.validItems && affordance == "":
		a.updatedMessage = a.guard
	default:
		a.updatedMessage = a.guard + "\n\n" + affordance
	}
	// Deliveries of one event that registers an evidence assignment are serialized by the event's lock, held until the answer is
	// given, so of two deliveries of one call the second finds the first's record and reuses it (CRW-1121 with CRW-1115).
	serialize := func() string {
		if commit.unlock != nil || a.inputText == "" {
			return ""
		}
		if _, _, resolved := spawnGrantScope(obj); resolved {
			_ = os.MkdirAll(tmpRoot, 0o700)
		}
		release, err := spawnHookEventLock(obj, tmpRoot, *a.toolUseID, true)
		if errors.Is(err, errSpawnHookEventBusy) {
			return DenyEnvelope(spawnHookBusyReason)
		}
		commit.unlock = release
		return ""
	}
	if deny := spawnHookEvidenceAssignment(&a, time.Now(), serialize); deny != "" {
		return stop(deny)
	}
	return a, "", false
}

// spawnHookPacketItems is the item projection of a spawn's tool_input (:870-880): the items of a v1 spawn whose message is absent or
// null (raw, as the input holds them), and, when they are valid (non-empty, each an object with a string type and, for a text item,
// a string text), the items as records and true. A v2 spawn and a v1 spawn with a message have no items. The first delivery and the
// replay of an event (spawnHookPacketText) read the packet through this one projection, so they judge the same text (CRW-1122).
func spawnHookPacketItems(toolInput pyjson.Object, v2 bool) ([]any, []pyjson.Object, bool) {
	var raw []any
	if value, hasMessage := toolInput.Lookup("message"); !v2 && (!hasMessage || value == nil) {
		raw, _ = toolInput.Get("items").([]any)
	}
	records := make([]pyjson.Object, 0, len(raw))
	for _, v := range raw {
		item, ok := spawnHookRecord(v)
		kind, typed := item.Get("type").(string)
		_, texted := item.Get("text").(string)
		if !ok || !typed || (kind == "text" && !texted) {
			return raw, nil, false
		}
		records = append(records, item)
	}
	if len(records) == 0 {
		return raw, nil, false
	}
	items := make([]any, len(records))
	for i, item := range records {
		items[i] = item
	}
	return items, records, true
}

// spawnHookPacketText is the text the final gate judges for a spawn's tool_input: its valid items' text joined by a blank line
// (spawnHookPacketItems), else its message ("" when that is not a string).
func spawnHookPacketText(toolInput pyjson.Object, v2 bool) string {
	if _, records, valid := spawnHookPacketItems(toolInput, v2); valid {
		var texts []string
		for _, item := range records {
			if item.Get("type") == "text" {
				texts = append(texts, item.Get("text").(string))
			}
		}
		return strings.Join(texts, "\n\n")
	}
	message, _ := toolInput.Get("message").(string)
	return message
}

// spawnHookPromptForms are the spellings a role's prompt override can have in a message that carries the hook's earlier answer: as it
// was inserted (trimmed), and as the hook's own normalization leaves it when it reads that message again (control markers stripped,
// runs of blank lines collapsed, skill mentions rewritten). Both are the same prompt, so neither is inserted a second time (CRW-1121).
func spawnHookPromptForms(prompt *string, skillsDir string) []string {
	if prompt == nil || text.Trim(*prompt) == "" {
		return nil
	}
	forms := []string{text.Trim(*prompt)}
	for _, preserve := range []bool{false, true} {
		form := StripControlMarkers(forms[0], preserve)
		if skillsDir != "" {
			form = NormalizeSkillMentions(form, skillsDir)
		}
		if form != "" && !slices.Contains(forms, form) {
			forms = append(forms, form)
		}
	}
	return forms
}

// spawnHookPromptLead is the form of the prompt that s starts with: s is that form, or the form is followed by a blank line.
func spawnHookPromptLead(s string, forms []string) (string, bool) {
	for _, form := range forms {
		if s == form || strings.HasPrefix(s, form+"\n\n") {
			return form, true
		}
	}
	return "", false
}

// spawnHookReplayCwd is the working directory of an event: obj.cwd, else the process's.
func spawnHookReplayCwd(obj map[string]any) string {
	if cwd, _ := obj["cwd"].(string); cwd != "" {
		return cwd
	}
	wd, _ := syscall.Getwd()
	return wd
}

// spawnHookSettingsDeny is the answer for a settings read that failed: an unusable store or role, or a store path that cannot be
// resolved, denies the recognized spawn with the store's error, whose text names the repair (CRW-1119; the oracle's catch printed
// nothing, so the spawn ran on the main model without its configured routing). Any other failure keeps the oracle's empty output.
func spawnHookSettingsDeny(err error) string {
	if errors.As(err, new(*role.UnusableSettingsError)) {
		return DenyEnvelope("crw: " + err.Error())
	}
	return ""
}

// spawnHookEvidenceAssignment is CRW-1115 (port: fixed; the oracle has no such step): when the caller's packet assigns its child a
// worktree (CRW-WORKTREE:) or allows it no evidence write (CRW-EVIDENCE: none), it builds the session's evidence assignment and
// injects its block right after the guard. The record itself is written by spawnHookRoute once the spawn is allowed. A packet
// that starts, after the guard, with the block of an open assignment this very tool call recorded for this request (a second pass
// of this hook: same tool_use_id) is left alone. The block of a record another call made (a copied packet), or of a call that
// cannot show its id, is taken out and this call gets an assignment of its own in its place, so two dispatches never share one;
// a block at that place that names no record is refused. A marker elsewhere in the text registers nothing and suppresses
// nothing. A native V2 ciphertext cannot be read, so it gets none. An ambiguous request or a tree that cannot be registered is a
// deny envelope: the parent asked for a contract the gate could not honour. serialize takes the event's lock before a record of
// the event is looked up, and returns a deny when another delivery keeps it.
func spawnHookEvidenceAssignment(a *spawnHookAssembly, now time.Time, serialize func() string) string {
	if a.encryptedV2Message {
		return ""
	}
	worktree, none, present, err := spawnEvidenceRequest(a.message)
	if err == nil && !present {
		return ""
	}
	mode := evidence.AssignTree
	if none {
		mode = evidence.AssignNone
	}
	toolUseID := ""
	if a.toolUseID != nil {
		toolUseID = *a.toolUseID
	}
	// A second pass over the hook's own output has the guard first and the assignment block right after it. Only that block, and
	// only when it names the open assignment this very tool call recorded for this request, stands for the registration; a marker
	// anywhere else in the packet (a placeholder, a log, another dispatch's quoted block) is the parent's own text and registers
	// nothing.
	if a.guardReapplied && err == nil {
		rest := strings.TrimPrefix(a.updatedMessage, a.guard)
		rest = strings.TrimPrefix(rest, "\n\n")
		for _, form := range a.promptForms {
			if after, ok := strings.CutPrefix(rest, form+"\n\n"); ok {
				rest = after
				break
			}
		}
		if id, ok := evidence.LeadingAssignmentID(rest); ok {
			recorded, readable := evidence.RecordedAssignment(a.cwd, a.sessionID, id)
			// An open record answers the input it was registered for (the digest of the input it answered the call with), not another
			// input of the same call: an edit made before the child claimed it is a dispatch of its own (CRW-1121).
			if readable && recorded.RegisteredBy(toolUseID, worktree, mode) &&
				(a.inputText == "" || recorded.AnswerInput == "" || recorded.AnswerInput == spawnHookDigest(a.inputText)) {
				return ""
			}
			// The input this call was answered with, delivered again, is the same event even after its child claimed the record: it
			// keeps that record, where registering another would leave an open one no child claims (CRW-1121, verification round 4).
			// The record holds the digest of that answered input, so another input of the call is still a dispatch of its own.
			if readable && a.inputText != "" && recorded.ToolUseID == toolUseID && recorded.AnswerInput == spawnHookDigest(a.inputText) {
				return ""
			}
			stale := ""
			if readable {
				stale = EvidenceAssignmentBlock(recorded.ID, recorded.Root, recorded.Mode == evidence.AssignNone)
				if toolUseID != "" && recorded.ToolUseID == toolUseID && recorded.Status == evidence.AssignmentOpen && recorded.AgentID == "" {
					a.supersedes = &recorded // this call's earlier registration, no child has it: the edited input replaces it
				}
			}
			after, isBlock := strings.CutPrefix(rest, stale)
			if stale == "" || !isBlock || after != "" && !strings.HasPrefix(after, "\n\n") {
				return DenyEnvelope("evidence assignment: the packet starts with an assignment block that is not a recorded assignment of this session")
			}
			// The copied block is taken out; this call's own block goes right after the guard below.
			head, remaining := a.updatedMessage[:len(a.updatedMessage)-len(rest)], strings.TrimPrefix(after, "\n\n")
			if remaining == "" {
				head = strings.TrimSuffix(head, "\n\n")
			}
			a.updatedMessage = head + remaining
		}
	}
	var assignment evidence.Assignment
	if err == nil {
		assignment, err = evidence.NewAssignment(a.sessionID, worktree, mode, now)
	}
	if err != nil {
		return DenyEnvelope("evidence assignment: " + err.Error())
	}
	assignment.ToolUseID = toolUseID
	// The assignment of an event whose input is known is named after the event: its session, its tool call and that input. A
	// delivery of the same event again (the host retrying the hook) finds the record the first delivery wrote and answers with it,
	// claimed or not, instead of registering a second dispatch that no child would ever claim: an open record without a child
	// refuses every later child of the session that is tied to no dispatch (CRW-1121 with CRW-1115). Another call, or another
	// input of this call, is another name, so each dispatch still has its own contract; a call without an id gets a random one.
	if a.inputText != "" {
		if deny := serialize(); deny != "" {
			return deny
		}
		assignment.ID = spawnHookEventAssignmentID(a.cwd, a.sessionID, toolUseID, a.inputText)
		if recorded, ok := evidence.RecordedAssignment(a.cwd, a.sessionID, assignment.ID); ok && recorded.ToolUseID == toolUseID {
			assignment, a.evidenceRecorded = recorded, true
		}
	}
	a.evidenceAssignment = &assignment
	block := EvidenceAssignmentBlock(assignment.ID, assignment.Root, assignment.Mode == evidence.AssignNone)
	// The block goes right after the guard, or after the prompt override when the packet already has it there (an earlier answer
	// whose block was taken out above): the route inserts the prompt only where it does not follow the guard yet, so a block put
	// between them would get the prompt a second time (CRW-1121).
	anchor := a.guard
	if form, ok := spawnHookPromptLead(strings.TrimPrefix(a.updatedMessage, a.guard+"\n\n"), a.promptForms); ok && strings.HasPrefix(a.updatedMessage, a.guard+"\n\n") {
		anchor = a.guard + "\n\n" + form
	}
	if a.updatedMessage == anchor {
		a.updatedMessage = anchor + "\n\n" + block
	} else {
		a.updatedMessage = strings.Replace(a.updatedMessage, anchor+"\n\n", anchor+"\n\n"+block+"\n\n", 1)
	}
	return ""
}

// spawnHookEventAssignmentID is the assignment id of one event: the first 128 bits of the sha256 of its cwd, session, tool use id
// and input, in the base32 alphabet of the ids NewAssignment mints (26 characters).
func spawnHookEventAssignmentID(cwd, sessionID, toolUseID, input string) string {
	sum := sha256.Sum256([]byte("crw-evidence-assignment\x00" + cwd + "\x00" + sessionID + "\x00" + toolUseID + "\x00" + input))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
}

// spawnHookRecord is isRecord over a decoded value: an ordered object, or a plain map with its keys sorted.
func spawnHookRecord(v any) (pyjson.Object, bool) {
	switch o := v.(type) {
	case pyjson.Object:
		return o, true
	case map[string]any:
		out := make(pyjson.Object, 0, len(o))
		for _, key := range slices.Sorted(maps.Keys(o)) {
			out = append(out, pyjson.Field{Key: key, Value: o[key]})
		}
		return out, true
	}
	return nil, false
}

// spawnHookView is the map the classifiers of classify.go read.
func spawnHookView(o pyjson.Object) map[string]any {
	out := make(map[string]any, len(o))
	for _, f := range o {
		out[f.Key] = f.Value
	}
	return out
}

// spawnHookSkillsDir is runtimeSkillsDir (:54-73): the first candidate that resolves, symbolic links followed, or none. The oracle's
// candidates are CXC_SKILLS_DIR (trimmed) and the plugin's skills directory next to its script; a Go binary has no script
// directory, so the second candidate is <PLUGIN_ROOT>/skills, the directory Codex names to the plugin's own hook commands.
func spawnHookSkillsDir(env host.LookupEnv) string {
	override, _ := env("CRW_SKILLS_DIR")
	candidates := []string{text.Trim(override)}
	if root, _ := env("PLUGIN_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, "skills"))
	}
	for _, candidate := range candidates {
		if abs, err := filepath.Abs(candidate); candidate != "" && err == nil {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				return real
			}
		}
	}
	return ""
}

// spawnHookTmpDir is Node's os.tmpdir() on POSIX: the first of TMPDIR, TMP and TEMP that is not empty, else /tmp, without one
// trailing slash unless that is the whole path. (grant.go takes the root from its caller.)
func spawnHookTmpDir(env host.LookupEnv) string {
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if dir, _ := env(key); dir != "" {
			if len(dir) > 1 {
				dir = strings.TrimSuffix(dir, "/")
			}
			return dir
		}
	}
	return "/tmp"
}
