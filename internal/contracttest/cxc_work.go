package contracttest

import (
	"fmt"
	"maps"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// workCLIClaim applies CRW-1527's public CLI delta after the older port claims.
// The oracle, hook answers, JSON output, observed files and exit codes stay intact.
// Legacy invocations still run: their one warning is compared, never discarded.
func workCLIClaim(s cxccorpus.Scenario, want map[string]string, claim cxcClaim) cxcClaim {
	claim.Set = maps.Clone(claim.Set)
	if claim.Set == nil {
		claim.Set = map[string]string{}
	}
	names := strings.NewReplacer(
		"crw pabcd orchestrate", "crw work state",
		"crw pabcd loop", "crw work continue",
		"crw pabcd plan", "crw work outline",
		"crw pabcd review-round", "crw work assessment",
		"crw pabcd", "crw work",
		"agent-gated IPABCD phase control", "agent-gated task state control",
		"multi-cycle PABCD loop", "multi-cycle task work",
		"changes the PABCD phase", "changes the task phase",
	)
	rootNames := strings.NewReplacer("recall,pabcd,role", "recall,work,role", "'recall', 'pabcd', 'role'", "'recall', 'work', 'role'")
	value := func(key string) (string, bool) {
		if v, ok := claim.Set[key]; ok {
			return v, true
		}
		for _, prefix := range claim.Remove {
			if strings.HasPrefix(key, prefix) {
				return "", false
			}
		}
		v, ok := want[key]
		return v, ok
	}
	for i, step := range s.Steps {
		if step.Hook != "" || step.MCP != nil || step.Node != nil || step.Write != nil || step.Wait != "" {
			continue
		}
		legacy := len(step.CLI) > 0 && step.CLI[0] == "pabcd"
		rename := rootNames.Replace
		if legacy {
			rename = names.Replace
		}
		prefix := fmt.Sprintf("steps/%d/", i)
		if form, _ := value(prefix + "form"); form == "text" {
			if out, ok := value(prefix + "stdout"); ok {
				claim.Set[prefix+"stdout"] = rename(out)
			}
		}
		if errOut, ok := value(prefix + "stderr"); ok {
			claim.Set[prefix+"stderr"] = rename(errOut)
			if !legacy {
				continue
			}
			replacement := "crw work"
			if len(step.CLI) > 1 && !strings.HasPrefix(step.CLI[1], "-") {
				replacement = names.Replace("crw pabcd " + step.CLI[1])
			}
			claim.Set[prefix+"stderr"] = fmt.Sprintf("warning: crw pabcd is deprecated; use %q (alias removed in the next major version)\n", replacement) + names.Replace(errOut)
		}
	}
	return claim
}
