package apply

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// StoreEvents reads events from the relay store: the event row, its relationship, the criteria set the relationship is judged under and
// the plan node it executes.
type StoreEvents struct{ Store *store.Store }

// Event implements EventReader.
func (s StoreEvents) Event(ctx context.Context, id string) (EventInfo, error) {
	ev := EventInfo{ID: id}
	err := s.Store.DB.QueryRowContext(ctx, `SELECT e.relationship_id, e.revision_hash, r.child_task_id, r.parent_task_id
 FROM events e JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.event_id = ?`, id).Scan(&ev.Relationship, &ev.Head, &ev.ChildTask, &ev.ParentTask)
	if errors.Is(err, sql.ErrNoRows) {
		return ev, ErrUnknownEvent
	}
	if err != nil {
		return ev, err
	}
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT DISTINCT set_digest FROM canonical_criteria WHERE relationship_id = ?`, ev.Relationship)
	if err != nil {
		return ev, err
	}
	defer rows.Close()
	var digests []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return ev, err
		}
		digests = append(digests, d)
	}
	if err := rows.Err(); err != nil {
		return ev, err
	}
	if len(digests) == 1 {
		ev.CriteriaDigest = digests[0]
	}
	// the node the relationship executes (the newest execution of the plan); none for a relationship outside a plan
	err = s.Store.DB.QueryRowContext(ctx, `SELECT plan_id, node_id FROM dag_node_executions WHERE relationship_id = ? ORDER BY execution_generation DESC LIMIT 1`, ev.Relationship).Scan(&ev.Plan, &ev.Node)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") {
		return ev, err
	}
	return ev, nil
}

// Git lists the commits of base..head, oldest first, in the repository.
func Git(ctx context.Context, repo, base, head string) ([]string, error) {
	if repo == "" {
		repo = "."
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-list", "--reverse", base+".."+head)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("apply: git rev-list %s..%s in %s: %w: %s", base, head, repo, err, strings.TrimSpace(errOut.String()))
	}
	return strings.Fields(out.String()), nil
}

// RelayCommand runs `crw relay [--state S] [--socket P] <args>` and answers its standard output. The program is the running executable.
type RelayCommand struct {
	Program, State, Socket string
	// Run replaces the process start; tests use it.
	Run func(ctx context.Context, program string, args ...string) ([]byte, int, error)
}

func (r RelayCommand) argv(args ...string) []string {
	argv := []string{"relay"}
	if r.State != "" {
		argv = append(argv, "--state", r.State)
	}
	if r.Socket != "" {
		argv = append(argv, "--socket", r.Socket)
	}
	return append(argv, args...)
}

func (r RelayCommand) run(ctx context.Context, args ...string) ([]byte, error) {
	argv := r.argv(args...)
	var out []byte
	var code int
	var err error
	if r.Run != nil {
		out, code, err = r.Run(ctx, r.Program, argv...)
	} else {
		cmd := exec.CommandContext(ctx, r.Program, argv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		out = stdout.Bytes()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code, err = exit.ExitCode(), nil
				if len(out) == 0 {
					out = stderr.Bytes()
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("crw relay %s: exit %d: %s", strings.Join(args[:min(len(args), 1)], " "), code, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Receive is Deps.Receive over `crw relay receive --event E --ack-turn T --verdict-file F`.
func (r RelayCommand) Receive(ctx context.Context, in ReceiveInput) (json.RawMessage, error) {
	out, err := r.run(ctx, "receive", "--event", in.Event, "--ack-turn", in.Turn, "--verdict-file", in.VerdictFile)
	if err != nil {
		return nil, err
	}
	if !json.Valid(bytes.TrimSpace(out)) {
		return nil, fmt.Errorf("crw relay receive answered no JSON: %s", strings.TrimSpace(string(out)))
	}
	return json.RawMessage(bytes.TrimSpace(out)), nil
}

// Correct is Deps.Correct over `crw relay dag-correct --prepare`: the instruction line the correction ruling must carry.
func (r RelayCommand) Correct(ctx context.Context, in CorrectionInput) (string, error) {
	out, err := r.run(ctx, "dag-correct", "--prepare", "--plan", in.Plan, "--node", in.Node, "--actor", in.Actor, "--manifest-request", in.ManifestRequest)
	if err != nil {
		return "", err
	}
	var answer struct {
		Instruction       string `json:"instruction"`
		DispatchRequestID string `json:"dispatch_request_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &answer); err != nil || answer.Instruction == "" {
		return "", fmt.Errorf("crw relay dag-correct --prepare answered no instruction: %s", strings.TrimSpace(string(out)))
	}
	if answer.DispatchRequestID != "" {
		return answer.Instruction + " (dispatch request " + answer.DispatchRequestID + ")", nil
	}
	return answer.Instruction, nil
}
