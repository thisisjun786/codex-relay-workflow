package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// Multi-agent V2's spawn_agent returns the child's agent path ("/root/<task_name>") and no thread id, so a report names the
// child by that path. The host's native thread database holds both: threads.agent_path of the child and the
// thread_spawn_edges row that makes it a child of the session. The checked boundary turns the path into the thread id once, at
// the start of a report, and the report then runs on the thread id exactly as it does for V1 (CRW-1260).

// createdAgentPathShape is an agent path: "/" and one or more segments of the characters a task name has (lowercase letters,
// digits and underscores; upper case and "-" are allowed so a host that widens the set is not refused here), no empty or dot
// segment, no trailing slash.
var createdAgentPathShape = regexp.MustCompile(`^(?:/[A-Za-z0-9_-]+)+$`)

const createdAgentPathMax = 512

// createdAgentPath reports whether v is written as an agent path (a string that starts with "/"), and whether it is a valid one.
func createdAgentPath(v any) (path string, isPath, valid bool) {
	s, ok := v.(string)
	if !ok || len(s) == 0 || s[0] != '/' {
		return "", false, false
	}
	return s, true, len(s) <= createdAgentPathMax && createdAgentPathShape.MatchString(s)
}

// createdBindAgentPath returns b with an agentId written as an agent path replaced by the thread id the path names among the
// children of the dispatch's session; any other b is returned as it is. The caller's map is never changed. A path that is not
// valid is "invalid agentId", as any other malformed id is; one that names no single child is refused with its reason and the
// correction every created refusal ends with, and the report writes nothing.
func createdBindAgentPath(ctx context.Context, env host.LookupEnv, d *Dispatch, b map[string]any) (map[string]any, error) {
	path, isPath, valid := createdAgentPath(b["agentId"])
	if !isPath {
		return b, nil
	}
	if !valid {
		return nil, errors.New("invalid agentId")
	}
	a := d.Attempts[len(d.Attempts)-1]
	id, err := createdResolveAgentPath(ctx, env, d.SessionID, d.ID, a.ID, path)
	if err != nil {
		return nil, err
	}
	bound := maps.Clone(b)
	bound["agentId"] = id
	return bound, nil
}

// createdResolveAgentPath is the thread id of the child of session whose agent path is path, read from the native thread
// database. The edge table is the witness of "child of the session" where the host has it, the thread's own spawn source where it
// does not. A task name the session used more than once names several threads; the one whose first message carries the marker of
// this attempt is the child, and without exactly one such thread the path is ambiguous.
func createdResolveAgentPath(ctx context.Context, env host.LookupEnv, session, dispatch, attempt, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var found []createdCheckIdentity
	noColumn := false
	err := createdCheckWithDB(ctx, env, func(conn *sql.Conn, columns map[string]bool) error {
		if !columns["agent_path"] {
			noColumn = true
			return nil
		}
		var edges int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'thread_spawn_edges'").Scan(&edges); err != nil {
			return err
		}
		query := "SELECT t.id FROM threads t WHERE t.agent_path = ?"
		args := []any{path}
		if edges > 0 {
			query = "SELECT t.id FROM threads t JOIN thread_spawn_edges e ON e.child_thread_id = t.id WHERE t.agent_path = ? AND e.parent_thread_id = ?"
			args = append(args, session)
		}
		rows, err := conn.QueryContext(ctx, query+" ORDER BY t.id", args...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, id := range ids {
			id, source, row, err := createdCheckScan(ctx, conn, columns, "WHERE id = ?", id)
			if err != nil {
				return err
			}
			if row, err = createdCheckParent(id, source, row); err == nil && row.Subagent && row.Parent == session {
				found = append(found, row)
			}
		}
		return nil
	})
	switch {
	case noColumn:
		return "", errors.New("the host's thread database records no agent paths, so " + path + " cannot be resolved to a thread" + createdCheckCorrection)
	case err != nil:
		return "", errors.New("host could not resolve agent path " + path + " to a thread" + createdCheckCorrection)
	}
	if len(found) > 1 {
		var marked []createdCheckIdentity
		for _, row := range found {
			if managedSpawnCarries(row.FirstMessage, dispatch, attempt) {
				marked = append(marked, row)
			}
		}
		found = marked
		if len(found) == 0 {
			return "", errors.New("agent path " + path + " names more than one child of this session and none carries this attempt's marker" + createdCheckCorrection)
		}
		if len(found) > 1 {
			return "", errors.New("agent path " + path + " names more than one child of this session that carries this attempt's marker" + createdCheckCorrection)
		}
	}
	if len(found) == 0 {
		return "", errors.New("agent path " + path + " names no child of this session in the host's thread database" + createdCheckCorrection)
	}
	return found[0].ID, nil
}

// createdForkedFrom is the thread the thread whose rollout is path was forked from, "" when the first line of the rollout is not a
// session_meta that names one or cannot be read. A forked thread starts with a copy of the history it was forked from, so its
// first user message is that history's and not the task it was spawned for.
func createdForkedFrom(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	f, err := dispatchOpenRollout(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	from, first := "", true
	_ = dispatchRolloutScan(ctx, f, func(line []byte) {
		if !first {
			return
		}
		first = false
		cancel() // only the first line is read
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ForkedFrom *string `json:"forked_from_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &meta) == nil && meta.Type == "session_meta" && meta.Payload.ForkedFrom != nil {
			from = *meta.Payload.ForkedFrom
		}
	})
	return from
}
