package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// landSource turns the integrated source of integratedDesignWorld into a land-head source (CRW-1649): the release is a land
// release naming head/tree/base and there is no DESIGN ledger or audit to prove it with - the run's land is proven by the
// relay's current state and the release's recorded base/tree instead.
func landSource(source []engine.Event) []engine.Event {
	var out []engine.Event
	for _, ev := range source {
		if ev.Op == OpDesignLedger || ev.Op == OpDesignAudit {
			continue
		}
		out = append(out, ev)
	}
	_, rel := rowOf(out, OpRelease)
	rel.Data["kind"] = "land"
	rel.Data["head"] = "head"
	rel.Data["tree"] = "tree"
	rel.Data["base"] = "base"
	return out
}

// landGit answers rev-parse ^{tree} with the recorded tree and every ancestry check with success, except the calls the
// flags name: lostBatch fails the batch ancestry check, baseNotAncestor fails the recorded base's ancestry of the head.
func landGit(t *testing.T, lostBatch, baseNotAncestor bool) func(context.Context, string, []string, ...string) (string, error) {
	return func(_ context.Context, _ string, _ []string, argv ...string) (string, error) {
		if len(argv) == 0 {
			return "", errors.New("empty git call")
		}
		switch argv[0] {
		case "rev-parse":
			if strings.HasSuffix(argv[len(argv)-1], "^{tree}") {
				return "tree", nil
			}
			return "", nil
		case "merge-base":
			pair := argv[len(argv)-2] + " " + argv[len(argv)-1]
			if (lostBatch && strings.Contains(pair, "batch")) || (baseNotAncestor && pair == "base head") {
				return "", &GitError{Dir: ".", Args: argv, Err: exit1(t)}
			}
			return "", nil
		}
		return "", errors.New("unexpected git call: " + strings.Join(argv, " "))
	}
}

func exit1(t *testing.T) error {
	t.Helper()
	if err := exec.Command("false").Run(); err != nil {
		return err
	}
	t.Fatal("false exited 0")
	return nil
}

// TestReuseIntegrated1649LandSource (CRW-1649): a node landed by a land-head run has no source DESIGN; the reuse proof reads
// the release's recorded head/tree/base instead of a ledger. A valid land reuses with the release base as review base and
// never invents an integrated-design pin.
func TestReuseIntegrated1649LandSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*world, *integratedDesignRelay, []engine.Event)
		proven bool
	}{
		{"proven", func(*world, *integratedDesignRelay, []engine.Event) {}, true},
		{"wrong release head", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			e.Data["head"] = "other"
		}, false},
		{"missing release head", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			delete(e.Data, "head")
		}, false},
		{"wrong tree", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			e.Data["tree"] = "other"
		}, false},
		{"missing tree", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			delete(e.Data, "tree")
		}, false},
		{"missing base", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			delete(e.Data, "base")
		}, false},
		{"changed criteria", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			e.Data["criteria_digest"] = "other"
		}, false},
		{"missing reviewer call", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpAccept)
			delete(e.Data, "reviewer_call")
		}, false},
		{"missing integration", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpIntegrate)
			delete(e.Data, "batch_commit")
		}, false},
		{"batch lost from target", func(w *world, _ *integratedDesignRelay, _ []engine.Event) {
			w.lane.d.Git = landGit(t, true, false)
		}, false},
		{"recorded base not ancestor of head", func(w *world, _ *integratedDesignRelay, _ []engine.Event) {
			w.lane.d.Git = landGit(t, false, true)
		}, false},
		{"missing audit subject", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			// the reviewer_call of the acceptance names a call the journal has no completed evidence of: change the
			// basis so the subject needs that call's verified review row, which is absent - release/accept/integrate
			// checks still pass and only the audit subject fails
			_, acc := rowOf(s, OpAccept)
			acc.Data["basis"] = "review"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, r, source := integratedDesignWorld(t)
			source = landSource(source)
			w.lane.d.DesignHistory = func(context.Context, string) ([]DesignRun, error) {
				return []DesignRun{{Run: "source", Events: source}}, nil
			}
			w.lane.d.Git = landGit(t, false, false)
			tc.mutate(w, r, source)
			w.must(NameDesignReuse, "M")
			// a land source proves the landing but has no DESIGN to pin: no integrated_design entry is fabricated
			fixed, err := integratedDesigns(rows(w.j.Events()), "M")
			if err != nil {
				t.Fatalf("integratedDesigns: %v", err)
			}
			if tc.proven && len(fixed) != 0 {
				t.Fatalf("%s: a land source invented an integrated design pin: %+v", tc.name, fixed)
			}
			w.ledgered(true, designOf("A"))
			out, err := w.op(NameReuseIntegrated, "M", "A")
			if !tc.proven {
				var refused *Refusal
				if !errors.As(err, &refused) || refused.Reason != ReasonIntegrationUnproven {
					t.Fatalf("%s: want integration_reuse_unproven, got %v", tc.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: proven land refused: %v", tc.name, err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			if m["reused"] != true || m["source_run"] != "source" || m["review_base"] != "base" || m["head"] != "head" {
				t.Fatalf("%s: reuse result %s", tc.name, out)
			}
			if m["source_kind"] != "land" {
				t.Fatalf("%s: source_kind %q in %s", tc.name, m["source_kind"], out)
			}
		})
	}
}
