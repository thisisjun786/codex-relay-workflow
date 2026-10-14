package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/affordance"
	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/provider"
	"github.com/thisisjun786/codex-relay-workflow/internal/recall"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/job"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
)

// componentHook is an ingress outside the PABCD stage table. Each component owns
// its stdin policy, observation name and answer; this dispatcher owns only routing.
type componentHook struct {
	ID, Event string
	Run       func(invocation, io.Reader) int
}

// componentHooks is the row-addition point for later provider-bridge and cxc-ops
// hooks. Their rows require no changes to dispatch or the bg-wake implementation.
func componentHooks() []componentHook {
	bg := func(event string) func(invocation, io.Reader) int {
		return func(c invocation, in io.Reader) int {
			cwd, err := os.Getwd()
			if err != nil {
				return 0
			}
			return job.RunHook(c.ctx, event, in, c.stdout, os.LookupEnv, cwd, time.Now)
		}
	}
	aff := func(event string) func(invocation, io.Reader) int {
		return func(c invocation, in io.Reader) int {
			cwd, _ := os.Getwd()
			return affordance.RunHook(c.ctx, event, in, c.stdout, os.LookupEnv, cwd)
		}
	}
	return []componentHook{
		{"session-start-injecting-recall-context", "session-start", func(c invocation, in io.Reader) int {
			// Node's process.cwd() is the physical directory, which a logical $PWD can hide.
			cwd, _ := recall.RecallPhysicalAbs(".")
			return recall.RunHook(c.ctx, "session-start", in, c.stdout, os.LookupEnv, cwd, func(home, path, cwd, source string, deps recall.RecallContextDeps) string {
				return recall.HandleSessionStart(recall.IndexStatusLine(home, path), cwd, source, recall.SessionStartOptions{Home: home, MemoryNotice: recall.MemoryPipelineNotice(home)}, deps)
			})
		}},
		{"post-compact-injecting-recall-context", "post-compact", func(c invocation, in io.Reader) int {
			cwd, _ := recall.RecallPhysicalAbs(".")
			return recall.RunHook(c.ctx, "post-compact", in, c.stdout, os.LookupEnv, cwd, nil)
		}},
		{"user-prompt-submit-detecting-recall-intent", "user-prompt-submit", func(c invocation, in io.Reader) int {
			cwd, _ := recall.RecallPhysicalAbs(".")
			return recall.RunHook(c.ctx, "user-prompt-submit", in, c.stdout, os.LookupEnv, cwd, nil)
		}},
		{"session-start-announcing-subagent-fallback", "session-start", func(c invocation, in io.Reader) int {
			return role.RunFallbackNoticeHook(c.ctx, in, c.stdout, os.LookupEnv, func(data []byte) string {
				raw, _ := harness.ReadStdin(bytes.NewReader(data))
				harness.RecordInvocation(raw, "subagent-config", "session-start", os.LookupEnv)
				return raw
			})
		}},
		// Spawn attach hook: the recursion and final-gate leg, with its own stdin policy and answer.
		{"pre-tool-use-attaching-skills", "pre-tool-use", func(c invocation, in io.Reader) int {
			// The leg's refusals warn (CRW-1307): its answer goes through the mode table before it is written. What it read names
			// the session of the record.
			var read, answer bytes.Buffer
			code := spawn.RunHook(c.ctx, io.TeeReader(io.LimitReader(in, harness.MaxStdinBytes+1), &read), &answer, os.LookupEnv)
			if code != 0 {
				return code
			}
			var out string
			if read.Len() > harness.MaxStdinBytes {
				// An input over the bound is unread input, released with a record as in every guard leg, not a refusal that warns.
				out = harness.ReleaseGuard(attachingSkillsLeg, "", "over 4194304 bytes", os.LookupEnv)
			} else {
				out = harness.SettleGuard(attachingSkillsLeg, read.String(), answer.String(), "", os.LookupEnv)
			}
			if out == "" {
				return 0
			}
			if _, err := io.WriteString(c.stdout, out); err != nil {
				return 2
			}
			return 0
		}},
		// GitHub post guard: CRW's own protection (no CXC oracle), with its own stdin policy and answer.
		{"pre-tool-use-guarding-github-post", "pre-tool-use", func(c invocation, in io.Reader) int {
			done := make(chan string, 1)
			go func() {
				defer githubPostGuardReaderDone()
				raw, over, ok := pabcdhook.GitHubPostInput(in)
				if !ok {
					done <- harness.ReleaseGuard(githubPostLeg, "", "a read error", os.LookupEnv)
					return
				}
				// Input that arrives once the leg has been cancelled finds a leg that already answered: nothing is recorded or judged for it.
				if c.ctx.Err() != nil {
					done <- ""
					return
				}
				// Like every other leg, the record is left before the judgment; it is metadata-only and an over-bound payload leaves none.
				if !over {
					harness.RecordInvocation(raw, harness.Component, "pre-tool-use-github-post", os.LookupEnv)
				}
				if over {
					done <- harness.ReleaseGuard(githubPostLeg, "", "over 1048576 bytes", os.LookupEnv)
					return
				}
				answer, rule := pabcdhook.GitHubPostDecision(raw, false)
				done <- harness.SettleGuard(githubPostLeg, raw, answer, rule, os.LookupEnv)
			}()
			select {
			case answer := <-done:
				if answer != "" {
					_, _ = io.WriteString(c.stdout, answer)
				}
				return 0
			case <-c.ctx.Done():
				// A cancelled guard lets the call through with a warning and a record (fail-open, CRW-1307): the command was not read.
				_, _ = io.WriteString(c.stdout, harness.ReleaseGuard(githubPostLeg, "", "the leg was cancelled", os.LookupEnv))
				return 0
			}
		}},
		// Relay child push guard (CRW-1284): a relay child pushes only its own branch; a session the relay does not know as a child is
		// not judged. An input that cannot be read whole, or a leg cancelled before it is, is refused, as the GitHub post guard refuses
		// it; input that arrives after the cancellation is neither recorded nor judged.
		{"pre-tool-use-guarding-child-push", "pre-tool-use", func(c invocation, in io.Reader) int {
			done := make(chan string, 1)
			go func() {
				raw, over, ok := pabcdhook.GitHubPostInput(in)
				switch {
				case c.ctx.Err() != nil:
					done <- ""
					return
				case !ok || over:
					done <- harness.ReleaseGuard(childPushLeg, "", "a read error or over 1048576 bytes", os.LookupEnv)
					return
				}
				harness.RecordInvocation(raw, harness.Component, "pre-tool-use-child-push", os.LookupEnv)
				once := "" // the unreadable lineage of a sub-agent is recorded once per session (CRW-1387); the relay's own record warns every time
				answer, rule := pabcdhook.ChildPushDecision(raw, func(session, cwd string) pabcdhook.ChildPushBinding {
					child, parent, err := childBindingOf(c.ctx, session)
					if err == nil && !child {
						if child, parent, err = subagentLineageBinding(c.ctx, raw); err != nil {
							// The marker is for a lineage that cannot be followed to its end; a relay record that could not be read at one hop
							// is a failure a later walk can pass, so it records as every relay-record failure does (CRW-1398).
							var walk *lineageWalkError
							if !errors.As(err, &walk) || !walk.relayRead {
								once = "unreadable-lineage"
							}
						}
					}
					switch {
					case err != nil:
						return pabcdhook.ChildPushBinding{Unreadable: err.Error()}
					case !child:
						return pabcdhook.ChildPushBinding{}
					}
					return pabcdhook.ChildPushBinding{Child: true, Own: pabcdhook.ChildPushOwnBranch(c.ctx, cwd, parent)}
				})
				done <- harness.SettleGuardOnce(childPushLeg, raw, answer, rule, once, os.LookupEnv)
			}()
			select {
			case answer := <-done:
				_, _ = io.WriteString(c.stdout, answer)
			case <-c.ctx.Done():
				_, _ = io.WriteString(c.stdout, harness.ReleaseGuard(childPushLeg, "", "the leg was cancelled", os.LookupEnv))
			}
			return 0
		}},
		// Provider-bridge component ingress; activation is owned by the cutover.
		{"session-start-ensuring-provider-bridge", "session-start", func(c invocation, in io.Reader) int {
			return provider.RunHook(c.ctx, in, c.stdout, os.LookupEnv)
		}},
		// Self-heal of the declared soft codex flags, report only: it diagnoses and warns, and
		// writes nothing (the J4 decision). Activation is owned by the cutover.
		{"session-start-healing-declared-features", "session-start", func(c invocation, in io.Reader) int {
			return configguard.RunSelfHealReportHook(c.ctx, in, c.stdout, os.LookupEnv)
		}},

		// The status line of the flow runs a thread owns (CRW-1205): printed at the end of that thread's turns only, for the runs that
		// are not over, and nothing for any other thread. It never blocks a turn.
		{"stop-showing-flow-status", "stop", func(c invocation, in io.Reader) int {
			return flowcli.RunStopHook(c.ctx, in, c.stdout, flowcli.Default(), "", "", 5*time.Second)
		}},
		{"stop-waking-on-background-completion", "stop", bg("stop")},
		{"user-prompt-submit-delivering-background-completions", "user-prompt-submit", bg("user-prompt-submit")},
		{"session-start-adopting-background-completions", "session-start", bg("session-start")},
		{"session-start-announcing-map-affordance", "session-start", aff("session-start")},
		{"post-compact-injecting-bg-terminal-affordance.post-compact", "post-compact", aff("post-compact")},
		{"post-compact-injecting-bg-terminal-affordance.user-prompt-submit", "user-prompt-submit", aff("user-prompt-submit")},
	}
}

// childBindingOf is the relay's record of a session for the child push guard; a test replaces it to make that record unreadable for a
// moment.
var childBindingOf = hook.ChildBinding

// subagentLineageBinding is the child push guard's fallback for a sub-agent whose session_id the relay does not know as a child
// (CRW-1370): when the input names an agent_id and a transcript_path, it follows the parent threads of the sub-agent's rollout, at most
// hook.LineageDepthLimit of them, to a thread the relay holds as a child. An input that is no sub-agent's, or names no transcript, binds nothing
// and reads nothing. A lineage that cannot be read is an error, which the guard answers with its warning and a decision record (fail-open).
func subagentLineageBinding(ctx context.Context, raw string) (child bool, parentCwd string, err error) {
	var input struct {
		AgentID        string `json:"agent_id"`
		TranscriptPath string `json:"transcript_path"`
	}
	if json.Unmarshal([]byte(raw), &input) != nil || input.AgentID == "" || input.TranscriptPath == "" {
		return false, "", nil
	}
	relayRead := false
	child, parentCwd, unreadable := hook.ChildBindingByLineage(ctx, input.TranscriptPath, func(ctx context.Context, session string) (bool, string, error) {
		isChild, cwd, err := childBindingOf(ctx, session)
		relayRead = relayRead || err != nil
		return isChild, cwd, err
	})
	if unreadable != "" {
		return false, "", &lineageWalkError{msg: unreadable, relayRead: relayRead}
	}
	return child, parentCwd, nil
}

// lineageWalkError is why the lineage walk ended without an answer. relayRead is true when it ended at a hop whose relay record could not be
// read (the walk stops there), not because the lineage itself cannot be followed.
type lineageWalkError struct {
	msg       string
	relayRead bool
}

func (e *lineageWalkError) Error() string { return e.msg }

// The component rows that are PreToolUse guards, by their registration names (the mode table's keys).
const (
	githubPostLeg      = "pre-tool-use-guarding-github-post"
	childPushLeg       = "pre-tool-use-guarding-child-push"
	attachingSkillsLeg = "pre-tool-use-attaching-skills"
)

// githubPostGuardReaderDone runs when the GitHub post guard's input goroutine has finished, recorded and judged or not; a test
// waits on it instead of on a sleep.
var githubPostGuardReaderDone = func() {}

func runComponentHook(c invocation, in io.Reader, rows []componentHook) (bool, int) {
	id := legArg(c.args)
	for _, row := range rows {
		if id != "" && row.ID == id && row.Event == c.args[0] {
			return true, row.Run(c, in)
		}
	}
	return false, 0
}
