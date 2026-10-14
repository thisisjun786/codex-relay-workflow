package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/sessionhost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// This file is the backend of a session run (CRW-1316 S3, CRW-1348): the run's calls are steps its owner thread takes and role
// subagents the run binds and harvests (engine.SessionConfig), so the backend starts no thread and no turn. What it keeps of the App
// Server backend is the run's working directories: SessionBackend embeds the Allocator, so it is the run's Placer, Restorer,
// HeadKeeper and DirRemover, and it proves the nodes' effects (Prover) and reads the source a QA call ended in (SourceReader) the
// way Backend does. SessionHost adapts the reads of internal/flow/sessionhost to the engine's SessionHost.

// errSessionBackend answers a thread or turn request of a session run, which the engine never makes.
var errSessionBackend = errors.New("agent: a session run starts no thread and no turn: its calls are steps the owner thread takes")

// SessionBackend is the backend of a session run.
type SessionBackend struct {
	*Allocator
	// OwnerCwd is the owner thread's working directory (the directory the run was started in): a trace digest reads it when it is
	// a git checkout.
	OwnerCwd string
	// BoundDir is the flow-bound directory the trace digests (bound.Dir() when empty).
	BoundDir string
}

// NewSessionBackend is the backend of a session run over its allocator.
func NewSessionBackend(alloc *Allocator, ownerCwd string) (*SessionBackend, error) {
	if alloc == nil {
		return nil, errNoAllocator
	}
	return &SessionBackend{Allocator: alloc, OwnerCwd: ownerCwd}, nil
}

// StartThread implements engine.Backend: a session run starts no thread.
func (b *SessionBackend) StartThread(context.Context, engine.ThreadSpec) (engine.ThreadInfo, error) {
	return engine.ThreadInfo{}, errSessionBackend
}

// RunTurn implements engine.Backend: a session run starts no turn.
func (b *SessionBackend) RunTurn(context.Context, engine.TurnSpec) (engine.TurnOutcome, error) {
	return engine.TurnOutcome{}, errSessionBackend
}

// ProveNode implements engine.Prover, as Backend does.
func (b *SessionBackend) ProveNode(ctx context.Context, events []engine.Event, node string) (engine.NodeProof, error) {
	return ops.Prover{Git: ops.GitFunc(b.Allocator.git)}.ProveNode(ctx, events, node)
}

// SourceOf implements engine.SourceReader, as Backend does.
func (b *SessionBackend) SourceOf(ctx context.Context, cwd, _ string) (source.Identity, error) {
	if cwd == "" {
		return source.Identity{}, errors.New("the call ran in no directory")
	}
	id, err := source.CaptureContext(ctx, cwd, source.Options{ExcludeStateArtifacts: true})
	if err != nil {
		return source.Identity{}, fmt.Errorf("the source of %s could not be read: %w", cwd, err)
	}
	return id, nil
}

// Trace takes the digests of a session call's record-time checks (CRW-1316 §6.4) for its directory P: the workspace is P's
// `git status --porcelain` and HEAD; outside is the owner thread's cwd, its status and HEAD, when it is a git checkout other than
// P; shared is the refs under refs/crw/ of the run's repository other than this run's own (the run moves those itself) and the
// binding files of the flow-bound directory (the hook's ledger and claims are left out: the owner's spawns write them while a
// subagent works). A part that cannot be read digests its error, so a part that becomes readable reads as a change.
func (b *SessionBackend) Trace(ctx context.Context, workspace string) (engine.TraceDigests, error) {
	git := b.Allocator.git
	state := func(dir string) string {
		status, err1 := git(ctx, dir, nil, "status", "--porcelain=v1", "--untracked-files=all")
		head, err2 := git(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{commit}")
		return digestParts(status, errString(err1), head, errString(err2))
	}
	out := engine.TraceDigests{Workspace: state(workspace)}
	if owner := b.OwnerCwd; owner != "" && filepath.Clean(owner) != filepath.Clean(workspace) {
		if top, err := git(ctx, owner, nil, "rev-parse", "--show-toplevel"); err == nil && strings.TrimSpace(top) != "" {
			out.Outside = state(owner)
		}
	}
	refs, err := git(ctx, b.Allocator.cfg.Repo, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/crw/")
	own := "refs/crw/flow/" + b.Allocator.cfg.RunID + "/"
	var kept []string
	for _, line := range strings.Split(refs, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, own) {
			kept = append(kept, line)
		}
	}
	out.Shared = digestParts(strings.Join(kept, "\n"), errString(err), boundFiles(b.BoundDir))
	return out, nil
}

// boundFiles is the text of the binding files of the flow-bound directory, by name.
func boundFiles(dir string) string {
	if dir == "" {
		d, err := bound.Dir()
		if err != nil {
			return "no home: " + err.Error()
		}
		dir = d
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "unreadable: " + err.Error()
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), bound.FileSuffix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		b.WriteString(n + "\x00" + string(raw) + "\x00" + errString(err) + "\x00")
	}
	return b.String()
}

func digestParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SessionHost is the engine's SessionHost over the session host reads of internal/flow/sessionhost.
type SessionHost struct {
	Host *sessionhost.Host
	// Warn receives what a marker search could not tie as it should (sessionhost.MarkerSearch.Warnings); nil drops it.
	Warn func(op string, warnings []string)
}

// ReadAgent implements engine.SessionHost.
func (h SessionHost) ReadAgent(ctx context.Context, threadID string) (engine.SessionAgent, error) {
	a, err := h.Host.ReadAgent(ctx, threadID)
	if err != nil {
		if errors.Is(err, sessionhost.ErrNotFound) {
			return engine.SessionAgent{}, fmt.Errorf("%w: %v", engine.ErrSessionAgentNotFound, err)
		}
		return engine.SessionAgent{}, err
	}
	return engine.SessionAgent{ThreadID: a.ThreadID, ParentThreadID: a.ParentThreadID, Model: a.Model, Effort: a.Effort, Cwd: a.Cwd, AgentRole: a.AgentRole,
		Status: a.Status, RolloutPath: a.RolloutPath}, nil
}

// Turns implements engine.SessionHost: a completed turn with no answer (commentary only) carries FinalPhase commentary, as the
// engine's contract says.
func (h SessionHost) Turns(ctx context.Context, threadID string, limit int, full bool) ([]engine.SessionTurn, error) {
	turns, err := h.Host.Turns(ctx, threadID, limit, full)
	if err != nil {
		if errors.Is(err, sessionhost.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", engine.ErrSessionAgentNotFound, err)
		}
		return nil, err
	}
	out := make([]engine.SessionTurn, 0, len(turns))
	for _, t := range turns {
		st := engine.SessionTurn{ID: t.ID, Status: t.Status, FirstUserText: t.FirstUserText, UpdatedAt: t.UpdatedAt}
		if full {
			st.FinalText, st.FinalPhase = t.FinalText, t.FinalPhase
			if !t.Answered {
				st.FinalText, st.FinalPhase = "", sessionhost.PhaseCommentary
			}
		}
		if t.ErrorInfo != nil {
			st.ErrorInfo, st.ErrorMessage = t.ErrorInfo.CodexErrorInfo, t.ErrorInfo.Message
		}
		out = append(out, st)
	}
	return out, nil
}

// FindByMarker implements engine.SessionHost.
func (h SessionHost) FindByMarker(ctx context.Context, ownerThread, op string) (string, bool, error) {
	res, err := h.Host.FindByMarker(ctx, ownerThread, op)
	if err != nil {
		return "", false, err
	}
	if len(res.Warnings) > 0 && h.Warn != nil {
		h.Warn(op, res.Warnings)
	}
	return res.Spawn.ThreadID, res.Found, nil
}

// Interrupt implements engine.SessionHost.
func (h SessionHost) Interrupt(ctx context.Context, threadID, turnID string) error {
	return h.Host.Interrupt(ctx, threadID, turnID)
}

// Tokens implements engine.SessionHost.
func (h SessionHost) Tokens(_ context.Context, rolloutPath string) (engine.Tokens, bool) {
	u, measured, err := sessionhost.Tokens(rolloutPath)
	if err != nil || !measured {
		return engine.Tokens{}, false
	}
	return engine.Tokens{Input: u.Input, CachedInput: u.CachedInput, Output: u.Output, ReasoningOutput: u.ReasoningOutput, Total: u.Total}, true
}
