package delivery

import (
	"context"
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Relationship is registry.get's record, read with the generation invariant enforced.
type Relationship struct {
	ID                string
	IssueKey          string
	Status            string
	Parent, Child     Endpoint
	Generation        int64
	ArtifactRoots     []string
	AllowedRecipients []string
	ScopeRef          any
	SupersededBy      any
	Generations       []Row
}

// Endpoint is one side of an assignment.
type Endpoint struct {
	TaskID, HostID string
	Cwd            any
}

// Active is status active and not superseded.
func (r Relationship) Active() bool { return r.Status == "active" && r.SupersededBy == nil }

func (r Relationship) generation(number int64) Row {
	for _, g := range r.Generations {
		if g.I("execution_generation") == number {
			return g
		}
	}
	return nil
}

// LoadRelationship is registry.get.
func LoadRelationship(ctx context.Context, s *store.Store, id string) (Relationship, error) {
	row, err := one(ctx, s, "SELECT * FROM relationships WHERE relationship_id = ?", id)
	if err != nil {
		return Relationship{}, err
	}
	if row == nil {
		return Relationship{}, refuse(UnregisteredRelationship, "no relationship %s", store.PyRepr(id))
	}
	generations, err := all(ctx, s, "SELECT * FROM generations WHERE relationship_id = ? ORDER BY execution_generation", id)
	if err != nil {
		return Relationship{}, err
	}
	r := Relationship{
		ID: id, IssueKey: row.S("issue_key"), Status: row.S("status"),
		Parent:     Endpoint{row.S("parent_task_id"), row.S("parent_host_id"), row.Opt("parent_cwd")},
		Child:      Endpoint{row.S("child_task_id"), row.S("child_host_id"), row.Opt("child_cwd")},
		Generation: row.I("execution_generation"), ScopeRef: row.Opt("scope_ref"),
		SupersededBy: row.Opt("superseded_by"), Generations: generations,
	}
	if err := json.Unmarshal([]byte(row.S("artifact_roots")), &r.ArtifactRoots); err != nil {
		return Relationship{}, err
	}
	if err := json.Unmarshal([]byte(row.S("allowed_recipients")), &r.AllowedRecipients); err != nil {
		return Relationship{}, err
	}
	if r.generation(r.Generation) == nil {
		return Relationship{}, refuse(UnknownGeneration, "%s points at generation %d which is not retained", store.PyRepr(id), r.Generation)
	}
	return r, nil
}

// RequireActive is registry.require_active.
func RequireActive(ctx context.Context, s *store.Store, id string) (Relationship, error) {
	r, err := LoadRelationship(ctx, s, id)
	if err != nil {
		return r, err
	}
	if r.Status != "active" {
		return r, refuse(RelationshipNotActive, "relationship %s is %s and is never auto-resumed", store.PyRepr(id), store.PyRepr(r.Status))
	}
	return r, nil
}

// ProjectKey is registry.project_key: grouping, never authorization.
func ProjectKey(r Relationship) string {
	if cwd, ok := r.Parent.Cwd.(string); ok && cwd != "" {
		return pyNormpath(cwd)
	}
	return "host:" + r.Parent.HostID
}
