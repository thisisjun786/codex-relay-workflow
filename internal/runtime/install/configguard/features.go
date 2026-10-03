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

// ReadDeclaredState calls only features/list and treats unseen declared flags as disabled.
func ReadDeclaredState(run CodexRunner) (map[string]bool, error) {
	res := run([]string{"features", "list"})
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("codex features list failed (exit %d): %s", res.ExitCode, text.Trim(res.Stderr))
	}
	parsed := ParseFeaturesList(res.Stdout)
	state := make(map[string]bool)
	for _, key := range DeclaredFeatures() {
		state[string(key)] = parsed[string(key)]
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
