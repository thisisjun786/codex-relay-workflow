package migrate

// inventory.go holds the inventory of docs/port-cxc/state-migration.md as data: one row per design table line for the project
// tables (retained and skipped), the user table (U mapped to V) and the Codex-home table, plus the fifth table of locks and
// intermediates as lock rows and the producer-intermediate rule. classify.go walks a selected scope through these rows. Nothing
// here writes: a row says what a path is, and the planner holds no write handle.

import (
	"crypto/sha256"
	"io/fs"
	"strings"
	"unicode/utf8"
)

// Item is one row of the dry plan, in the fields the design's JSON output names. Source and Destination are root-relative paths
// with "/" separators; Source is "." for a scope's root and "" for a destination-side note (a temporary an older run left where
// this run's destination is). Destination is "" when nothing is written (every skip row). Size, Digest and Mode are filled for a
// copy or transform row from the source file, read once, read-only; a directory row carries its mode only.
type Item struct {
	Scope       Scope
	Source      string
	Destination string
	Disposition Disposition
	Reason      string
	Size        int64
	Digest      [sha256.Size]byte
	Mode        fs.FileMode
}

// Plan is the ordered classification of a selected scope; on a refusal it holds the items examined before the refusal.
type Plan struct {
	Items []Item
}

// The reasons this slice adds to types.go's M1 set.
const (
	ReasonLocked     Reason = "locked"     // a lock is present: the state may be in use
	ReasonActive     Reason = "active"     // an unresolved dispatch attempt or a running/unknown BG job
	ReasonUnreadable Reason = "unreadable" // a retained record cannot be read, or is not the shape its writer makes
)

// The reason texts the plan reports.
const (
	inventoryReasonIntermediate = "producer intermediate; not durable state"
	inventoryReasonOldTemp      = "temporary of an older run; reported, not touched"
	inventoryReasonUnknown      = "not in the inventory"
	inventoryReasonContainer    = "container of listed entries; no state of its own"
	inventoryReasonFallback     = "excluded messenger service state; the literal fallback home ignores the override"
)

// inventoryRow is one line of the design's tables as data. pattern is root-relative: literal segments, "*" wildcarding any run
// inside one segment, and a trailing "**" meaning the subtree below that path. disp is what the path is; reason is the report
// text; judge names the record whose state decides the disposition ("dispatch", "bg" or ""); lock marks the fifth table (its
// presence refuses the scope). Table order is precedence: the first matching row claims a path.
type inventoryRow struct {
	pattern string
	disp    Disposition
	reason  string
	judge   string
	lock    bool
}

// inventoryProjectRows is the project table (W/.codexclaw -> W/.crw) with the fifth table's project lock rows first.
var inventoryProjectRows = []inventoryRow{
	{pattern: "sessions/*.json.lock", lock: true, reason: "a session lock is present; the state may be in use"},
	{pattern: "goalplans/*/.goalplan.lock", lock: true, reason: "a goalplan write lock is present, with or without owner.json"},
	{pattern: "dispatches/*/*.json.lock", lock: true, reason: "a dispatch lock is present, never broken"},
	{pattern: ".gitignore", disp: DispSkip, reason: "CXC text; CRW publishes its own canonical text no-replace"},
	{pattern: "sessions/*.json", disp: DispCopy},
	{pattern: "ledger.jsonl", disp: DispCopy},
	{pattern: "interviews/*.jsonl", disp: DispCopy},
	{pattern: "interview/freeze.json", disp: DispCopy},
	{pattern: "plan/*/**", disp: DispCopy, reason: "user plan tree; a .tmp name inside it is ordinary data"},
	{pattern: "goalplans/*/goalplan.json", disp: DispCopy},
	{pattern: "goalplans/*/ledger.jsonl", disp: DispCopy},
	{pattern: "goalplans/*/schema-v2.marker", disp: DispCopy},
	{pattern: "evidence/**", disp: DispCopy, reason: "artifact group; a .tmp name inside it is ordinary data"},
	{pattern: "evidence-attempts/*.json", disp: DispCopy},
	{pattern: "evidence-unrecordable/*.json", disp: DispCopy},
	{pattern: "sources/*.json", disp: DispCopy},
	{pattern: "metrics.jsonl", disp: DispCopy},
	{pattern: "objective-kind/*.json", disp: DispCopy},
	{pattern: "divergence/*.mode.json", disp: DispCopy},
	{pattern: "divergence/candidates.jsonl", disp: DispCopy},
	{pattern: "render-observations.jsonl", disp: DispCopy},
	{pattern: "dispatches/*/*.json", disp: DispCopy, judge: "dispatch"},
	{pattern: "bg/*.json", disp: DispCopy, judge: "bg"},
	{pattern: "bg/*.out", disp: DispCopy},
	{pattern: "bg/*.exit", disp: DispCopy},
	{pattern: "bg/*.out.helper.cjs", disp: DispSkip, reason: "generated Node execution helper; the Windows activation is excluded"},
	{pattern: "bg/disabled", disp: DispCopy},
	{pattern: "bg/enabled-at", disp: DispCopy},
	{pattern: "bg/ledger.jsonl", disp: DispCopy},
	{pattern: "attest.json", disp: DispCopy},
	{pattern: "subagents.json", disp: DispSkip, reason: "the project role layer is removed; the global layer replaces it"},
	{pattern: "worktree-guard/*.json", disp: DispSkip, reason: "rebuildable injection marker"},
	{pattern: "affordance-recovery/*.pending", disp: DispSkip, reason: "once-only queued advisory is regenerated, not resumed as evidence"},
	{pattern: "friction.jsonl", disp: DispSkip, reason: "dormant hook layer was not ported; no Go store reader"},
	{pattern: "edit-shapes.jsonl", disp: DispSkip, reason: "dormant ledger is expressly excluded from the port"},
	{pattern: "rules/*.md", disp: DispSkip, reason: "rule injection was not ported; the user files stay in the source"},
	{pattern: "release/**", disp: DispSkip, reason: "upstream release tooling has no CRW state consumer"},
	{pattern: "traces/activations.jsonl", disp: DispSkip, reason: "unported trace layer"},
	{pattern: "cache/repomap/tags.v1/**", disp: DispSkip, reason: "derived diskcache data; a custom cache location is not discovered"},
	{pattern: "bridge.db", disp: DispSkip, reason: "excluded messenger bridge store; a live database is never copied"},
	{pattern: "bridge.db-wal", disp: DispSkip, reason: "excluded messenger bridge store"},
	{pattern: "bridge.db-shm", disp: DispSkip, reason: "excluded messenger bridge store"},
	{pattern: "bridge-events.jsonl*", disp: DispSkip, reason: "excluded messenger event-log layer"},
}

// inventoryUserRows is the user table (U -> V). The serve.* rows are the literal fallback home: the messenger service ignores the
// override, so classifyUser also enumerates the literal home when U differs (the same reason text, a never-copied item).
var inventoryUserRows = []inventoryRow{
	{pattern: "subagents.json", disp: DispCopy},
	{pattern: "config.json", disp: DispCopy},
	{pattern: "model-catalog.json", disp: DispSkip, reason: "environment-keyed catalog cache is rebuilt"},
	{pattern: "recall/**", disp: DispSkip, reason: "derived search index; a cold rebuild is deliberate"},
	{pattern: "skill-cache/**", disp: DispSkip, reason: "TTL cache can be fetched again"},
	{pattern: "serve.out.log", disp: DispSkip, reason: inventoryReasonFallback},
	{pattern: "serve.err.log", disp: DispSkip, reason: inventoryReasonFallback},
	{pattern: "serve.cmd", disp: DispSkip, reason: inventoryReasonFallback},
	{pattern: "venvs/repomap/**", disp: DispSkip, reason: "rebuildable interpreter environment, not durable state"},
	{pattern: "runtime/ast-grep/*/sg", disp: DispSkip, reason: "executable installation cache, outside the state copy"},
	{pattern: "runtime/ast-grep/*/sg.exe", disp: DispSkip, reason: "executable installation cache, outside the state copy"},
}

// inventoryCodexRows is the Codex-home table, mapped in place: CodexLeaf decides the transforms before these rows, and the lock
// row is the fifth table's Codex entry.
var inventoryCodexRows = []inventoryRow{
	{pattern: "agents/.*-update.lock", lock: true, reason: "a role registration lock is present; the Codex scope refuses"},
	{pattern: "config.toml", disp: DispSkip, reason: "migration neither rewrites configuration nor grants hook trust"},
	{pattern: "config.toml.bak-*", disp: DispSkip, reason: "retrust backup already remains in the unchanged Codex home"},
	{pattern: "agents/*.toml", disp: DispSkip, reason: "role registration owns the shared host files"},
	{pattern: "agents/*.toml.backup-*", disp: DispSkip, reason: "role registration backup"},
	{pattern: "codexclaw/subagents.json", disp: DispSkip, reason: "a removed layer is not imported; no v0.2.40 writer"},
	{pattern: "codexclaw/hook-observations/**", disp: DispSkip, reason: "rebuildable invocation diagnostics of the old plugin"},
	{pattern: "memories_*.sqlite", disp: DispSkip, reason: "the host-owned database stays in place"},
	{pattern: "memories_*.sqlite-wal", disp: DispSkip, reason: "host-owned database sidecar stays in place"},
	{pattern: "memories_*.sqlite-shm", disp: DispSkip, reason: "host-owned database sidecar stays in place"},
	{pattern: "memories_*.sqlite-journal", disp: DispSkip, reason: "host-owned database sidecar stays in place"},
	{pattern: "runtime/ast-grep/**", disp: DispSkip, reason: "executable installation is outside migration"},
}

// inventoryMatch reports whether a root-relative path matches a row pattern.
func inventoryMatch(pattern, path string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(ps) == 0 || len(xs) == 0 || pattern == "" || path == "" {
		return false
	}
	subtree := ps[len(ps)-1] == "**"
	if subtree {
		ps = ps[:len(ps)-1]
	}
	if len(xs) < len(ps) || !subtree && len(xs) != len(ps) {
		return false
	}
	for i, seg := range ps {
		if !inventorySegment(seg, xs[i]) {
			return false
		}
	}
	return true
}

// inventorySegment matches one segment pattern against one name; "*" matches any run of characters, possibly none.
func inventorySegment(pattern, name string) bool {
	for {
		i := strings.IndexByte(pattern, '*')
		if i < 0 {
			return pattern == name
		}
		if !strings.HasPrefix(name, pattern[:i]) {
			return false
		}
		name, pattern = name[i:], pattern[i+1:]
		if pattern == "" {
			return true
		}
		if j := strings.IndexByte(pattern, '*'); j < 0 {
			return strings.HasSuffix(name, pattern) && len(name) >= len(pattern)
		} else {
			chunk := pattern[:j]
			k := strings.Index(name, chunk)
			if k < 0 {
				return false
			}
			name, pattern = name[k+len(chunk):], pattern[j:]
		}
	}
}

// inventoryUnder reports whether a row pattern can match a strict descendant of dir, so dir is a container to traverse. An
// exact pattern matches one path, so a directory at or below its depth is not under it; a subtree pattern reaches every path
// below its fixed prefix, so a directory at or below that depth is.
func inventoryUnder(pattern, dir string) bool {
	ps := strings.Split(pattern, "/")
	subtree := ps[len(ps)-1] == "**"
	if subtree {
		ps = ps[:len(ps)-1]
	}
	xs := strings.Split(dir, "/")
	if dir == "" || len(ps) == 0 || len(xs) >= len(ps) && !subtree {
		return false
	}
	for i, seg := range xs {
		if i == len(ps) {
			break
		}
		if !inventorySegment(ps[i], seg) {
			return false
		}
	}
	return true
}

// inventoryMatchRow returns the first row of the table that claims path.
func inventoryMatchRow(rows []inventoryRow, path string) *inventoryRow {
	for i := range rows {
		if inventoryMatch(rows[i].pattern, path) {
			return &rows[i]
		}
	}
	return nil
}

// inventoryTraverses reports whether any row of the table reaches below dir.
func inventoryTraverses(rows []inventoryRow, dir string) bool {
	for i := range rows {
		if inventoryUnder(rows[i].pattern, dir) {
			return true
		}
	}
	return false
}

// inventoryCopies reports whether a copy or transform row reaches below dir, so the container itself is a copy. A lock row
// transfers nothing, so it never makes its container a copy.
func inventoryCopies(rows []inventoryRow, dir string) bool {
	for i := range rows {
		if (rows[i].disp == DispCopy || rows[i].disp == DispTransform) && inventoryUnder(rows[i].pattern, dir) {
			return true
		}
	}
	return false
}

// inventoryNameSupported refuses a name this slice would write: a name the destination cannot hold as one path component, is not
// text, or carries a control byte is ReasonUnsupported. A name an entry merely reports (a skip row or an unknown child) is never
// refused for its spelling. The design names the refusal in its preflight list without defining it; this reading is disclosed.
func inventoryNameSupported(path, name string) error {
	switch {
	case len(name) > 255:
		return refuse(ReasonUnsupported, path, "the name is longer than one path component may be")
	case !utf8.ValidString(name):
		return refuse(ReasonUnsupported, path, "the name is not valid UTF-8")
	case strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return refuse(ReasonUnsupported, path, "the name carries a control byte")
	}
	return nil
}

// inventoryIntermediateRow is one producer temporary of docs/port-cxc/state-migration.md:105 as data: the scope the producer
// writes in, the root-relative directory it writes in ("" is a scope root, "*" matches any run inside one segment) and the
// exact basename shape it writes beside its final file. A name is a producer intermediate only when its scope and directory
// match the row and its basename matches that row's shape.
type inventoryIntermediateRow struct {
	scope Scope
	dir   string
	match func(name string) bool
}

// inventoryIntermediateRows names every producer the design's fifth table lists, with the source line that writes its
// temporary. The first row whose scope, directory and shape all match claims the name; the two user-root rows share a shape
// because subagents.json and model-catalog.json are written by the same writer pattern, and their comments name the file each
// covers.
var inventoryIntermediateRows = []inventoryIntermediateRow{
	// state.ts:387 publishes sessions/<id>.json through <finalPath>.<pid>.<uuid>.tmp.
	{ScopeProject, "sessions", inventoryIntermediateFinalPidUUID},
	// state.ts:625 rewrites sessions/<id>.json through <finalPath>.<pid>.<ms>.tmp.
	{ScopeProject, "sessions", inventoryIntermediateFinalPidMs},
	// goalplan.ts:932 writes goalplans/<slug>/goalplan.json through <finalPath>.<pid>.<ms>.tmp.
	{ScopeProject, "goalplans/*", inventoryIntermediateFinalPidMs},
	// session-source.ts:188 writes sources/<session>.json through <path>.<uuid>.tmp.
	{ScopeProject, "sources", inventoryIntermediateFinalUUID},
	// subagent-evidence.ts:222 writes evidence-attempts/<...>.json through <p>.<pid>.<ms>.tmp.
	{ScopeProject, "evidence-attempts", inventoryIntermediateFinalPidMs},
	// subagent-evidence.ts:355 probes evidence-unrecordable/ with .probe-<pid>-<ms>.
	{ScopeProject, "evidence-unrecordable", inventoryIntermediateProbePidMs},
	// bg-wake store.ts:55 writes any bg/<path> through <path>.tmp-<pid>-<ms>.
	{ScopeProject, "bg", inventoryIntermediateFinalTmpPidMs},
	// bg-wake spawn.ts:50 writes bg/<id>.exit through <exitPath>.tmp, that is <id>.exit.tmp.
	{ScopeProject, "bg", inventoryIntermediateFinalExitTmp},
	// subagent-config store.ts:256 writes the project store subagents.json through <storePath>.<uuid>.tmp.
	{ScopeProject, "", inventoryIntermediateFinalUUID},
	// subagent-config store.ts:256 writes the user store subagents.json through <globalStorePath>.<uuid>.tmp.
	{ScopeUser, "", inventoryIntermediateFinalUUID},
	// subagent-config live-catalog.ts:86 writes the user model-catalog.json through <path>.<uuid>.tmp.
	{ScopeUser, "", inventoryIntermediateFinalUUID},
	// config-guard self-heal.ts:275,310 write the Codex-home <marker>.json (and the install manifest) through <path>.tmp.
	{ScopeCodex, "", inventoryIntermediateFinalJSONTmp},
	// cxc-ops hook-trust.ts:363 writes the Codex-home config.toml through .config.toml.tmp-<pid>-<ms>.
	{ScopeCodex, "", inventoryIntermediateConfigTomlTmpPidMs},
	// subagent-config role-registration.ts:88 writes agents/<role>.toml through .<role>-<uuid>.tmp.
	{ScopeCodex, "agents", inventoryIntermediateDotRoleUUIDTmp},
}

// inventoryIntermediateShape reports whether name, in dirPath of scope, is a producer temporary. The directory is matched with
// the inventory row matcher, so a row's "*" wildcards one segment and a scope root is the empty directory.
func inventoryIntermediateShape(scope Scope, dirPath, name string) bool {
	for i := range inventoryIntermediateRows {
		row := &inventoryIntermediateRows[i]
		if row.scope != scope || !inventoryIntermediateDir(row.dir, dirPath) || !row.match(name) {
			continue
		}
		return true
	}
	return false
}

// inventoryIntermediateDir matches a row's directory pattern against the directory the walker is in; the empty pattern is the
// scope root and any other pattern is the inventory segment matcher (literal segments and "*").
func inventoryIntermediateDir(pattern, dirPath string) bool {
	if pattern == "" {
		return dirPath == ""
	}
	return inventoryMatch(pattern, dirPath)
}

// The producer temporary shapes. Each is the exact basename a producer writes beside its final file, with the final-name part
// required to be non-empty, so a user file such as .123.1760000000000.tmp stays ordinary data. A digit field matches digits
// only and a uuid field the canonical uuid shape, as the design's row 105 asks; the bounded pid and millisecond widths stay in
// classifyProducerTemp, which owns the evidence rule.

// inventoryIntermediateDigits reports a non-empty run of ASCII digits.
func inventoryIntermediateDigits(s string) bool { return classifyDigits(s, 1, len(s)) }

// inventoryIntermediateFinalPidMs is <final>.<pid>.<ms>.tmp.
func inventoryIntermediateFinalPidMs(name string) bool {
	rest, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	rest, ms, cut := classifyCutLast(rest, ".")
	if !cut || !inventoryIntermediateDigits(ms) {
		return false
	}
	final, pid, cut := classifyCutLast(rest, ".")
	return cut && final != "" && inventoryIntermediateDigits(pid)
}

// inventoryIntermediateFinalPidUUID is <final>.<pid>.<uuid>.tmp.
func inventoryIntermediateFinalPidUUID(name string) bool {
	rest, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	rest, uuid, cut := classifyCutLast(rest, ".")
	if !cut || !classifyUUID(uuid) {
		return false
	}
	final, pid, cut := classifyCutLast(rest, ".")
	return cut && final != "" && inventoryIntermediateDigits(pid)
}

// inventoryIntermediateFinalUUID is <final>.<uuid>.tmp.
func inventoryIntermediateFinalUUID(name string) bool {
	rest, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	final, uuid, cut := classifyCutLast(rest, ".")
	return cut && final != "" && classifyUUID(uuid)
}

// inventoryIntermediateFinalTmpPidMs is <final>.tmp-<pid>-<ms>, the bg store's atomic write.
func inventoryIntermediateFinalTmpPidMs(name string) bool {
	i := strings.LastIndexByte(name, '-')
	if i < 0 {
		return false
	}
	rest, ms := name[:i], name[i+1:]
	if !inventoryIntermediateDigits(ms) {
		return false
	}
	j := strings.LastIndexByte(rest, '-')
	if j < 0 {
		return false
	}
	final, ok := strings.CutSuffix(rest[:j], tempSuffix)
	return ok && final != "" && inventoryIntermediateDigits(rest[j+1:])
}

// inventoryIntermediateProbePidMs is .probe-<pid>-<ms>, the unrecordable-directory writability probe.
func inventoryIntermediateProbePidMs(name string) bool {
	rest, ok := strings.CutPrefix(name, ".probe-")
	if !ok {
		return false
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return false
	}
	return inventoryIntermediateDigits(rest[:i]) && inventoryIntermediateDigits(rest[i+1:])
}

// inventoryIntermediateFinalExitTmp is <final>.exit.tmp, the bg exit record's temporary.
func inventoryIntermediateFinalExitTmp(name string) bool {
	final, ok := strings.CutSuffix(name, ".exit"+tempSuffix)
	return ok && final != ""
}

// inventoryIntermediateFinalJSONTmp is <final>.json.tmp, the Codex-home marker and manifest temporary.
func inventoryIntermediateFinalJSONTmp(name string) bool {
	final, ok := strings.CutSuffix(name, ".json"+tempSuffix)
	return ok && final != ""
}

// inventoryIntermediateConfigTomlTmpPidMs is .config.toml.tmp-<pid>-<ms>, the hook-trust temporary.
func inventoryIntermediateConfigTomlTmpPidMs(name string) bool {
	rest, ok := strings.CutPrefix(name, ".config.toml.tmp-")
	if !ok {
		return false
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return false
	}
	return inventoryIntermediateDigits(rest[:i]) && inventoryIntermediateDigits(rest[i+1:])
}

// inventoryIntermediateDotRoleUUIDTmp is .<role>-<uuid>.tmp, the role-registration temporary. The uuid is the last 36
// characters, so the separator is found from the right rather than at the last "-" inside the uuid.
func inventoryIntermediateDotRoleUUIDTmp(name string) bool {
	rest, ok := strings.CutSuffix(name, tempSuffix)
	if !ok {
		return false
	}
	role, ok := strings.CutPrefix(rest, ".")
	if !ok || len(role) < 38 {
		return false
	}
	sep := len(role) - 37
	return role[sep] == '-' && role[:sep] != "" && classifyUUID(role[sep+1:])
}
