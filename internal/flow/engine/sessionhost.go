package engine

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// This file is the consumer side of the session backend (CRW-1316 §3): the engine's session calls read the role subagents the
// run's owner thread (the project orchestrator) spawned through SessionHost, which internal/flow/sessionhost implements over the
// App Server (thread/read, thread/turns/list, turn/interrupt and the rollout). The engine never starts a thread or a turn of its
// own on that path: it publishes a step (StepBus) and binds the subagent a dispatch report names.

// Backend names recorded in the run's started row (`backend`).
const (
	BackendAppServer = "app-server"
	BackendSession   = "session"
)

// ErrSessionAgentNotFound is the error a SessionHost returns when the host says the thread does not exist (as opposed to a host
// that cannot be reached, whose error is any other): only a missing thread counts towards a lost attempt.
var ErrSessionAgentNotFound = errors.New("engine: the host has no such thread")

// SessionAgent is a subagent thread as thread/read describes it.
type SessionAgent struct {
	ThreadID       string
	ParentThreadID string
	// Model and Effort are the thread's configured model and reasoning effort.
	Model, Effort string
	// Cwd is the working directory the host reports (the subagent inherits its parent's).
	Cwd       string
	AgentRole string
	// Status is the thread status type: idle, notLoaded, active or systemError.
	Status string
	// RolloutPath is the thread's rollout file (thread/read `path`), "" when the host names none.
	RolloutPath string
}

// SessionTurn is one turn of a subagent thread as thread/turns/list gives it.
type SessionTurn struct {
	ID string
	// Status is the turn status: inProgress, completed, failed or interrupted.
	Status string
	// FirstUserText is the text of the turn's first userMessage item ("" in a summary that does not carry it).
	FirstUserText string
	// FinalText and FinalPhase are the turn's answer as the host's items give it: the last agentMessage whose phase is
	// final_answer, else (a provider that sets no phase) the last agentMessage without a phase when no commentary message came
	// before it, with FinalPhase "final_answer" or "". A turn whose last message is commentary and that has neither carries
	// FinalPhase "commentary" and no answer. Only a full read fills them.
	FinalText, FinalPhase string
	// ErrorInfo and ErrorMessage are Turn.error.codexErrorInfo (its variant name, verbatim) and Turn.error.message of a failed or
	// interrupted turn.
	ErrorInfo, ErrorMessage string
	// UpdatedAt is the last change the host shows for the turn (zero when it shows none).
	UpdatedAt time.Time
}

// SessionHost is what a session call reads of the host. Every method only reads, apart from Interrupt.
type SessionHost interface {
	// ReadAgent reads one thread (thread/read, includeTurns false). A thread the host does not have is ErrSessionAgentNotFound.
	ReadAgent(ctx context.Context, threadID string) (SessionAgent, error)
	// Turns lists the thread's turns, newest first: at most limit of them (0 is every one), each in full when full is set.
	Turns(ctx context.Context, threadID string, limit int, full bool) ([]SessionTurn, error)
	// FindByMarker finds the subagent of the owner thread whose turn carries the operation marker op (`<call>/t<k>-a<n>`): the
	// owner thread's spawn records joined with the subagents' first user messages. found is false when there is none; an error
	// means the search is not complete.
	FindByMarker(ctx context.Context, ownerThread, op string) (threadID string, found bool, err error)
	// Interrupt sends turn/interrupt for a turn of a subagent.
	Interrupt(ctx context.Context, threadID, turnID string) error
	// Tokens reads the last token_count of a rollout; ok is false when it cannot be read.
	Tokens(ctx context.Context, rolloutPath string) (Tokens, bool)
}

// TraceDigests are the digests a session call takes when its subagent is bound and again when it is harvested (§6.4): the state
// of the call's directory P (its `git status --porcelain` and HEAD), of what lies outside it that the subagent should not touch
// (the owner thread's cwd when it is a git checkout), and of the run's shared state (the refs under refs/crw/ of the shared
// checkout and the flow-bound directory). A difference is recorded as a warning; the result is used.
type TraceDigests struct {
	Workspace string `json:"workspace,omitempty"`
	Outside   string `json:"outside,omitempty"`
	Shared    string `json:"shared,omitempty"`
}

// SessionConfig turns the run's agent calls into session calls. Backend stays the run's Placer (and Prover, HeadKeeper); its
// StartThread and RunTurn are not used.
type SessionConfig struct {
	Host SessionHost
	// Bus is where the steps are published and the dispatch reports are read.
	Bus StepBus
	// Owner is the run's owner thread: every bound subagent's parentThreadId.
	Owner string
	// Poll is how often a bound subagent and the inbox are read (DefaultSessionPoll).
	Poll time.Duration
	// LostAfter is how long a subagent may stay notLoaded (or unknown to the host) with its marker turn unchanged before the
	// attempt counts as lost (DefaultSessionLostAfter).
	LostAfter time.Duration
	// Wait waits for d or until ctx ends; nil sleeps.
	Wait func(ctx context.Context, d time.Duration) error
	// Trace takes the digests of §6.4 for a call's directory; nil takes none.
	Trace func(ctx context.Context, workspace string) (TraceDigests, error)
	// RoleText is the role section of a packet; nil uses the role's overlay of internal/flow/roles without its native-spawn
	// section (a role subagent is a leaf).
	RoleText func(role string, qa bool) (string, error)
	// Wake stages a wake of the owner thread for the steps that wait for it (sessionwake.go, CRW-1316 §10); nil wakes nobody.
	Wake func(ctx context.Context, w SessionWake) error
	// WakeAfter is how long steps wait without a dispatch before the owner is notified again, RenotifyMax the longest interval
	// between two notifications, and UnresponsiveAfter how long they wait before the run writes its session_unresponsive
	// escalation (DefaultSessionWakeAfter, DefaultSessionRenotifyMax, DefaultSessionUnresponsiveAfter).
	WakeAfter, RenotifyMax, UnresponsiveAfter time.Duration
}

// Session defaults.
const (
	DefaultSessionPoll      = 10 * time.Second
	DefaultSessionLostAfter = 10 * time.Minute
)

func (s *SessionConfig) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return DefaultSessionPoll
}

func (s *SessionConfig) lostAfter() time.Duration {
	if s.LostAfter > 0 {
		return s.LostAfter
	}
	return DefaultSessionLostAfter
}

func (s *SessionConfig) wait(ctx context.Context, d time.Duration) error {
	if s.Wait != nil {
		return s.Wait(ctx, d)
	}
	return SleepWait(ctx, d)
}

// HeadKeeper is implemented by a backend that can read a worktree's HEAD and keep a commit on a ref of the repository: the
// executor HEAD check of a session call (§6.3) preserves a commit the subagent made on the stray ref of its attempt.
type HeadKeeper interface {
	HeadOf(ctx context.Context, path string) (string, error)
	KeepRef(ctx context.Context, ref, commit string) error
}

// StrayRef is the ref a commit an executor subagent made in its worktree is kept on: refs/crw/flow/<run>/stray/<node>/a<attempt>.
func StrayRef(runID, node string, attempt int) string {
	return StrayRefPrefix(runID) + refPart(node) + "/a" + strconv.Itoa(attempt)
}

// StrayRefPrefix is the prefix of every stray ref of a run.
func StrayRefPrefix(runID string) string { return "refs/crw/flow/" + refPart(runID) + "/stray/" }

// RecordedBackend is the backend a run's started rows record: the last one that names a backend, else the App Server backend (a
// run started before the field existed).
func RecordedBackend(events []Event, runID string) string {
	backend := ""
	for _, ev := range events {
		if ev.Kind == KindRun && ev.RunID == runID && ev.Event == "started" {
			if b := rowStr(ev.Data, "backend"); b != "" {
				backend = b
			}
		}
	}
	if backend == "" {
		return BackendAppServer
	}
	return backend
}

// StartedBackend is the backend the run's started rows record and whether the journal holds a started row of the run at all (a
// run whose first process stored none goes on with the backend it was started for).
func StartedBackend(events []Event, runID string) (string, bool) {
	return RecordedBackend(events, runID), hasStarted(events, runID)
}

// IsTerminalEvent reports whether a call row ends the call (completed, null_model, held, refused, superseded, outcome_unknown).
func IsTerminalEvent(event string) bool { return isTerminal(event) }

// hasStarted is whether the journal holds a started row of the run.
func hasStarted(events []Event, runID string) bool {
	for _, ev := range events {
		if ev.Kind == KindRun && ev.RunID == runID && ev.Event == "started" {
			return true
		}
	}
	return false
}
