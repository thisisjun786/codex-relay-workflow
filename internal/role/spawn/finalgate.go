package spawn

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// This file ports CXC v0.2.40's final-gate-guard.ts (subagent-config/src/final-gate-guard.ts, commit 3c1459ac, all 189 lines): the
// check that refuses a spawn packet which marks itself as the final-gate reviewer while no fresh test receipt (and, for a web, tui
// or desktop criterion, QA receipt) exists. It is an early warning and not the enforcement layer: the marker, the session state, a
// goalplan with a finalGate and a readable source tree all have to be there, and every other failure lets the spawn through. It
// reads files below cwd and the receipts the goalplan names, and writes nothing. The route that calls it is a later issue; nothing
// here registers a hook. The names are those of contract/schema/cxc/name-substitution.json. Each way the port differs from the
// oracle, and each defect it keeps, is recorded in docs/port-cxc/known-defects.md.

const (
	spawnFinalGateMarker = "[CRW-FINAL-GATE]"
	spawnFinalGatePrefix = "[crw — final gate]"
	spawnFinalGateSource = spawnFinalGatePrefix + " SOURCE-ROOT: source identity is unavailable; inspect the binding and installed modules before review."
	spawnFinalGateHeader = spawnFinalGatePrefix + " This spawn is marked as the final gate, but its prerequisites are not in place:"
	spawnFinalGateFooter = "Run the checks, record their receipts under " + crwdir.DirName + "/evidence/, then dispatch the gate reviewer."
)

// FinalGateCheck is the answer of CheckFinalGatePrereqs: OK, or a refusal and the text to deny the spawn with.
type FinalGateCheck struct {
	OK     bool
	Reason string
}

// spawnFinalGateSlot is one receipt the check asks for: its label in the refusal and the path the goalplan records for it.
type spawnFinalGateSlot struct {
	label string
	path  any
}

// CheckFinalGatePrereqs is checkFinalGatePrereqs. A packet without the marker, a call without a session id, a session whose state
// names no slug, and a goalplan that is missing or has no finalGate (an object or, as in the oracle, an array) all answer OK. Past
// those it asks for a receipt of the test run, and of the QA run when any criterion of the whole plan (not the active work phase)
// has the surface web, tui or desktop: a path that is not recorded, or a file that is missing, empty or holds no readable source
// identity, is missing; a receipt of another source tree than the current one is stale. The tree is the session's source work,
// resolved from its binding; one that cannot be resolved is a refusal, before any receipt is read.
//
// capture reads the identity of the source worktree it is given (the oracle's test seam). A given capture is asked about the root
// the session's binding names and its answer takes that root as its sourceRoot when the root is not cwd; nil reads the tree as the
// session's own capture does.
func CheckFinalGatePrereqs(packetText, sessionID, cwd string, capture func(cwd string) source.Identity) FinalGateCheck {
	allow := FinalGateCheck{OK: true}
	if !strings.Contains(packetText, spawnFinalGateMarker) || sessionID == "" {
		return allow
	}
	slug, _ := spawnFinalGateObject(state.StatePath(cwd, sessionID))["slug"].(string)
	if slug == "" {
		return allow
	}
	plan := spawnFinalGateObject(filepath.Join(cwd, crwdir.DirName, goalplan.GoalplansSubdir, slug, goalplan.GoalplanFile))
	var gate map[string]any
	switch g := plan["finalGate"].(type) {
	case map[string]any:
		gate = g
	case []any: // typeof [] is "object": the oracle goes on, with no receipt path to find in it
	default:
		return allow
	}
	slots := []spawnFinalGateSlot{{"test", gate["testReceiptPath"]}}
	criteria, _ := plan["criteria"].([]any)
	if slices.ContainsFunc(criteria, func(c any) bool {
		criterion, _ := c.(map[string]any)
		surface, _ := criterion["surface"].(string)
		return surface == "web" || surface == "tui" || surface == "desktop"
	}) {
		slots = append(slots, spawnFinalGateSlot{"QA", gate["qaReceiptPath"]})
	}
	current, resolved := spawnFinalGateCurrent(cwd, sessionID, capture)
	if !resolved {
		return FinalGateCheck{Reason: spawnFinalGateSource}
	}
	var missing, stale []string
	for _, slot := range slots {
		path, _ := slot.path.(string)
		if path == "" {
			missing = append(missing, slot.label+" receipt path is not recorded in finalGate")
		} else if identity, readable := spawnFinalGateReceipt(cwd, path); !readable {
			missing = append(missing, slot.label+" receipt is missing, empty or unreadable: "+path)
		} else if source.Compare(identity, current).Kind == source.ComparisonDifferent {
			stale = append(stale, slot.label+" receipt was produced against "+spawnFinalGateShort(identity)+", but the tree is now "+spawnFinalGateShort(current))
		}
	}
	if len(missing)+len(stale) == 0 {
		return allow
	}
	lines := []string{spawnFinalGateHeader}
	for _, line := range append(missing, stale...) {
		lines = append(lines, "  - "+line)
	}
	return FinalGateCheck{Reason: strings.Join(append(lines, spawnFinalGateFooter), "\n")}
}

// spawnFinalGateCurrent is the identity of the tree the receipts are compared with; resolved is false when it cannot be had, as
// when the oracle's capture throws, which a callback that panics does too.
func spawnFinalGateCurrent(cwd, sessionID string, capture func(cwd string) source.Identity) (current source.Identity, resolved bool) {
	defer func() {
		if recover() != nil {
			current, resolved = source.Identity{}, false
		}
	}()
	if capture == nil {
		captured, err := sourcesession.Capture(cwd, sessionID, sourcesession.CaptureOptions{})
		return captured, err == nil
	}
	sourceCwd, err := sourcesession.Resolve(cwd, sessionID)
	if err != nil {
		return source.Identity{}, false
	}
	current = capture(sourceCwd)
	if sourceCwd != cwd {
		current.SourceRoot = &sourceCwd
	}
	return current, true
}

// spawnFinalGateReceipt is the source identity a receipt file holds. The path is read as written when it is absolute and below cwd
// otherwise, links followed; a file that is not a regular non-empty one, or holds no readable identity, is not readable.
func spawnFinalGateReceipt(cwd, path string) (source.Identity, bool) {
	abs := filepath.Join(cwd, path)
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	}
	if info, err := os.Stat(abs); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return source.Identity{}, false
	}
	return spawnFinalGateIdentity(spawnFinalGateObject(abs)["sourceIdentity"])
}

// spawnFinalGateIdentity is readIdentity: a kind that is resolved or unavailable, a string commitSha and a boolean dirty, a treeHash
// when it is a string, and a sourceRoot that is absent or an absolute string (present as null it is refused).
func spawnFinalGateIdentity(raw any) (source.Identity, bool) {
	fields, _ := raw.(map[string]any)
	kind, _ := fields["kind"].(string)
	sha, shaOK := fields["commitSha"].(string)
	dirty, dirtyOK := fields["dirty"].(bool)
	if (kind != string(source.KindResolved) && kind != string(source.KindUnavailable)) || !shaOK || !dirtyOK {
		return source.Identity{}, false
	}
	identity := source.Identity{Kind: source.Kind(kind), CommitSha: sha, Dirty: dirty}
	identity.TreeHash, _ = fields["treeHash"].(string)
	if value, present := fields["sourceRoot"]; present {
		root, isString := value.(string)
		if !isString || !filepath.IsAbs(root) {
			return source.Identity{}, false
		}
		identity.SourceRoot = &root
	}
	return identity, true
}

// spawnFinalGateObject is readJson: the object a file holds as JSON.parse reads it, or nil when the file cannot be read, is not JSON
// or is not an object. Links are followed.
func spawnFinalGateObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := pyjson.Loads(source.DecodeUTF8(data), pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	return pyjson.Map(value)
}

// spawnFinalGateShort is the first seven UTF-16 code units of the commit, and "+dirty" for a tree with uncommitted changes.
func spawnFinalGateShort(identity source.Identity) string {
	short := spawnInlineUTF16Prefix(identity.CommitSha, 7)
	if identity.Dirty {
		short += "+dirty"
	}
	return short
}
