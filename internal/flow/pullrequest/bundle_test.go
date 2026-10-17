package pullrequest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// TestBundleBodyCarriesTheCloseEvidence (CRW-1423): the pull request body of a bundle states the close's own gate evidence —
// the kind, command, state and digest the gate recorded — not text built from the commands alone.
func TestBundleBodyCarriesTheCloseEvidence(t *testing.T) {
	digest := "9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e7d6c5b4a9f8e"
	gate, _ := json.Marshal(ops.GateOutcome{Milestone: "M1", Plan: "plan-1", Head: strings.Repeat("a", 40), State: ops.GatePass,
		Commands: []string{"go test ./..."},
		Evidence: []ops.Evidence{{Kind: "test", Command: "go test ./...", State: "pass", Digest: digest}}})
	cc, _ := json.Marshal(map[string]any{"verdict": "verified", "head": strings.Repeat("a", 40), "milestone": "M1", "engine_family": "gpt", "model": "gpt-6-astra", "round": 1})
	body := Body(bundleBody(ops.BundlePROpenRequest{
		Milestone: ops.Milestone{ID: "M1", Plan: "plan-1"},
		Close:     ops.CloseResultOutcome{Milestone: "M1", Base: "dev", Tip: strings.Repeat("a", 40), Gate: gate, Crosscheck: cc, GateHead: strings.Repeat("a", 40)},
	}), []tracking.Ref{{Repo: "o/r", Number: 10}})
	for _, want := range []string{digest[:12], "verified", "Milestone M1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the body lacks %q:\n%s", want, body)
		}
	}
}
