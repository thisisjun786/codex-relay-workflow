// Package session ports CXC v0.2.40 session-cli.ts (commit 3c1459ac).
// Relay dispatch owns argument parsing and wraps the oracle answer under out.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Options are the arguments already read by the relay parser.
type Options struct {
	Command, SourceRoot string
	JSON                bool
}

// Result retains the oracle's answer and exit code, before the relay envelope.
type Result struct {
	Out  any
	Code int
	// Note is a diagnostic for stderr that is not part of the answer: the native root policy's note (an empty HOME
	// reads the account home), given on a refusal too. Empty when there is none.
	Note string
}

type stateCheck struct {
	exists bool
	phase  any // null for absent state, otherwise the validated phase string
}

const unreadableState = "State is unreadable or corrupt; existing bytes were preserved. Inspect the state file before retrying."

// inspectState reads identity before readState can default or normalize it.
func inspectState(cwd, sessionID string) (stateCheck, string) {
	root := filepath.Join(cwd, crwdir.DirName)
	for _, dir := range []string{root, filepath.Join(root, state.SessionsSubdir)} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return stateCheck{}, ""
		}
		if err != nil {
			return stateCheck{}, unreadableState
		}
		if !info.IsDir() {
			return stateCheck{}, "State directories must be real directories, not symlinks or non-directory files."
		}
	}
	path := state.StatePath(cwd, sessionID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return stateCheck{}, ""
	}
	if err != nil {
		return stateCheck{}, unreadableState
	}
	if !info.Mode().IsRegular() {
		return stateCheck{}, "State must be a regular file, not a symlink or directory."
	}
	// As in the oracle, parent-directory tampering by a hostile same user is out of scope.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return stateCheck{}, unreadableState
	}
	info, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return stateCheck{}, unreadableState
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return stateCheck{}, "State must be a regular file."
	}
	body, err := io.ReadAll(f)
	if err = errors.Join(err, f.Close()); err != nil {
		return stateCheck{}, unreadableState
	}
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(source.DecodeUTF8(body)))
	decoder.UseNumber() // unrelated large numbers must not change the identity checks
	if err := decoder.Decode(&parsed); err != nil {
		return stateCheck{}, unreadableState
	}
	if _, err := decoder.Token(); err != io.EOF {
		return stateCheck{}, unreadableState
	}
	raw, ok := parsed.(map[string]any)
	if !ok || raw == nil {
		return stateCheck{}, "State JSON is corrupt; existing bytes were preserved."
	}
	if raw["sessionId"] != sessionID {
		return stateCheck{}, "State sessionId does not match the native session; existing bytes were preserved."
	}
	phase, ok := raw["phase"].(string)
	if !ok || !slices.Contains(state.AllPhases(), state.Phase(phase)) {
		return stateCheck{}, "State phase is invalid; existing bytes were preserved."
	}
	return stateCheck{exists: true, phase: phase}, ""
}

// Run verifies the native owner, inspects raw state and optionally binds state or source.
// Current performs no state writes; bind never resets existing bytes.
func Run(opts Options, cwd string, env host.LookupEnv) Result {
	return run(opts, cwd, env, state.EnsureState)
}

func run(opts Options, cwd string, env host.LookupEnv, ensure func(string, string) (bool, error)) Result {
	note := ""
	fail := func(message string) Result {
		out := contract.OrderedObject{{Key: "ok", Value: false}, {Key: "error", Value: message}, {Key: "hooksVerified", Value: false}}
		if opts.JSON {
			return Result{Out: out, Code: 1, Note: note}
		}
		return Result{Out: message + "\nhooksVerified: false", Code: 1, Note: note}
	}
	identity, err := host.ResolveNativeSession(cwd, env)
	note = identity.Note
	if err != nil {
		return fail(err.Error())
	}
	checked, message := inspectState(identity.Cwd, identity.SessionID)
	if message != "" {
		return fail(message)
	}
	created := false
	if opts.Command == "bind" {
		created, err = ensure(identity.Cwd, identity.SessionID)
		if err != nil {
			return fail("Could not create session state exclusively. Check state directory access and retry; no existing state was reset.")
		}
		checked, message = inspectState(identity.Cwd, identity.SessionID)
		if message != "" {
			return fail(message)
		}
		if !checked.exists {
			return fail("Session state disappeared during bind. Retry after the concurrent operation finishes.")
		}
	}
	if opts.Command == "source" && !checked.exists {
		return fail("Bind session state before binding a source worktree.")
	}
	var sourceCwd string
	if opts.Command == "source" {
		sourceCwd, err = sourcesession.Bind(identity.Cwd, identity.SessionID, opts.SourceRoot)
	} else {
		sourceCwd, err = sourcesession.Resolve(identity.Cwd, identity.SessionID)
	}
	if err != nil {
		return fail("SOURCE-ROOT: " + err.Error())
	}
	out := contract.OrderedObject{{Key: "ok", Value: true}, {Key: "sessionId", Value: identity.SessionID}, {Key: "cwd", Value: identity.Cwd}, {Key: "sourceCwd", Value: sourceCwd}}
	if sourceCwd != identity.Cwd {
		id, err := sourcesession.Capture(identity.Cwd, identity.SessionID, sourcesession.CaptureOptions{})
		if err != nil {
			return fail("SOURCE-ROOT: " + err.Error())
		}
		out = append(out, contract.Field{Key: "sourceIdentity", Value: identityFields(id)})
	}
	out = append(out, contract.Field{Key: "source", Value: "CODEX_THREAD_ID"}, contract.Field{Key: "dbPath", Value: identity.DBPath},
		contract.Field{Key: "statePath", Value: state.StatePath(identity.Cwd, identity.SessionID)}, contract.Field{Key: "stateExists", Value: checked.exists},
		contract.Field{Key: "phase", Value: checked.phase}, contract.Field{Key: "created", Value: created}, contract.Field{Key: "hooksVerified", Value: false})
	if opts.JSON {
		return Result{Out: out, Note: note}
	}
	var lines []string
	for _, field := range out {
		value := fmt.Sprint(field.Value)
		if field.Value == nil {
			value = "null"
		} else if _, ok := field.Value.(contract.OrderedObject); ok {
			value = "[object Object]" // retained oracle text-mode defect
		}
		lines = append(lines, field.Key+": "+value)
	}
	return Result{Out: strings.Join(lines, "\n"), Note: note}
}

func identityFields(id source.Identity) contract.OrderedObject {
	out := contract.OrderedObject{{Key: "kind", Value: string(id.Kind)}, {Key: "commitSha", Value: id.CommitSha}, {Key: "dirty", Value: id.Dirty}}
	if id.TreeHash != "" {
		out = append(out, contract.Field{Key: "treeHash", Value: id.TreeHash})
	}
	out = append(out, contract.Field{Key: "capturedAt", Value: id.CapturedAt})
	if id.SourceRoot != nil {
		out = append(out, contract.Field{Key: "sourceRoot", Value: *id.SourceRoot})
	}
	return out
}
