package spawn

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the first half of CXC v0.2.40's runSpawnAttachHook (subagent-config/src/spawn-attach-hook.ts:849-987: reading the
// payload, the recursion deny and the message assembly) and runtimeSkillsDir (:54-73), with the CRW names of
// contract/schema/cxc/name-substitution.json. It stops where the oracle starts to apply role routing (:987), and it leaves out the
// managed dispatch loop (:889-907, a later issue), so "managed" is always absent. Nothing here registers, or is, a hook: the next
// issue finishes the answer (promptOverride, trust prefix, ciphertext restore, item re-assembly, the output envelope) from the
// assembly. Differences from the oracle, each recorded in docs/port-cxc/known-defects.md:
//   - the skills directory: CRW_SKILLS_DIR, then <PLUGIN_ROOT>/skills where the oracle has the module-relative plugin directory;
//   - an unusable store, a missing home and an unknown role stop with empty output, as the oracle's throw does;
//   - a subagent spawn whose grant scope cannot be resolved is denied, where the oracle's throw allows it (a security fix);
//   - the working directory is read with the kernel call (syscall.Getwd), like process.cwd().

// spawnHookAssembly is every value the oracle's runSpawnAttachHook holds when it reaches :987, each named after its oracle variable.
// JSON key order is kept (pyjson.Object): the oracle answers {...toolInput, items: updatedItems}, which keeps each key where the
// caller wrote it, and a Go map would not.
type spawnHookAssembly struct {
	v2Spawn            bool                 // v2Spawn: a collaboration hook name or v2 payload markers (:863)
	toolInput          pyjson.Object        // toolInput: obj.tool_input (:859)
	itemInput          []any                // itemInput: tool_input.items of a v1 spawn without message, nil for the oracle's null (:874); records once valid
	validItems         bool                 // validItems: non-empty, every item an object with a string type and, for text, a string text (:876)
	textItems          []pyjson.Object      // textItems: the text items (:878)
	mappedItems        []pyjson.Object      // mappedItems: itemInput with each text item stripped of control markers and normalized, nil for null (:913)
	firstText          int                  // firstText: index of the first text item in mappedItems, -1 without one (:920)
	itemBlocks         []string             // itemBlocks: the skill bodies to append after the text items (:919)
	message            string               // message: the caller's text, the text items joined for items (:885)
	encryptedV2Message bool                 // encryptedV2Message: a v2 message of Fernet shape; the next issue keeps its bytes (:887)
	cwd                string               // cwd: obj.cwd, else the process working directory, unresolved (:888)
	role               role.RoleName        // role: InferRole over the item scan or the normalized message (:925)
	resolution         role.SpawnResolution // resolution: ResolveSpawnConfig for role (:926)
	trustPrefix        string               // trustPrefix: always empty, the trust warning went with the project layer (:927)
	guard              string               // guard: the surface's guard block and the grant instruction (:979)
	updatedMessage     string               // updatedMessage: the start value of the oracle's evidenceExemptMessage (:984, :987)
}

// spawnHookAssemble reads one PreToolUse payload in the oracle's order. The third result is true when the answer is already known:
// the second result is then the deny envelope, or empty for the oracle's empty output (a no-op, or a throw its outer catch turns
// into nothing), and the assembly is the zero value. Otherwise the assembly is complete. A Fernet-shaped message stops nothing, and an
// exact guard reapplication assembles: only its later output is empty.
//
// obj is a decoded payload. tool_input and its items may be an ordered pyjson.Object or a plain map; a plain map is read with sorted
// keys, which is a compatibility path, because a payload decoded into maps would otherwise read as no object and allow the spawn.
func spawnHookAssemble(obj map[string]any, env host.LookupEnv) (spawnHookAssembly, string, bool) {
	stop := func(deny string) (spawnHookAssembly, string, bool) { return spawnHookAssembly{}, deny, true }
	if event, _ := obj["hook_event_name"].(string); event != "PreToolUse" || !IsSpawnToolName(obj["tool_name"]) {
		return stop("")
	}
	toolInput, ok := spawnHookRecord(obj["tool_input"])
	if !ok {
		return stop("")
	}
	a := spawnHookAssembly{toolInput: toolInput, firstText: -1}
	a.v2Spawn = IsCollaborationToolName(obj["tool_name"]) || IsV2SpawnInput(spawnHookView(toolInput))

	// Project only the caller's text, never attachment metadata (:870-880). A null message is present, so items are then unread.
	if _, hasMessage := toolInput.Lookup("message"); !a.v2Spawn && !hasMessage {
		a.itemInput, _ = toolInput.Get("items").([]any)
	}
	var records []pyjson.Object
	a.validItems = len(a.itemInput) > 0
	for _, raw := range a.itemInput {
		item, ok := spawnHookRecord(raw)
		kind, typed := item.Get("type").(string)
		_, texted := item.Get("text").(string)
		if !ok || !typed || (kind == "text" && !texted) {
			a.validItems = false
			break
		}
		records = append(records, item)
	}
	message, isString := toolInput.Get("message").(string)
	outgoing := message
	if a.validItems {
		a.itemInput = make([]any, len(records))
		var texts []string
		for i, item := range records {
			a.itemInput[i] = item
			if item.Get("type") == "text" {
				a.textItems = append(a.textItems, item)
				texts = append(texts, item.Get("text").(string))
			}
		}
		outgoing = strings.Join(texts, "\n\n")
	}

	// D1: a spawn by a subagent needs a minted grant, and is denied before the message no-op below (:881-882). Where the oracle
	// throws for a grant scope it cannot resolve (:396) and its outer catch prints nothing, which allows the spawn, the port denies:
	// a failed grant check never lets a subagent recurse (known-defects, security).
	tmpRoot, now := spawnHookTmpDir(env), time.Now()
	spawnedBySubagent := IsSubagentSpawner(obj)
	if spawnedBySubagent && !ConsumeRecursionGrant(obj, outgoing, tmpRoot, now) {
		return stop(DenyEnvelope(RecurseDenyReason))
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
	var minted string
	if _, _, resolved := spawnGrantScope(obj); resolved && !spawnedBySubagent && strings.Contains(message, SubspawnToken) {
		_ = os.MkdirAll(tmpRoot, 0o700)
		minted, _ = MintRecursionGrant(obj, tmpRoot, now)
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
	a.role = InferRole(toolInput.Get("agent_type"), roleSource)
	resolution, err := role.ResolveSpawnConfig(env, a.role)
	if err != nil {
		return stop("") // the oracle's throw, caught by its outer catch
	}
	a.resolution = resolution

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

	// D2: the surface's guard (:972-979); a recognized exact guard prefix keeps a second pass idempotent (:983-985).
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
		a.guard += "\nOne child spawn is authorized. Include this exact one-time capability in that spawn message: [CRW-SUBSPAWN-GRANT:" + minted + "]"
	}
	if a.trustPrefix != "" {
		affordance = strings.TrimPrefix(affordance, a.trustPrefix)
	}
	switch {
	case affordance == a.guard || strings.HasPrefix(affordance, a.guard+"\n\n"):
		a.updatedMessage = affordance
	case a.validItems && affordance == "":
		a.updatedMessage = a.guard
	default:
		a.updatedMessage = a.guard + "\n\n" + affordance
	}
	return a, "", false
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
