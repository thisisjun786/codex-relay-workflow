package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
)

// The divergence CLI: the argument layer over the divergence mode and candidate archive of
// internal/pabcd/metric, the Go form of CXC v0.2.40 pabcd-state/src/divergence-cli.ts (commit
// 3c1459ac) under the CRW names of contract/schema/cxc/name-substitution.json. Behaviour is ported
// as-is, the oracle's defects included (docs/port-cxc/known-defects.md, "Found by the divergence CLI
// port"), with one adaptation the result type forces: the oracle's mode write sits outside any try and
// its entry point reports the throw as "codexclaw cli failed: <message>" on stderr, exit 1, so this
// library returns that error to its caller (see RunDivergenceCli) instead of crashing.
//
// No command is registered here: CRW-547 adds the dispatcher row (crw pabcd divergence). The file has
// no package-level initializer.

// DivergenceCliResult carries the text and exit status without writing process streams.
type DivergenceCliResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// divergenceCliUsageText ports usage() (divergence-cli.ts:18-28): the fallback for a topic or verb the
// CLI does not know, code 1, no trailing newline.
const divergenceCliUsageText = `divergence: expected one of:
  divergence mode --session <id> on|off [--cwd <owner-root>] [--collapse P|D] [--reason <text>] [--json]
  divergence candidate add --session <id> [--cwd <owner-root>] --kind strong-1|add-1|alternative --title <text> --rationale <text> --source <url> [--source <url>...] [--status proposed|built|checked|kept|discarded] [--change-class parameter-tweak|branch-toggle|state-space-redesign|evaluator-change] [--killed-at-phase P|A|B|C|D] [--worktree <path>] [--json]
  divergence candidate list --session <id> [--cwd <owner-root>] [--json]`

// divergenceCliHelpText ports renderDivergenceHelp (:72-92) with the CRW names ("crw pabcd
// divergence", "crw-loop"), no trailing newline.
const divergenceCliHelpText = `crw pabcd divergence — record the deliberate divergence mode and its candidates

Usage:
  crw pabcd divergence mode on --session <id> --collapse P|D --reason <why> [--json]
  crw pabcd divergence mode off --session <id> --reason <why> [--json]
  crw pabcd divergence candidate add --session <id> --kind strong-1|add-1|alternative
      --change-class parameter-tweak|branch-toggle|state-space-redesign|evaluator-change
      --title <text> --rationale <text> --source <url> [--source <url>]...
      [--killed-at-phase P|A|B|C|D] [--json]
  crw pabcd divergence candidate list --session <id> [--json]
  crw pabcd divergence --help

Notes:
  Collapse EARLY at P for satisfy-spec work; collapse LATE at D for
  maximize-metric work where the local metric can deceive (crw-loop).
  Turn divergence off once the plateau is broken — it is a mode, not a state.
  Every candidate needs at least one --source: an unsourced candidate is a guess.`

// RenderDivergenceHelp is renderDivergenceHelp: the help text the dispatcher prints for a help token.
func RenderDivergenceHelp() string { return divergenceCliHelpText }

// divergenceCliUsage is usage(): the value carries code 1.
func divergenceCliUsage() DivergenceCliResult {
	return DivergenceCliResult{Output: divergenceCliUsageText, Code: 1}
}

// divergenceCliFlag ports readFlag (:30-34): the token after the flag's first occurrence; nil when the
// flag is absent or is the last token. "" is a value, as the oracle's "?? null" keeps it.
func divergenceCliFlag(argv []string, name string) *string {
	idx := slices.Index(argv, name)
	if idx == -1 || idx+1 >= len(argv) {
		return nil
	}
	value := argv[idx+1]
	return &value
}

// divergenceCliFlags ports readAllFlags (:36-42): every following token that is present and non-empty
// (the oracle's truthiness test drops "" and a token missing at the end).
func divergenceCliFlags(argv []string, name string) []string {
	out := []string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == name && i+1 < len(argv) && argv[i+1] != "" {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// divergenceCliHas ports hasFlag (:44-46): membership anywhere in argv.
func divergenceCliHas(argv []string, name string) bool { return slices.Contains(argv, name) }

// divergenceCliParseCollapse ports parseCollapse (:48-50); the parse* helpers return nil for an absent
// or unrecognised value, and pointers into the metric types otherwise.
func divergenceCliParseCollapse(raw *string) *metric.CollapsePoint {
	if raw == nil {
		return nil
	}
	value := metric.CollapsePoint(*raw)
	if value != metric.CollapseP && value != metric.CollapseD {
		return nil
	}
	return &value
}

// divergenceCliParseKind ports parseKind (:52-54).
func divergenceCliParseKind(raw *string) *metric.CandidateKind {
	if raw == nil {
		return nil
	}
	value := metric.CandidateKind(*raw)
	switch value {
	case metric.KindStrong1, metric.KindAdd1, metric.KindAlternative:
		return &value
	}
	return nil
}

// divergenceCliParseStatus ports parseStatus (:56-58).
func divergenceCliParseStatus(raw *string) *metric.CandidateStatus {
	if raw == nil {
		return nil
	}
	value := metric.CandidateStatus(*raw)
	switch value {
	case metric.StatusProposed, metric.StatusBuilt, metric.StatusChecked, metric.StatusKept, metric.StatusDiscarded:
		return &value
	}
	return nil
}

// divergenceCliParseChangeClass ports parseChangeClass (:60-62).
func divergenceCliParseChangeClass(raw *string) *metric.CandidateChangeClass {
	if raw == nil {
		return nil
	}
	value := metric.CandidateChangeClass(*raw)
	switch value {
	case metric.ChangeParameterTweak, metric.ChangeBranchToggle, metric.ChangeStateSpaceRedesign, metric.ChangeEvaluatorChange:
		return &value
	}
	return nil
}

// divergenceCliParseKilledAtPhase ports parseKilledAtPhase (:64-66).
func divergenceCliParseKilledAtPhase(raw *string) *metric.CandidateKilledAtPhase {
	if raw == nil {
		return nil
	}
	value := metric.CandidateKilledAtPhase(*raw)
	switch value {
	case metric.PhaseP, metric.PhaseA, metric.PhaseB, metric.PhaseC, metric.PhaseD:
		return &value
	}
	return nil
}

// divergenceCliSession ports readSession (:68-70): --session, else -s. The oracle's nullish coalescing
// falls through only on null, so an empty --session is returned rather than replaced.
func divergenceCliSession(argv []string) *string {
	if value := divergenceCliFlag(argv, "--session"); value != nil {
		return value
	}
	return divergenceCliFlag(argv, "-s")
}

// divergenceCliQuote is JSON.stringify of a string: metric's rowQuote owns this spelling for the
// archive rows (metrics.go), but that helper is unexported and this issue does not edit the metric
// package, so the wrapper's session id uses this copy, pinned to the encoder by
// TestDivergenceCliQuoteSpellsAsTheMetricEncoderDoes. An invalid UTF-8 byte is written as U+FFFD.
func divergenceCliQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// RunDivergenceCli ports runDivergenceCli (:94-165). The error is the oracle's one uncaught path: the
// mode write (:115) sits outside any try, and the entry point's unhandledRejection listener
// (cli.ts:530) reports it as "codexclaw cli failed: <message>" on stderr, exit 1 ("crw cli failed:"
// once the wiring calls it). Every other refusal is a result value, exactly as the oracle returns.
func RunDivergenceCli(argv []string, cwd string) (DivergenceCliResult, error) {
	cwdOut := cwd
	if value := divergenceCliFlag(argv, "--cwd"); value != nil {
		cwdOut = *value
	}
	topic, verb := "", ""
	if len(argv) > 0 {
		topic = argv[0]
	}
	if len(argv) > 1 {
		verb = argv[1]
	}
	asJSON := divergenceCliHas(argv, "--json")
	// The help check runs before the session guard (:100, before :104), so a help token needs no session.
	if len(argv) == 0 || topic == "help" || topic == "--help" || topic == "-h" {
		return DivergenceCliResult{Output: RenderDivergenceHelp(), Code: 0}, nil
	}
	session := divergenceCliSession(argv)
	if session == nil || *session == "" {
		return DivergenceCliResult{Output: "divergence: --session <id> is required", Code: 1}, nil
	}
	switch {
	case topic == "mode":
		return divergenceCliMode(cwdOut, *session, verb, argv, asJSON)
	case topic == "candidate" && verb == "add":
		return divergenceCliCandidateAdd(cwdOut, *session, argv, asJSON), nil
	case topic == "candidate" && verb == "list":
		return divergenceCliCandidateList(cwdOut, *session, asJSON), nil
	}
	return divergenceCliUsage(), nil
}

// divergenceCliMode ports the mode branch (:106-124): on/off writes, anything else reads.
func divergenceCliMode(cwdOut, sessionID, verb string, argv []string, asJSON bool) (DivergenceCliResult, error) {
	var state *bool
	switch verb {
	case "on":
		on := true
		state = &on
	case "off":
		off := false
		state = &off
	}
	if state == nil {
		mode, ok := metric.ReadDivergenceMode(cwdOut, sessionID)
		if asJSON {
			if !ok {
				return DivergenceCliResult{Output: "{\"mode\":null}", Code: 0}, nil
			}
			return DivergenceCliResult{Output: "{\"mode\":" + metric.EncodeMode(mode) + "}", Code: 0}, nil
		}
		if !ok {
			return DivergenceCliResult{Output: "divergence mode: unset", Code: 0}, nil
		}
		return DivergenceCliResult{Output: divergenceCliModeText(mode), Code: 0}, nil
	}
	// An invalid or missing --collapse becomes "D" (parseCollapse(...) ?? "D", :113).
	collapsePoint := metric.CollapseD
	if parsed := divergenceCliParseCollapse(divergenceCliFlag(argv, "--collapse")); parsed != nil {
		collapsePoint = *parsed
	}
	reason := "resolved"
	if *state {
		reason = "plateau"
	}
	if value := divergenceCliFlag(argv, "--reason"); value != nil {
		reason = *value
	}
	mode, err := metric.WriteDivergenceMode(cwdOut, metric.ModeInput{SessionID: sessionID, Active: *state, CollapsePoint: collapsePoint, Reason: reason})
	if err != nil {
		return DivergenceCliResult{}, err
	}
	if asJSON {
		return DivergenceCliResult{Output: metric.EncodeMode(mode), Code: 0}, nil
	}
	return DivergenceCliResult{Output: divergenceCliModeText(mode), Code: 0}, nil
}

// divergenceCliModeText is the text form of a mode (:112, :123).
func divergenceCliModeText(mode metric.DivergenceMode) string {
	onOff := "off"
	if mode.Active {
		onOff = "on"
	}
	return fmt.Sprintf("divergence mode: %s collapse=%s", onOff, mode.CollapsePoint)
}

// divergenceCliCandidateAdd ports the candidate add branch (:120-155): the oracle's validation order
// (kind, change-class, killed-at-phase, title, rationale, source), then the archive write. An empty
// --change-class or --killed-at-phase is absent, not an error (:131, :134 test the raw value first).
func divergenceCliCandidateAdd(cwdOut, sessionID string, argv []string, asJSON bool) DivergenceCliResult {
	kind := divergenceCliParseKind(divergenceCliFlag(argv, "--kind"))
	// An invalid --status becomes "proposed" (parseStatus(...) ?? "proposed", :122).
	status := metric.StatusProposed
	if parsed := divergenceCliParseStatus(divergenceCliFlag(argv, "--status")); parsed != nil {
		status = *parsed
	}
	changeClassRaw := divergenceCliFlag(argv, "--change-class")
	killedAtPhaseRaw := divergenceCliFlag(argv, "--killed-at-phase")
	changeClass := divergenceCliParseChangeClass(changeClassRaw)
	killedAtPhase := divergenceCliParseKilledAtPhase(killedAtPhaseRaw)
	title := divergenceCliFlag(argv, "--title")
	rationale := divergenceCliFlag(argv, "--rationale")
	sourceURLs := divergenceCliFlags(argv, "--source")
	if kind == nil {
		return DivergenceCliResult{Output: "divergence candidate add: --kind strong-1|add-1|alternative is required", Code: 1}
	}
	if changeClassRaw != nil && *changeClassRaw != "" && changeClass == nil {
		return DivergenceCliResult{Output: "divergence candidate add: --change-class parameter-tweak|branch-toggle|state-space-redesign|evaluator-change is required", Code: 1}
	}
	if killedAtPhaseRaw != nil && *killedAtPhaseRaw != "" && killedAtPhase == nil {
		return DivergenceCliResult{Output: "divergence candidate add: --killed-at-phase P|A|B|C|D is required", Code: 1}
	}
	if title == nil || *title == "" {
		return DivergenceCliResult{Output: "divergence candidate add: --title <text> is required", Code: 1}
	}
	if rationale == nil || *rationale == "" {
		return DivergenceCliResult{Output: "divergence candidate add: --rationale <text> is required", Code: 1}
	}
	if len(sourceURLs) == 0 {
		return DivergenceCliResult{Output: "divergence candidate add: at least one --source <url> is required", Code: 1}
	}
	worktree := ""
	if value := divergenceCliFlag(argv, "--worktree"); value != nil {
		worktree = *value
	}
	candidate, err := metric.RecordDivergenceCandidate(cwdOut, metric.CandidateInput{
		SessionID: sessionID, Kind: *kind, Title: *title, Rationale: *rationale, SourceURLs: sourceURLs,
		Status: &status, Worktree: worktree, ChangeClass: changeClass, KilledAtPhase: killedAtPhase,
	})
	if err != nil {
		return DivergenceCliResult{Output: "divergence candidate add: " + err.Error(), Code: 1}
	}
	if asJSON {
		return DivergenceCliResult{Output: metric.EncodeCandidate(candidate), Code: 0}
	}
	return DivergenceCliResult{Output: fmt.Sprintf("divergence candidate: %s (%s) sources=%d", candidate.ID, candidate.Kind, len(candidate.SourceURLs)), Code: 0}
}

// divergenceCliCandidateList ports the candidate list branch (:157-162).
func divergenceCliCandidateList(cwdOut, sessionID string, asJSON bool) DivergenceCliResult {
	candidates := metric.ReadDivergenceCandidates(cwdOut, sessionID)
	if asJSON {
		rows := make([]string, len(candidates))
		for i, candidate := range candidates {
			rows[i] = metric.EncodeCandidate(candidate)
		}
		return DivergenceCliResult{Output: "{\"sessionId\":" + divergenceCliQuote(sessionID) + ",\"candidates\":[" + strings.Join(rows, ",") + "]}", Code: 0}
	}
	if len(candidates) == 0 {
		return DivergenceCliResult{Output: "divergence candidate list: no candidates for session " + sessionID, Code: 0}
	}
	lines := make([]string, len(candidates))
	for i, candidate := range candidates {
		lines[i] = fmt.Sprintf("%s %s %s sources=%d", candidate.ID, candidate.Kind, candidate.Status, len(candidate.SourceURLs))
	}
	return DivergenceCliResult{Output: strings.Join(lines, "\n"), Code: 0}
}
