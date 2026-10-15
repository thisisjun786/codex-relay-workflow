package engine

import (
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// CRW-1488 (1209 trial finding 12): the go the verifier runs (verifier.go_root) reaches every agent call the run starts. A session
// call's subagent shell is the owner's, which the run cannot set, so its packet names the go and the exact prefix that runs it; an
// App Server call's thread gets the entries in its shell (ThreadSpec.Tools).

func testRepositoryTools() *RepositoryTools {
	return &RepositoryTools{
		Env:    []string{"PATH=/opt/go-1.27/bin:/usr/bin", "GOROOT=/opt/go-1.27", "GOTOOLCHAIN=local"},
		Prefix: `PATH=/opt/go-1.27/bin:"$PATH" GOROOT=/opt/go-1.27 GOTOOLCHAIN=local`,
		Facts:  []string{"go: /opt/go-1.27/bin/go (verifier.go_root /opt/go-1.27 in /cfg/config.json), the go crw.verify runs, with GOROOT /opt/go-1.27 and GOTOOLCHAIN=local"},
	}
}

// Every session call's packet, of every role, names the configured go and the prefix its commands take.
func TestSessionPacketNamesRepositoryTools(t *testing.T) {
	k := newSessionKit(t)
	k.h.cfg.Tools = testRepositoryTools()
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if st.Role == "executor" && !strings.Contains(st.Workspace, "/call/") {
			return orchAnswer{edit: "P:" + st.Op}
		}
		return orchAnswer{}
	}
	value[[]string](t, k.h.run(sessionRoles))
	spawned := k.orch.spawned()
	if len(spawned) != 5 {
		t.Fatalf("%d spawns, want 5", len(spawned))
	}
	for _, st := range spawned {
		raw, err := os.ReadFile(st.Packet)
		if err != nil {
			t.Fatalf("%s: packet: %v", st.Op, err)
		}
		packet := string(raw)
		for _, want := range []string{"## Repository tools", testRepositoryTools().Facts[0], "`" + testRepositoryTools().Prefix + "`"} {
			if !strings.Contains(packet, want) {
				t.Errorf("%s (%s): the packet does not name %q:\n%s", st.Op, st.Role, want, packet)
			}
		}
		if strings.Index(packet, "## Repository tools") > strings.Index(packet, "## Task") {
			t.Errorf("%s: the tools section comes after the task", st.Op)
		}
	}
}

// A run whose configuration names no tool writes the packet of before: no tools section.
func TestSessionPacketWithoutRepositoryTools(t *testing.T) {
	k := newSessionKit(t)
	value[[]string](t, k.h.run(`const a = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
return [a.status]`))
	for _, st := range k.orch.spawned() {
		raw, _ := os.ReadFile(st.Packet)
		if strings.Contains(string(raw), "Repository tools") {
			t.Errorf("%s: a run with no tools names some:\n%s", st.Op, raw)
		}
	}
}

// An App Server call's thread is started with the run's tools, for every role.
func TestThreadSpecCarriesRepositoryTools(t *testing.T) {
	h := newHarness(t)
	h.cfg.Tools = testRepositoryTools()
	var mu sync.Mutex
	var got []ThreadSpec
	h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
		mu.Lock()
		got = append(got, spec)
		mu.Unlock()
		return ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}, nil
	}
	value[[]string](t, h.run(`const a = await agent.call('design', { agentType: 'architect', label: 'arch' })
const e = await agent.call('look', { agentType: 'explorer', label: 'exp' })
return [a.status, e.status]`))
	if len(got) != 2 {
		t.Fatalf("%d threads started, want 2", len(got))
	}
	for _, spec := range got {
		if spec.Tools == nil || !slices.Equal(spec.Tools.Env, testRepositoryTools().Env) || !slices.Equal(spec.Tools.Facts, testRepositoryTools().Facts) {
			t.Errorf("%s (%s): the thread is started with tools %+v", spec.CallID, spec.Role, spec.Tools)
		}
	}
}

// An App Server call's turns name the tools too (CRW-1488 r1): a thread loaded again after a host restart reloads its configuration
// and may not have them, so every turn's text ends with the same section, after the task. The journaled input is the script's.
func TestAppServerTurnNamesRepositoryTools(t *testing.T) {
	h := newHarness(t)
	h.cfg.Tools = testRepositoryTools()
	var mu sync.Mutex
	var prompts []string
	h.fake.Handler = func(r FakeRequest) FakeReply {
		mu.Lock()
		prompts = append(prompts, r.Prompt)
		mu.Unlock()
		return FakeReply{}
	}
	value[[]string](t, h.run(`const a = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
return [a.status]`))
	if len(prompts) != 1 {
		t.Fatalf("%d turns, want 1", len(prompts))
	}
	for _, want := range []string{"look around", "## Repository tools", testRepositoryTools().Facts[0], "`" + testRepositoryTools().Prefix + "`"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("the turn does not name %q:\n%s", want, prompts[0])
		}
	}
	if strings.Index(prompts[0], "## Repository tools") < strings.Index(prompts[0], "look around") {
		t.Errorf("the tools section comes before the task:\n%s", prompts[0])
	}
	for _, row := range h.callRows("exp") {
		if row.Event == EvTurnRequested && rowStr(row.Data, "input") != "look around" {
			t.Errorf("the journaled input is %q, want the script's", rowStr(row.Data, "input"))
		}
	}
}

// toolsBackend is a fake backend that takes the run's tools for the threads it loads again (agent.Backend.SetRepositoryTools).
type toolsBackend struct {
	*Fake
	got []*RepositoryTools
}

func (b *toolsBackend) SetRepositoryTools(t *RepositoryTools) { b.got = append(b.got, t) }

// A run gives its backend the tools before any call, so a thread the backend resumes (a held call after a host restart, a reconcile)
// is loaded with them.
func TestRunGivesTheBackendItsTools(t *testing.T) {
	h := newHarness(t)
	b := &toolsBackend{Fake: h.fake}
	h.cfg.Backend = b
	h.cfg.Tools = testRepositoryTools()
	value[[]string](t, h.run(`const a = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
return [a.status]`))
	if len(b.got) != 1 || b.got[0] != h.cfg.Tools {
		t.Errorf("the backend was given %v, want the run's tools once", b.got)
	}
}
