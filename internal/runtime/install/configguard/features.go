package configguard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// DeclaredFeature is a flag declared by CXC v0.2.40 config-guard/src/features.ts.
type DeclaredFeature string

const (
	FeatureMultiAgent                  DeclaredFeature = "multi_agent"
	FeatureGoals                       DeclaredFeature = "goals"
	FeatureHooks                       DeclaredFeature = "hooks"
	FeatureDefaultModeRequestUserInput DeclaredFeature = "default_mode_request_user_input"
)

// DeclaredFeatures preserves the oracle order and excludes multi_agent_v2.
func DeclaredFeatures() []DeclaredFeature {
	return []DeclaredFeature{FeatureMultiAgent, FeatureGoals, FeatureHooks, FeatureDefaultModeRequestUserInput}
}

// SoftFeatures names flags whose enable failure must be reported without failing activation.
func SoftFeatures() []DeclaredFeature {
	return []DeclaredFeature{FeatureDefaultModeRequestUserInput}
}

// SoftFeatureImpact is oracle runtime data, kept in its original language.
func SoftFeatureImpact() map[DeclaredFeature]string {
	return map[DeclaredFeature]string{
		FeatureDefaultModeRequestUserInput: "Default 모드에서 질문선택지 UI(request_user_input)가 모델에게 노출되지 않는다. " +
			"Plan 모드에서는 계속 동작한다.",
	}
}

// CodexRunResult is the injected runner's answer; these helpers never run a binary themselves.
type CodexRunResult struct {
	Stdout, Stderr string
	ExitCode       int
}

type CodexRunner func(args []string) CodexRunResult

// ParseFeaturesList reads an exact declared first field and a true/false last field.
// Middle fields are ignored, as in config-guard/src/features.ts:61-76.
func ParseFeaturesList(stdout string) map[string]bool {
	declared := DeclaredFeatures()
	result := make(map[string]bool)
	for _, line := range text.SplitLines(stdout) {
		fields := strings.FieldsFunc(line, tomlIsSpace)
		if len(fields) < 2 || !slices.Contains(declared, DeclaredFeature(fields[0])) {
			continue
		}
		switch strings.ToLower(fields[len(fields)-1]) {
		case "true":
			result[fields[0]] = true
		case "false":
			result[fields[0]] = false
		}
	}
	return result
}

// FeatureState is what "codex features list" says about a declared flag (CRW-1143).
type FeatureState string

// The observed states. Unsupported is a flag the CLI lists no row for (an upstream without it); Unavailable is a list that
// could not be read, which says nothing about any flag.
const (
	FeatureEnabled     FeatureState = "enabled"
	FeatureDisabled    FeatureState = "disabled"
	FeatureUnsupported FeatureState = "unsupported"
	FeatureUnavailable FeatureState = "unavailable"
)

// ParseFeatureStates reads the declared flags' rows: the exact name first, any stage words in the middle, and true or false
// last. A declared row that is cut short or ends in anything else, and two rows for one flag that disagree, are refused
// rather than read as disabled (the oracle's ParseFeaturesList dropped them silently, so a later or earlier row decided). A
// declared flag without a row is unsupported.
func ParseFeatureStates(stdout string) (map[string]FeatureState, error) {
	declared := DeclaredFeatures()
	seen := map[string]FeatureState{}
	for _, line := range text.SplitLines(stdout) {
		fields := strings.FieldsFunc(line, tomlIsSpace)
		if len(fields) == 0 || !slices.Contains(declared, DeclaredFeature(fields[0])) {
			continue
		}
		var state FeatureState
		switch last := fields[len(fields)-1]; {
		case len(fields) >= 2 && strings.EqualFold(last, "true"):
			state = FeatureEnabled
		case len(fields) >= 2 && strings.EqualFold(last, "false"):
			state = FeatureDisabled
		default:
			return nil, fmt.Errorf("codex features list has a row for %s that crw cannot read (%q), so the flag state is unknown", fields[0], text.Trim(line))
		}
		if prior, ok := seen[fields[0]]; ok && prior != state {
			return nil, fmt.Errorf("codex features list has conflicting rows for %s, so the flag state is unknown", fields[0])
		}
		seen[fields[0]] = state
	}
	states := map[string]FeatureState{}
	for _, key := range declared {
		state, ok := seen[string(key)]
		if !ok {
			state = FeatureUnsupported
		}
		states[string(key)] = state
	}
	return states, nil
}

// ReadFeatureStates calls only features/list.
func ReadFeatureStates(run CodexRunner) (map[string]FeatureState, error) {
	res := run([]string{"features", "list"})
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("codex features list failed (exit %d): %s", res.ExitCode, text.Trim(res.Stderr))
	}
	return ParseFeatureStates(res.Stdout)
}

// ReadDeclaredState is ReadFeatureStates as enabled or not: a flag without a row reads as not enabled, and a list crw cannot
// read is an error.
func ReadDeclaredState(run CodexRunner) (map[string]bool, error) {
	states, err := ReadFeatureStates(run)
	if err != nil {
		return nil, err
	}
	state := make(map[string]bool)
	for key, s := range states {
		state[key] = s == FeatureEnabled
	}
	return state, nil
}

// FeaturesToEnable returns only not-already-true flags in declaration order.
func FeaturesToEnable(currentState map[string]bool) []DeclaredFeature {
	result := make([]DeclaredFeature, 0)
	for _, key := range DeclaredFeatures() {
		if !currentState[string(key)] {
			result = append(result, key)
		}
	}
	return result
}
