package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// Decode the wire result so these regressions also compile before source support.
type researchPlan struct {
	Routes []Route                                 `json:"routes"`
	Source struct{ Call, Head, Tree, Base string } `json:"source"`
}

func researchContext(call string) map[string]any {
	return map[string]any{"node": "A", "tier": "executor", "regions": []string{"A/"}, "source_call": call, "findings": []any{}}
}

func researchRoute(t *testing.T, u *ucWorld, ec map[string]any) researchPlan {
	t.Helper()
	var p researchPlan
	if err := json.Unmarshal(u.must("exploreRoute", ExploreResearch, ec, []Question{{Question: "Where is the new query?"}}), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResearchSourcePreservesPendingTree(t *testing.T) {
	u := newUCWorld(t)
	u.lane.d.Repo = u.repo
	write(t, u.wt, "a.txt", "staged\n", 0o644)
	gitIn(t, u.wt, "add", "a.txt")
	write(t, u.wt, "new.txt", "untracked\n", 0o644)
	u.build("source", "working\n")
	head, index := gitIn(t, u.wt, "rev-parse", "HEAD"), gitIn(t, u.wt, "write-tree")
	status := gitIn(t, u.wt, "status", "--porcelain")
	before := indexBytes(t, u.wt)
	p := researchRoute(t, u, researchContext("source"))
	if p.Source.Head == "" {
		t.Fatalf("no source snapshot: %+v", p)
	}
	if p.Source.Call != "source" || p.Source.Base != head || p.Source.Tree != u.snapshot() {
		t.Fatalf("wrong source: %+v", p.Source)
	}
	for file, want := range map[string]string{"a.txt": "working", "new.txt": "untracked"} {
		if got := gitIn(t, u.repo, "show", p.Source.Head+":"+file); got != want {
			t.Errorf("snapshot %s = %q", file, got)
		}
		b, err := os.ReadFile(filepath.Join(u.wt, file))
		if err != nil || string(b) != want+"\n" {
			t.Errorf("original %s changed: %q %v", file, b, err)
		}
	}
	if gitIn(t, u.repo, "rev-parse", p.Source.Head+"^") != head || gitIn(t, u.repo, "rev-parse", p.Source.Head+"^{tree}") != p.Source.Tree {
		t.Fatal("snapshot ancestry/tree mismatch")
	}
	if gitIn(t, u.wt, "rev-parse", "HEAD") != head || gitIn(t, u.wt, "write-tree") != index || !bytes.Equal(before, indexBytes(t, u.wt)) || gitIn(t, u.wt, "status", "--porcelain") != status {
		t.Fatal("original HEAD/index/status changed")
	}
	if refs := gitIn(t, u.repo, "for-each-ref", "--format=%(objectname)", "refs/crw/flow/run-1/research/"); refs != p.Source.Head {
		t.Fatalf("unretained snapshot: %s", refs)
	}
	if again := researchRoute(t, u, researchContext("source")); again.Source.Head != p.Source.Head {
		t.Fatal("same source moved")
	}
}

func TestResearchSourceUsesTheQuestionSnapshot(t *testing.T) {
	u := newUCWorld(t)
	u.lane.d.Repo = u.repo
	u.build("source", "question time\n")
	write(t, u.wt, "a.txt", "later\n", 0o644)
	p := researchRoute(t, u, researchContext("source"))
	if p.Source.Head == "" {
		t.Fatal("no source snapshot")
	}
	if gitIn(t, u.repo, "show", p.Source.Head+":a.txt") != "question time" {
		t.Fatal("read moving worktree instead of recorded source")
	}
	b, err := os.ReadFile(filepath.Join(u.wt, "a.txt"))
	if err != nil || string(b) != "later\n" {
		t.Fatal("later edits changed")
	}
}

func TestResearchSourceRejectsInvalidCaller(t *testing.T) {
	for _, bad := range []string{"unknown", "wrong-node", "explorer", "qa", "missing-tree", "missing-base", "other-run"} {
		t.Run(bad, func(t *testing.T) {
			u := newUCWorld(t)
			u.lane.d.Repo = u.repo
			u.build("source", "dirty\n")
			events := u.j.Events()
			j := engine.NewMemoryJournal()
			for _, ev := range events {
				if ev.CallID == "source" {
					switch bad {
					case "unknown":
						ev.CallID = "not-source"
					case "wrong-node":
						ev.Data["node"] = "B"
					case "explorer":
						ev.Data["role"] = "explorer"
					case "qa":
						ev.Data["qa"] = true
					case "missing-tree":
						delete(ev.Data, "pending_tree")
					case "missing-base":
						delete(ev.Data, "allocation")
					case "other-run":
						ev.RunID = "other"
					}
				}
				j.Append(ev)
			}
			u.j = j
			decisions := &MemoryDecisions{}
			u.lane.d.Decisions = decisions
			_, err := u.op("exploreRoute", ExploreResearch, researchContext("source"), []Question{{Question: "where?"}})
			if err == nil || !strings.Contains(err.Error(), "research_source_unavailable") {
				t.Fatalf("invalid source accepted: %v", err)
			}
			if refs := gitIn(t, u.repo, "for-each-ref", "refs/crw/flow/run-1/research/"); refs != "" {
				t.Fatal("invalid source retained")
			}
			if len(decisions.Spawn) != 0 {
				t.Fatal("invalid source recorded a spawn")
			}
		})
	}
}

func TestResearchSourceReuseIsContextBound(t *testing.T) {
	for _, change := range []string{"same", "tree", "regions", "findings"} {
		t.Run(change, func(t *testing.T) {
			u := newUCWorld(t)
			u.lane.d.Repo = u.repo
			u.build("first", "dirty\n")
			ec := researchContext("first")
			p := researchRoute(t, u, ec)
			u.must("exploreRecord", ExploreResearch, ec, []Answer{{Digest: p.Routes[0].Digest, Question: p.Routes[0].Question, Route: RouteSpawn, Answer: "old answer"}})
			body := "dirty\n"
			if change == "tree" {
				body = "different\n"
			}
			u.build("second", body)
			ec = researchContext("second")
			if change == "regions" {
				ec["regions"] = []string{"B/"}
			}
			if change == "findings" {
				ec["findings"] = []any{map[string]any{"id": "f1", "text": "another finding"}}
			}
			next := researchRoute(t, u, ec)
			if change == "same" {
				if next.Routes[0].Route != RouteReuse || next.Routes[0].Answer != "old answer" {
					t.Fatalf("same context not reused: %+v", next)
				}
			} else if next.Routes[0].Route != RouteSpawn || next.Routes[0].Digest == p.Routes[0].Digest {
				t.Fatalf("stale answer reused after %s: %+v", change, next)
			}
		})
	}
}

func TestResearchSourceProofChecksSnapshot(t *testing.T) {
	u := newUCWorld(t)
	u.lane.d.Repo = u.repo
	u.build("source", "dirty\n")
	researchRoute(t, u, researchContext("source"))
	row, _ := rows(u.j.Events()).opRows(OpExploreRoute).last(func(engine.Event) bool { return true })
	op := u.lane.Ops()[NameExploreRoute]
	if ok, why, err := op.Proven(context.Background(), u.j.Events(), row); !ok || err != nil {
		t.Fatalf("valid snapshot: %v %s %v", ok, why, err)
	}
	row.Data["source_head"] = strings.Repeat("f", 40)
	if ok, why, err := op.Proven(context.Background(), u.j.Events(), row); ok || why != "research_source_missing" || err != nil {
		t.Fatalf("missing snapshot proved: %v %s %v", ok, why, err)
	}
}
