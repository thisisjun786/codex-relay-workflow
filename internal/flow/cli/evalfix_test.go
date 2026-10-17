package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// The tests of the correction round after the pre-merge evaluation of CRW-1277 (c4e8c198): d1 (integrate binds the acceptance and the
// head), d2 (a required command the shipped verifier cannot run), d3 (the host memory bound), d4 (the compiler's milestone rules at
// start), d5 (a linked skills tree).

// buildLane builds the lane of a run over the lab's store as BuildLane does (the lab's plan and handoff, the shipped verifier unless opt
// names one) and answers the SchedRelay the lane holds.
func (l *laneLab) buildLane(run string, raw []byte, opt LaneOptions) *SchedRelay {
	l.t.Helper()
	return l.buildLaneWith(run, raw, nil, opt)
}

// buildLaneWith is buildLane with more members of the run's arguments (the child host settings a start pinned, CRW-1285).
func (l *laneLab) buildLaneWith(run string, raw []byte, extra map[string]any, opt LaneOptions) *SchedRelay {
	l.t.Helper()
	ref, err := PinHandoff(l.w.state, run, raw)
	if err != nil {
		l.t.Fatal(err)
	}
	j, err := journal.Open(context.Background(), l.w.st, run, journal.Identity{PlanID: l.plan, PlanRevision: 1, ActorTaskID: labOwner, EpochNonce: run}, journal.Options{Immediate: true})
	if err != nil {
		l.t.Fatal(err)
	}
	script := filepath.Join(l.t.TempDir(), "laneful.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'laneful' }\nawait crw.commit({ node: 'n1' })\n"), 0o600); err != nil {
		l.t.Fatal(err)
	}
	members := map[string]any{"plan": l.plan, "milestone": "M1", "handoff": ref}
	for k, v := range extra {
		members[k] = v
	}
	args, _ := json.Marshal(members)
	req := LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: run, Workflow: "x", Script: script, Args: args, Thread: labOwner, CreatedAt: "2026-10-12T09:00:00Z"}, State: l.w.state},
		Store: l.w.st, Journal: j, Identity: journal.Identity{PlanID: l.plan, PlanRevision: 1, ActorTaskID: labOwner, EpochNonce: run}, Repo: l.repo,
		Policy: unitCheckPolicy()}
	var got *SchedRelay
	opt.Relay = func(r *SchedRelay) ops.Relay { got = r; return &labRelay{SchedRelay: r, l: l} }
	if opt.SkillsDigest == "" {
		opt.SkillsDigest = "sha256:lab-skills"
	}
	if _, err := BuildLane(context.Background(), req, opt); err != nil {
		l.t.Fatal(err)
	}
	return got
}

// TestIntegrateBindsTheRequestedAcceptanceAndHead (d1): an integration names the acceptance and the head it integrates; the batch merges
// the node's current acceptance only, so a call that names another acceptance, or the current one with another head, is refused before
// anything is merged and the branch does not move.
func TestIntegrateBindsTheRequestedAcceptanceAndHead(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	tried := false
	l.after = func(r *SchedRelay, effect string) {
		if effect != "accept:"+node || tried {
			return
		}
		tried = true
		var id, head string
		if err := r.Sched.Store.DB.QueryRow("SELECT acceptance_id, head FROM dag_agent_acceptances WHERE node_id = ?", node).Scan(&id, &head); err != nil {
			t.Errorf("the acceptance: %v", err)
			return
		}
		base := l.git(l.repo, "rev-parse", "integration")
		for name, req := range map[string]ops.IntegrateRequest{
			"another acceptance": {AcceptanceID: "agent-acceptance-obsolete", Head: head},
			"another head":       {AcceptanceID: id, Head: base},
			"no head":            {AcceptanceID: id},
		} {
			req.Plan, req.Node, req.Actor, req.RunID = l.plan, node, labOwner, r.RunID
			res, err := r.Integrate(ctx, req)
			if err == nil && res.Landed {
				t.Errorf("%s: the integration landed: %+v", name, res)
			}
			if err == nil && res.Reason == "" {
				t.Errorf("%s: the refusal names no reason: %+v", name, res)
			}
			if tip := l.git(l.repo, "rev-parse", "integration"); tip != base {
				t.Errorf("%s: the branch moved from %s to %s", name, base, tip)
			}
			var n int
			if err := r.Sched.Store.DB.QueryRow("SELECT COUNT(*) FROM dag_integration_batches").Scan(&n); err != nil || n != 0 {
				t.Errorf("%s: %d integration batches written (%v)", name, n, err)
			}
		}
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	if !tried {
		t.Fatal("the acceptance was never reached")
	}
	l.effectsOnce("A-1")
}

// TestBuildLaneAppliesTheHostMemoryBound (d3): the run's releases carry the host memory bound dag-release applies, read from the
// environment and sampled again for every release of the long-lived run: a host short of memory holds a node back (defer:host_memory)
// and writes no execution, and the same node is released once the host has memory again.
func TestBuildLaneAppliesTheHostMemoryBound(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	proc := t.TempDir()
	meminfo := func(gib int64) {
		text := fmt.Sprintf("MemTotal: 64395904 kB\nMemAvailable: %d kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n", gib*(1<<20))
		if err := os.WriteFile(filepath.Join(proc, "meminfo"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"CRW_DAG_HOST_PROC_ROOT": proc, "CRW_DAG_HOST_MAX_PRESSURE_SOME_AVG60": "100", "CRW_DAG_HOST_MAX_PRESSURE_FULL_AVG10": "100"}
	r := l.buildLane("run-hostmem0001", l.handoffBytes(nil), LaneOptions{Getenv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	node := l.nodeOf["A-1"]
	meminfo(1)
	held, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-mem"})
	if err != nil || held.Released || held.Reason != "defer:host_memory" {
		t.Fatalf("a host short of memory: %+v %v", held, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions"); n != 0 {
		t.Errorf("%d executions written while the host was short of memory", n)
	}
	meminfo(64)
	freed, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-mem"})
	if err != nil || !freed.Released {
		t.Fatalf("the host has memory again: %+v %v", freed, err)
	}
	env["CRW_DAG_HOST_MIN_AVAILABLE_GIB"] = "lots"
	if _, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-mem2"}); err == nil || !strings.Contains(err.Error(), "CRW_DAG_HOST_MIN_AVAILABLE_GIB") {
		t.Errorf("a threshold that is not a number: %v", err)
	}
}

// TestMilestoneRefusesWhatTheCompilerRefuses (d4): the run applies the compiler's rules to the entry of the milestone it reads, so a
// handoff edited after it was compiled (a surface the compiler does not know, a grade outside C1 to C5, a duplicate surface, a blank
// criterion or command) is refused at the start of the run and by the relay, and never reaches the final gate.
func TestMilestoneRefusesWhatTheCompilerRefuses(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	cases := map[string]func(*compile.Milestone){
		"unknown surface":   func(m *compile.Milestone) { m.Surfaces = []string{"webb"} },
		"duplicate surface": func(m *compile.Milestone) { m.Surfaces = []string{"web", "web"} },
		"grade C0":          func(m *compile.Milestone) { m.Grade = "C0" },
		"grade lower case":  func(m *compile.Milestone) { m.Grade = "c3" },
		"blank criterion":   func(m *compile.Milestone) { m.Criteria = []string{"ok", " "} },
		"blank command":     func(m *compile.Milestone) { m.RequiredCommands = []string{"crw-dev ci local", " "} },
		"no title":          func(m *compile.Milestone) { m.Title = " " },
	}
	i := 0
	for name, mutate := range cases {
		i++
		raw := l.handoffBytes(func(in *compile.Input) { mutate(&in.Projects[0].Milestones[0]) })
		if _, err := l.relayFor(fmt.Sprintf("run-compiled%02d", i), raw, 1).Milestone(ctx, l.plan, "M1"); err == nil {
			t.Errorf("%s: the relay reads the milestone", name)
		}
		in, err := DecodeHandoff(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if why := CheckHandoff(in, l.plan, "M1"); why == "" {
			t.Errorf("%s: the start accepts the handoff", name)
		}
	}
	good, err := DecodeHandoff(l.handoffBytes(nil))
	if err != nil {
		t.Fatal(err)
	}
	if why := CheckHandoff(good, l.plan, "M1"); why != "" {
		t.Errorf("the compiled handoff is refused: %s", why)
	}
}

// CRW-1363: unknown requirements remain readable; verification warns and records
// which configured command actually ran.
func TestMilestoneAcceptsUnknownRequiredCommands(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	withCommand := func(cmds ...string) []byte {
		return l.handoffBytes(func(in *compile.Input) { in.Projects[0].Milestones[0].RequiredCommands = cmds })
	}
	shipped := l.buildLane("run-verifier001", withCommand("make e2e"), LaneOptions{})
	if _, err := shipped.Milestone(ctx, l.plan, "M1"); err != nil {
		t.Errorf("a command the shipped verifier cannot run: %v", err)
	}
	for name, cmds := range map[string][]string{"none": nil, "ci local": {ops.CILocalCommand}} {
		ok := l.buildLane("run-verifier-"+strings.ReplaceAll(name, " ", ""), withCommand(cmds...), LaneOptions{})
		if _, err := ok.Milestone(ctx, l.plan, "M1"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	custom := l.buildLane("run-verifier002", withCommand("make e2e"), LaneOptions{Verifier: labVerifier{l}})
	if _, err := custom.Milestone(ctx, l.plan, "M1"); err != nil {
		t.Errorf("a lane whose verifier runs commands: %v", err)
	}
}

// TestPluginSkillsDigestFollowsLinks (d5): the skills digest hashes the content a link resolves to (a linked skills directory, a linked
// skill file or directory), so a change of linked instruction bytes changes the digest; a link that cannot be resolved is an unresolved
// digest, never the digest of an empty tree.
func TestPluginSkillsDigestFollowsLinks(t *testing.T) {
	const empty = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(root string) string {
		return PluginSkillsDigest(func(k string) (string, bool) { return root, k == "PLUGIN_ROOT" })
	}

	real := t.TempDir()
	write(filepath.Join(real, "skills", "a", "SKILL.md"), "one\n")
	base := digest(real)
	if !strings.HasPrefix(base, "sha256:") || base == empty {
		t.Fatalf("the real tree: %s", base)
	}

	// the skills directory itself is a link
	src := t.TempDir()
	write(filepath.Join(src, "a", "SKILL.md"), "one\n")
	linkedRoot := t.TempDir()
	if err := os.Symlink(src, filepath.Join(linkedRoot, "skills")); err != nil {
		t.Fatal(err)
	}
	if got := digest(linkedRoot); got != base {
		t.Errorf("a linked skills directory digests to %s, the same tree unlinked to %s", got, base)
	}
	write(filepath.Join(src, "a", "SKILL.md"), "two\n")
	if got := digest(linkedRoot); got == base || got == empty {
		t.Errorf("changed linked bytes digest to %s", got)
	}

	// a skill file and a skill directory that are links
	mixed := t.TempDir()
	body := t.TempDir()
	write(filepath.Join(body, "file.md"), "file\n")
	write(filepath.Join(body, "dir", "SKILL.md"), "dir\n")
	if err := os.MkdirAll(filepath.Join(mixed, "skills", "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(body, "file.md"), filepath.Join(mixed, "skills", "s", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(body, "dir"), filepath.Join(mixed, "skills", "t")); err != nil {
		t.Fatal(err)
	}
	before := digest(mixed)
	write(filepath.Join(body, "file.md"), "file changed\n")
	afterFile := digest(mixed)
	write(filepath.Join(body, "dir", "SKILL.md"), "dir changed\n")
	afterDir := digest(mixed)
	if !strings.HasPrefix(before, "sha256:") || before == empty || afterFile == before || afterDir == afterFile {
		t.Errorf("linked skill content does not move the digest: %s %s %s", before, afterFile, afterDir)
	}

	// a link to nothing, and a link back into its own tree
	broken := t.TempDir()
	write(filepath.Join(broken, "skills", "a", "SKILL.md"), "x\n")
	if err := os.Symlink(filepath.Join(broken, "nowhere"), filepath.Join(broken, "skills", "gone")); err != nil {
		t.Fatal(err)
	}
	if got := digest(broken); !strings.HasPrefix(got, "unresolved:") {
		t.Errorf("a broken link: %s", got)
	}
	loop := t.TempDir()
	write(filepath.Join(loop, "skills", "a", "SKILL.md"), "x\n")
	if err := os.Symlink(filepath.Join(loop, "skills"), filepath.Join(loop, "skills", "a", "back")); err != nil {
		t.Fatal(err)
	}
	if got := digest(loop); !strings.HasPrefix(got, "unresolved:") {
		t.Errorf("a link back into its tree: %s", got)
	}
}
