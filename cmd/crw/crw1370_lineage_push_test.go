package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1370: the hook input of a thread two or more levels below a relay child was never observed (the host's decision records hold only depth 1
// sub-agents; docs/port-cxc/known-defects/CRW-1370.md). If the host stamps the immediate parent's thread id as session_id, the session has no
// binding, and the guard follows parent_thread_id through the rollouts named by transcript_path, at most four parents up, to the relay child.

// lineageRollouts writes a Codex home's rollouts for a chain of threads, each the sub-agent of the one before it: threads[0] is a root session (the
// relay child), threads[i] has parent threads[i-1]. It returns the path of the rollout of each thread.
func (f *subagentPushFixture) lineageRollouts(threads ...string) map[string]string {
	f.t.Helper()
	dir := filepath.Join(f.home, "sessions", "2026", "10", "14")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	paths := map[string]string{}
	for i, id := range threads {
		source := `"vscode"`
		if i > 0 {
			source = fmt.Sprintf(`{"subagent":{"thread_spawn":{"parent_thread_id":%q,"depth":%d}}}`, threads[i-1], i)
		}
		line := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"session_id":%q,"source":%s,"base_instructions":"%s"}}`+"\n", id, threads[0], source, strings.Repeat("x", 4000))
		path := filepath.Join(dir, "rollout-2026-10-14T00-00-00-"+id+".jsonl")
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			f.t.Fatal(err)
		}
		paths[id] = path
	}
	return paths
}

func lineageThread(n int) string { return fmt.Sprintf("01a13570-0000-7000-8000-%012d", 100+n) }

// answerFrom is the declared leg's stdout for a sub-agent payload that names its transcript.
func (f *subagentPushFixture) answerFrom(session, agent, transcript, cwd, command string) string {
	f.t.Helper()
	o := map[string]any{"hook_event_name": "PreToolUse", "session_id": session, "turn_id": "t", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": command}, "transcript_path": transcript}
	if agent != "" {
		o["agent_id"], o["agent_type"] = agent, "worker"
	}
	payload, err := json.Marshal(o)
	if err != nil {
		f.t.Fatal(err)
	}
	var out bytes.Buffer
	claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-child-push"}, stdout: &out}, bytes.NewReader(payload), componentHooks())
	if !claimed || code != 0 {
		f.t.Fatalf("claimed %v, exit %d", claimed, code)
	}
	return out.String()
}

// A depth-2 thread whose input carries the immediate parent's thread id is judged as the relay child it descends from: a push to dev warns as the
// child's own does, its own branch passes, a force, delete or mirror push onto dev or main stays the block.
func TestCRW1370DeepSubagentWithItsParentsThreadIdIsJudgedAsTheRelayChild(t *testing.T) {
	f := newSubagentPushFixture(t)
	s1, s2 := lineageThread(1), lineageThread(2)
	rollouts := f.lineageRollouts(crw1303Kid, s1, s2)
	push := "git push origin HEAD:dev"
	want := f.answer(crw1303Kid, "", f.kid, push)
	if w, warned := guardWarning(want); !warned || !strings.Contains(w, "통합은 부모·통합기 몫") {
		t.Fatalf("the child's own push to dev: %q", want)
	}
	if got := f.answerFrom(s1, s2, rollouts[s2], f.kid, push); got != want {
		t.Errorf("a depth-2 thread's push to dev is not answered as the child's own:\ndepth 2: %s\nchild:   %s", got, want)
	}
	if out := f.answerFrom(s1, s2, rollouts[s2], f.kid, "git push origin HEAD:r2-x"); out != "" {
		t.Errorf("a depth-2 thread's push of the child's own branch: %q", out)
	}
	for _, command := range []string{"git push -f origin HEAD:dev", "git push --delete origin main", "git push --mirror origin"} {
		out := f.answerFrom(s1, s2, rollouts[s2], f.kid, command)
		if !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "통합은 부모·통합기 몫") {
			t.Errorf("a depth-2 thread: %s: %s", command, out)
		}
	}
	for _, r := range f.records() {
		if r["agentId"] == s2 && (r["leg"] != childPushLeg || r["sessionId"] != s1) {
			t.Errorf("record %v", r)
		}
	}
	var blocks int
	for _, r := range f.records() {
		if r["decision"] == "block" && r["rule"] == "shared-branch-history" && r["agentId"] == s2 {
			blocks++
		}
	}
	if blocks != 3 {
		t.Errorf("%d block records of the depth-2 thread, want 3: %v", blocks, f.records())
	}
}

// The binding of session_id comes first: a payload that carries the relay child's own session_id (the shape of every depth-1 input observed) is
// judged without a rollout being read, so an unreadable transcript_path changes nothing for it.
func TestCRW1370ABoundSessionIdNeverReadsTheLineage(t *testing.T) {
	f := newSubagentPushFixture(t)
	missing := filepath.Join(f.home, "sessions", "no", "such", "rollout.jsonl")
	got := f.answerFrom(crw1303Kid, lineageThread(2), missing, f.kid, "git push origin HEAD:dev")
	if w, warned := guardWarning(got); !warned || !strings.Contains(w, "통합은 부모·통합기 몫") || strings.Contains(got, "cannot be read") {
		t.Errorf("a bound session_id: %q", got)
	}
}

// A lineage that ends at a root with no relay child above it is a parent's or an integrator's sub-agent: not judged, nothing recorded. A payload
// that is not a sub-agent's, and one with no transcript_path, never reads a rollout.
func TestCRW1370ALineageWithNoRelayChildIsNotJudgedAndLeavesNoRecord(t *testing.T) {
	f := newSubagentPushFixture(t)
	s1, s2 := lineageThread(1), lineageThread(2)
	rollouts := f.lineageRollouts(crw1303Parent, s1, s2)
	before := len(f.records())
	for _, command := range []string{"git push origin HEAD:dev", "git push -f origin HEAD:dev"} {
		if out := f.answerFrom(s1, s2, rollouts[s2], f.parent, command); out != "" {
			t.Errorf("a sub-agent of a session that is no relay child: %s: %q", command, out)
		}
		if out := f.answerFrom(s1, "", rollouts[s2], f.parent, command); out != "" {
			t.Errorf("a payload that is no sub-agent's: %s: %q", command, out)
		}
		if out := f.answerFrom(s1, s2, "", f.parent, command); out != "" {
			t.Errorf("a payload with no transcript_path: %s: %q", command, out)
		}
	}
	if after := len(f.records()); after != before {
		t.Errorf("%d decision records were left for sessions that are no relay child", after-before)
	}
}

// The walk goes at most four parents up: a thread four parents below the relay child is judged as it, one five parents below is not found, and the push
// passes with the warning and a record (fail-open), a force push onto dev included.
func TestCRW1370TheLineageLimitIsFourParents(t *testing.T) {
	f := newSubagentPushFixture(t)
	chain := []string{crw1303Kid}
	for i := 1; i <= 5; i++ {
		chain = append(chain, lineageThread(i))
	}
	rollouts := f.lineageRollouts(chain...)
	push := "git push origin HEAD:dev"
	want := f.answer(crw1303Kid, "", f.kid, push)
	// Four parents up: the thread's input carries its immediate parent's thread id.
	if got := f.answerFrom(chain[3], chain[4], rollouts[chain[4]], f.kid, push); got != want {
		t.Errorf("a thread four parents below the relay child is not judged as it:\n%s\n%s", got, want)
	}
	// Five parents up.
	for _, command := range []string{push, "git push -f origin HEAD:dev"} {
		out := f.answerFrom(chain[4], chain[5], rollouts[chain[5]], f.kid, command)
		w, warned := guardWarning(out)
		if !warned || !strings.Contains(w, "deeper than 4 parents") || strings.Contains(out, "permissionDecision") {
			t.Errorf("a thread five parents below the relay child: %s: %q", command, out)
		}
	}
}

// Anything else the lineage walk cannot read is no binding, never a block: the push passes with the [crw warning] text and a warn record that names the
// sub-agent and never the command text, a force push onto dev included (the session is not known to be a child). The record is left once per session
// (CRW-1387).
func TestCRW1370AnUnreadableLineageFailsOpenWithARecord(t *testing.T) {
	s1, s2, s3 := lineageThread(1), lineageThread(2), lineageThread(3)
	for _, c := range []struct {
		name   string
		remove string // the thread whose rollout is gone
		from   string // the thread whose rollout is the payload's transcript_path
		reason string
	}{
		{"a transcript that does not exist", s3, s3, "cannot be read"},
		{"a parent thread with no rollout", s2, s3, "cannot be found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSubagentPushFixture(t)
			rollouts := f.lineageRollouts(crw1303Kid, s1, s2, s3)
			if err := os.Remove(rollouts[c.remove]); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"git push origin HEAD:dev", "git push -f origin HEAD:dev"} {
				out := f.answerFrom(s2, s3, rollouts[c.from], f.kid, command)
				w, warned := guardWarning(out)
				if !warned || !strings.Contains(w, c.reason) || strings.Contains(out, "permissionDecision") {
					t.Errorf("%s: %q", command, out)
				}
			}
			// CRW-1387: the unreadable lineage of a session is recorded once, at its first push, not on each one.
			rs := f.records()
			if len(rs) != 1 {
				t.Fatalf("%d decision records, want one for the session's two unjudged pushes (1): %v", len(rs), rs)
			}
			for _, r := range rs {
				if r["leg"] != childPushLeg || r["decision"] != "warn" || r["agentId"] != s3 || r["sessionId"] != s2 || r["rule"] == "shared-branch-history" {
					t.Errorf("record %v", r)
				}
			}
		})
	}
}
