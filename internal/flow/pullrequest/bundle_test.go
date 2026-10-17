package pullrequest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
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

func TestBundleBodyIncludesMilestoneHandoff(t *testing.T) {
	body := Body(bundleBody(ops.BundlePROpenRequest{
		Milestone: ops.Milestone{ID: "M1", Title: "Reject stale receipts", Criteria: []string{"A receipt for another head is refused."}},
		Close:     ops.CloseResultOutcome{Tip: bodyTip},
	}), nil)
	for _, want := range []string{"Goal: Reject stale receipts", "- c1: A receipt for another head is refused.", "Next action:"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestOpenBundleChecksHandoffBeforePublication(t *testing.T) {
	credential := "INTEGRATION_TOKEN=" + "fixture-value"
	for _, field := range []string{"title", "criteria", "safe"} {
		t.Run(field, func(t *testing.T) {
			g := newGitWorld(t)
			fake := trackingfake.New(t, "o/r")
			fake.HeadSHA = g.remoteSHA
			d := deps(t, fake, g)
			d.PublicationPolicy = func(string) (publishpolicy.Policy, error) { return publishpolicy.Policy{}, nil }
			in := ops.BundlePROpenRequest{
				Target: ops.BundlePRTarget{Repo: "o/r", Checkout: g.checkout, HeadRef: "crw/m1", BaseRef: "dev",
					ReviewRecord: filepath.Join(t.TempDir(), "review.json")},
				Milestone: ops.Milestone{ID: "M1", Title: "Reject stale receipts", Criteria: []string{"A stale receipt is refused."},
					Nodes: []ops.PlanNode{{Issue: "o:r:10"}}},
				Close: ops.CloseResultOutcome{Tip: g.git(g.checkout, "rev-parse", "crw/m1")},
			}
			switch field {
			case "title":
				in.Milestone.Title = credential
			case "criteria":
				in.Milestone.Criteria = []string{credential}
			}
			result, err := OpenBundle(context.Background(), d, in)
			if field == "safe" {
				if err != nil || result.State != "opened" || g.remoteSHA("crw/m1") != in.Close.Tip {
					t.Fatalf("safe handoff: result %+v, error %v", result, err)
				}
				pulls := fake.Pulls()
				if len(pulls) != 1 || !strings.Contains(pulls[1].Body, "Goal: Reject stale receipts") ||
					!strings.Contains(pulls[1].Body, "- c1: A stale receipt is refused.") {
					t.Fatalf("safe handoff was not published: %+v", pulls)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "publication scrub blocked") {
				t.Errorf("credential was not blocked: result %+v, error %v", result, err)
			} else if strings.Contains(err.Error(), credential) {
				t.Error("refusal echoed the credential")
			}
			if got := g.remoteSHA("crw/m1"); got != "" {
				t.Errorf("blocked handoff pushed %s", got)
			}
			if len(fake.Pulls()) != 0 {
				t.Error("blocked handoff opened a pull request")
			}
			for _, request := range fake.Requests() {
				if strings.HasPrefix(request, "POST ") || strings.HasPrefix(request, "PATCH ") {
					t.Errorf("blocked handoff sent a write: %s", request)
				}
			}
		})
	}
}
