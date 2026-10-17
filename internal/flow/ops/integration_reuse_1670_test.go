package ops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func TestReuseIntegrated1670LandFixProof(t *testing.T) {
	for _, scenario := range []string{
		"proven",
		"no commit",
		"commit only in another run",
		"commit before release",
		"commit after acceptance",
		"commit of another node",
		"commit superseded",
		"wrong fixed tree",
		"missing fixed tree",
		"later commit has another head",
		"release matches but later commit has another head",
		"no verification",
		"verification before commit",
		"verification after acceptance",
		"verification of another head",
		"verification did not run",
		"verification failed",
		"verification superseded",
		"later verification failed",
		"fixed head not descended from release",
		"fixed head not descended from base",
		"wrong release tree",
	} {
		t.Run(scenario, func(t *testing.T) {
			w, _, source := integratedDesignWorld(t)
			source = landSource(source)
			_, rel := rowOf(source, OpRelease)
			rel.Data["head"], rel.Data["tree"] = "released", "release-tree"
			fix := engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: OpCommit, CallID: "fix",
				Data: map[string]any{"node": "A", "commit": "head", "parent": "released", "tree": "tree"}}
			verified := engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: OpVerify, CallID: "fix-verify",
				Data: map[string]any{"node": "A", "head": "head", "ran": true, "ok": true}}
			at, _ := rowOf(source, OpAccept)
			source = slices.Insert(source, at, fix, verified)
			remove := func(op string) {
				i, _ := rowOf(source, op)
				source = slices.Delete(source, i, i+1)
			}
			moveBefore := func(op, before string) {
				_, e := rowOf(source, op)
				remove(op)
				i, _ := rowOf(source, before)
				source = slices.Insert(source, i, e)
			}
			moveAfterAccept := func(op string) {
				_, e := rowOf(source, op)
				remove(op)
				i, _ := rowOf(source, OpAccept)
				source = slices.Insert(source, i+1, e)
			}
			var extra []DesignRun
			switch scenario {
			case "no commit", "commit only in another run":
				remove(OpCommit)
				if scenario == "commit only in another run" {
					extra = []DesignRun{{Run: "another", Events: []engine.Event{fix, verified}}}
				}
			case "commit before release":
				moveBefore(OpCommit, OpRelease)
			case "commit after acceptance":
				moveAfterAccept(OpCommit)
			case "commit of another node":
				fix.Data["node"] = "other"
			case "commit superseded":
				source = append(source, engine.Event{Kind: engine.KindCall, Event: engine.EvSuperseded, Op: OpCommit, CallID: fix.CallID})
			case "wrong fixed tree":
				fix.Data["tree"] = "wrong"
			case "missing fixed tree":
				delete(fix.Data, "tree")
			case "later commit has another head", "release matches but later commit has another head":
				i, _ := rowOf(source, OpAccept)
				later := engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: OpCommit, CallID: "later-fix",
					Data: map[string]any{"node": "A", "commit": "other", "parent": "head", "tree": "tree"}}
				source = slices.Insert(source, i, later)
				if scenario == "release matches but later commit has another head" {
					rel.Data["head"], rel.Data["tree"] = "head", "tree"
				}
			case "no verification":
				remove(OpVerify)
			case "verification before commit":
				moveBefore(OpVerify, OpCommit)
			case "verification after acceptance":
				moveAfterAccept(OpVerify)
			case "verification of another head":
				verified.Data["head"] = "other"
			case "verification did not run":
				verified.Data["ran"] = false
			case "verification failed":
				verified.Data["ok"] = false
			case "verification superseded":
				source = append(source, engine.Event{Kind: engine.KindCall, Event: engine.EvSuperseded, Op: OpVerify, CallID: verified.CallID})
			case "later verification failed":
				i, _ := rowOf(source, OpAccept)
				later := engine.Event{Kind: engine.KindCall, Event: engine.EvCompleted, Op: OpVerify, CallID: "later-verify",
					Data: map[string]any{"node": "A", "head": "head", "ran": true, "ok": false}}
				source = slices.Insert(source, i, later)
			case "wrong release tree":
				rel.Data["tree"] = "wrong"
			}
			for i := range source {
				source[i].Seq, source[i].RunID = i+1, "source"
			}
			git := landGit(t, false, false)
			w.lane.d.Git = func(ctx context.Context, dir string, env []string, argv ...string) (string, error) {
				if argv[0] == "rev-parse" && argv[len(argv)-1] == "released^{tree}" {
					return "release-tree", nil
				}
				if argv[0] == "merge-base" {
					pair := argv[len(argv)-2] + " " + argv[len(argv)-1]
					if (scenario == "fixed head not descended from release" && pair == "released head") ||
						(scenario == "fixed head not descended from base" && pair == "base head") {
						return "", &GitError{Dir: dir, Args: argv, Err: exit1(t)}
					}
				}
				return git(ctx, dir, env, argv...)
			}
			w.lane.d.DesignHistory = func(context.Context, string) ([]DesignRun, error) {
				return append([]DesignRun{{Run: "source", Events: source}}, extra...), nil
			}
			w.must(NameDesignReuse, "M")
			w.ledgered(true, designOf("A"))
			raw, err := w.op(NameReuseIntegrated, "M", "A")
			if scenario != "proven" {
				var refused *Refusal
				if !errors.As(err, &refused) || refused.Reason != ReasonIntegrationUnproven {
					t.Fatalf("want integration_reuse_unproven, got %s / %v", raw, err)
				}
				return
			}
			var out ReuseIntegratedResult
			if err != nil || json.Unmarshal(raw, &out) != nil || !out.Reused || out.Head != "head" ||
				out.SourceRun != "source" || out.SourceKind != "land" || out.SourceReleaseCall != "release" ||
				out.SourceAcceptCall != "accept" || out.SourceIntegrateCall != "integrate" || out.ReviewBase != "base" {
				t.Fatalf("land-fix proof lost the source or the fixed head: %s / %v", raw, err)
			}
		})
	}
}
