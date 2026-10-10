package spawn

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// This file ports the managed dispatch leg of CXC v0.2.40's runSpawnAttachHook: dispatchSources (:420-449) and the managed
// dispatch loop (:892-907), with the CRW names of contract/schema/cxc/name-substitution.json. The final-gate call and the
// issuance stay in hook_route.go. Nothing here registers, or is, a hook. The one departure is the trust-warning strip
// (:426-427): the oracle prefixes the message with the project layer's trust warning, which decision 7 removed, so the
// warning is always empty and the strip is the identity; it is recorded in docs/port-cxc/known-defects.md.

// spawnGrantInstruction is the coordinator guard's grant instruction, up to the capability. The mint site (hook.go) and the
// unwrap below share it so the recorded grant text stays in one place.
const spawnGrantInstruction = "\nOne child spawn is authorized. Include this exact one-time capability in that spawn message: "

// spawnDispatchSource is one dispatchSources entry: the source line, and the role whose promptOverride preceded it in an
// unwrapped guarded message. The role is nil only for a message that already opens with a dispatch marker.
type spawnDispatchSource struct {
	Source string
	Role   *role.RoleName
}

// spawnDispatchLine is the oracle's line(): the first line of s and no other.
func spawnDispatchLine(s string) string { return text.SplitLines(s)[0] }

// spawnDispatchSources is dispatchSources (:420-449). A message that opens with a dispatch marker is a direct source. A
// message wrapped in one of the four guard blocks is unwrapped (a coordinator guard's grant instruction is stripped first),
// and the roles whose promptOverride leads the remaining text each contribute a source, longest prompt first. Anything else
// has no source. The settings read is the global store (decision 7); a failure is the oracle's readSettings throw, which the
// caller turns into empty output.
func spawnDispatchSources(message string, env host.LookupEnv) ([]spawnDispatchSource, error) {
	return spawnDispatchSourcesWith(message, func() role.SettingsSnapshot { return spawnHookSettings(env) })
}

// spawnHookSettings reads the helper role settings; a hook event calls it at most once (CRW-1124), and a test counts the calls.
var spawnHookSettings = role.ReadSettingsSnapshot

// spawnDispatchSourcesWith is spawnDispatchSources over the event's settings snapshot, read only when a source needs it.
func spawnDispatchSourcesWith(message string, snapshot func() role.SettingsSnapshot) ([]spawnDispatchSource, error) {
	if strings.HasPrefix(message, "[CRW-DISPATCH:") {
		return []spawnDispatchSource{{Source: spawnDispatchLine(message)}}, nil
	}
	settings := snapshot()
	if _, err := settings.Role(role.Explorer); err != nil && !errors.As(err, new(*role.UnusableSettingsError)) {
		return nil, err // a store that cannot be found, as the oracle's readSettings throw
	}
	rest, unwrapped := message, false
	// The oracle's anchored, case-sensitive grant pattern, compiled here so the package keeps no initializer work.
	grant := regexp.MustCompile("^" + regexp.QuoteMeta(spawnGrantInstruction) + "\\[CRW-SUBSPAWN-GRANT:[a-f0-9]{64}\\]")
	for _, block := range []struct {
		text        string
		coordinator bool
	}{
		{V1ScopeBlock, false},
		{LeafGuardBlock, false},
		{V1ScopeBlockCoordinator, true},
		{LeafGuardBlockCoordinator, true},
	} {
		if !strings.HasPrefix(rest, block.text) {
			continue
		}
		tail := rest[len(block.text):]
		if block.coordinator {
			tail = grant.ReplaceAllString(tail, "")
		}
		if !strings.HasPrefix(tail, "\n\n") {
			continue
		}
		rest, unwrapped = tail[2:], true
		break
	}
	if !unwrapped {
		return nil, nil
	}
	// Each role's prompt decides which source a guarded message carries, so an unusable store or role leaves the managed routing
	// undecided when a dispatch marker could follow that role's prompt: the spawn is denied with the store's error (CRW-1119). Text
	// with no marker at its start or behind a blank line has no source whatever any prompt is, so an unusable role stops only its
	// own routing there.
	if strings.HasPrefix(rest, "[CRW-DISPATCH:") || strings.Contains(rest, "\n\n[CRW-DISPATCH:") {
		if err := settings.Err(); err != nil {
			return nil, err
		}
	}
	type entry struct {
		role   role.RoleName
		prompt string
	}
	entries := make([]entry, 0, len(role.Roles()))
	for _, r := range role.Roles() {
		prompt := ""
		if cfg, _ := settings.Role(r); cfg.PromptOverride != nil {
			p := cfg.PromptOverride
			prompt = text.Trim(*p)
		}
		entries = append(entries, entry{r, prompt})
	}
	// JavaScript's sort is stable and string.length counts UTF-16 units, so ties keep Roles order.
	slices.SortStableFunc(entries, func(a, b entry) int {
		return spawnInlineUTF16Units(b.prompt) - spawnInlineUTF16Units(a.prompt)
	})
	var sources []spawnDispatchSource
	for _, e := range entries {
		if e.prompt != "" && !strings.HasPrefix(rest, e.prompt+"\n\n") {
			continue
		}
		source := rest
		if e.prompt != "" {
			source = strings.TrimPrefix(rest, e.prompt+"\n\n")
		}
		if !strings.HasPrefix(source, "[CRW-DISPATCH:") {
			continue
		}
		name := e.role
		sources = append(sources, spawnDispatchSource{Source: spawnDispatchLine(source), Role: &name})
	}
	return sources, nil
}

// spawnHookManaged is the managed dispatch loop (:892-907). The first source that resolves and, for an unwrapped source,
// matches its role becomes the assembly's managed spawn. A full-history fork is denied outright. A run with sources but no
// match is denied "managed dispatch: <the remembered error, or the header text>"; a nil result is the oracle's thrown
// "invalid managed dispatch marker".
func spawnHookManaged(a *spawnHookAssembly, sources []spawnDispatchSource) (string, bool) {
	var dispatchError error
	resolver := role.NewManagedSpawnResolver(a.cwd) // the root and each source's record are looked at once per event (CRW-1124)
	for _, candidate := range sources {
		if IsFullHistoryFork(spawnHookView(a.toolInput)) {
			return DenyEnvelope("managed fallback requires a fresh context"), true
		}
		resolved, err := resolver.Preview(a.sessionID, candidate.Source)
		if err == nil && resolved == nil {
			err = errors.New("invalid managed dispatch marker")
		}
		if err != nil {
			dispatchError = err
			continue
		}
		if candidate.Role != nil && resolved.Role != *candidate.Role {
			continue
		}
		a.managed, a.dispatchSource = resolved, candidate.Source
		return "", false
	}
	if len(sources) == 0 {
		return "", false
	}
	message := "dispatch header does not match its configured role"
	if dispatchError != nil {
		message = spawnParityNodeError(dispatchError)
	}
	return DenyEnvelope("managed dispatch: " + message), true
}

// spawnHookWithout is a JSON object without the named keys, every other field keeping its place. It is the delete the oracle
// applies to updatedInput.model and updatedInput.reasoning_effort when the managed candidate's field is null; pyjson keeps no
// delete, and a loaded payload holds at most one field per key, so a filter is exact.
func spawnHookWithout(o pyjson.Object, keys ...string) pyjson.Object {
	out := make(pyjson.Object, 0, len(o))
	for _, field := range o {
		if !slices.Contains(keys, field.Key) {
			out = append(out, field)
		}
	}
	return out
}
