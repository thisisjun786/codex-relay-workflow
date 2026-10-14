package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// LineageDepthLimit is how many parent threads ChildBindingByLineage follows from a sub-agent thread before it gives up.
const LineageDepthLimit = 4

// lineageMetaLimit bounds the first line of a rollout (the session_meta record, which carries the thread's base instructions).
const lineageMetaLimit = 8 << 20

var lineageThreadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ChildBindingByLineage is the child push guard's fallback for a sub-agent thread whose payload session_id the relay does not know as a
// child (CRW-1370): if the host stamps the immediate parent's thread id, not the root session's, into the hook input of a thread two or more
// levels below a relay child, session_id binds to nothing. Starting from the sub-agent's own rollout (transcriptPath), it reads each
// rollout's session_meta record (its first line), asks bind about the parent_thread_id it names, and goes on to that parent's rollout, at most
// LineageDepthLimit parents deep. It reads rollout files only; it writes nothing and runs no command.
//
// child is true, with the parent's working directory, when a thread of the lineage is a relay child. It is false with an empty unreadable
// when the lineage ended at a thread with no parent and none of the threads is a child (a parent, an integrator: not judged). A rollout that
// cannot be found or read, a lineage deeper than the limit and a relay record that cannot be read are never a child and never a refusal:
// unreadable says why, and the caller warns and records it (fail-open).
func ChildBindingByLineage(ctx context.Context, transcriptPath string, bind func(ctx context.Context, session string) (bool, string, error)) (child bool, parentCwd, unreadable string) {
	path := transcriptPath
	for hop := 1; hop <= LineageDepthLimit+1; hop++ {
		parent, err := rolloutParent(path)
		if err != nil {
			return false, "", "the rollout of the sub-agent's lineage cannot be read: " + err.Error()
		}
		if parent == "" {
			return false, "", ""
		}
		if hop > LineageDepthLimit {
			return false, "", fmt.Sprintf("the lineage of the sub-agent thread is deeper than %d parents", LineageDepthLimit)
		}
		isChild, cwd, err := bind(ctx, parent)
		if err != nil {
			return false, "", "the relay's record of a parent thread cannot be read: " + err.Error()
		}
		if isChild {
			return true, cwd, ""
		}
		if path, err = rolloutOf(path, parent); err != nil {
			return false, "", "the rollout of a parent thread cannot be found: " + err.Error()
		}
	}
	return false, "", ""
}

// openRollout opens a rollout for rolloutParent; a test reads through it how much of the file was read.
var openRollout = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// rolloutParent is the parent_thread_id the session_meta record of a rollout names, "" for a thread that is not a sub-agent's.
func rolloutParent(path string) (string, error) {
	f, err := openRollout(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The read stops one byte past the limit (CRW-1387): a first line without a newline is never read whole, however long the file.
	line, err := bufio.NewReaderSize(io.LimitReader(f, lineageMetaLimit+1), 64<<10).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(line) > lineageMetaLimit {
		return "", errors.New("the first line of the rollout is too long")
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			ParentThreadID string `json:"parent_thread_id"`
			Source         any    `json:"source"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil || record.Type != "session_meta" {
		return "", errors.New("the first line of the rollout is not a session_meta record")
	}
	source, ok := record.Payload.Source.(map[string]any)
	if !ok {
		return "", nil
	}
	subagent, _ := source["subagent"].(map[string]any)
	spawn, _ := subagent["thread_spawn"].(map[string]any)
	parent, _ := spawn["parent_thread_id"].(string)
	if parent == "" {
		return "", nil
	}
	if !lineageThreadID.MatchString(parent) {
		return "", errors.New("the parent_thread_id of the rollout is not a thread id")
	}
	return parent, nil
}

// rolloutOf finds the rollout of a thread next to the rollout of its child: the rollouts of a Codex home live under its sessions directory
// (sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl), found from the child's own path, and under its archived_sessions directory.
func rolloutOf(childRollout, thread string) (string, error) {
	var root string
	for dir := filepath.Dir(childRollout); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "sessions" {
			root = filepath.Dir(dir)
			break
		}
	}
	if root == "" {
		return "", errors.New("the rollout is not under a sessions directory")
	}
	for _, pattern := range []string{
		filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"),
		filepath.Join(root, "archived_sessions", "rollout-*-"+thread+".jsonl"),
	} {
		if found, _ := filepath.Glob(pattern); len(found) > 0 {
			return found[0], nil
		}
	}
	return "", errors.New("no rollout names the thread " + thread)
}
