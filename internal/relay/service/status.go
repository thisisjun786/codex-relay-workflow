package service

import (
	"context"
	"database/sql"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"sort"
)

func (s *Service) Status(ctx context.Context) Object {
	r := s.Record()
	owner, h, detail := s.Ownership(r)
	if h != nil {
		_ = h.Close()
	}
	intent := s.Intent()
	held := s.LockIsHeld()
	launch := s.ResolveLaunchPolicy()
	observed := s.ReadWorkerPolicy(ctx)
	var running any
	if truth(get(observed, "observed")) {
		policy, _ := get(observed, "policy").(Object)
		running = get(policy, "digest")
	}
	matches := "unknown"
	if truth(running) && truth(get(launch, "digest")) {
		matches = "different"
		if equal(running, get(launch, "digest")) {
			matches = "same"
		}
	}
	launch = set(launch, "appliesTo", "the next daemon launched from this state directory", "runningDigest", running, "matchesRunning", matches)
	return obj("enabled", get(intent, "enabled"), "intentConfigured", get(intent, "configured"), "intentChangedAt", get(intent, "changedAt"), "intentChangedBy", get(intent, "changedBy"), "launchPolicy", launch, "running", held, "ownership", owner, "ownershipDetail", nullable(detail), "pid", get(r, "pid"), "workerPid", get(r, "workerPid"), "startedAt", get(r, "startedAt"), "storeId", get(r, "storeId"), "socketPath", nullable(s.Socket), "installationId", s.InstallationID, "stateDirectory", s.Selection.Path, "scopeAuthority", s.Scope.Authority, "scopeRoot", s.Scope.Root, "lock", func() string {
		if held {
			return "held"
		}
		return "free"
	}(), "staleRecord", r != nil && truth(get(r, "pid")) && !held, "restarts", get(r, "restarts"), "consecutiveFailures", get(r, "consecutiveFailures"), "lastExit", get(r, "lastExit"), "nextRestartAt", get(r, "nextRestartAt"), "conflicts", s.Conflicts(), "projects", s.Projects(ctx))
}
func (s *Service) Projects(ctx context.Context) Object {
	type group struct {
		count, active   int
		parents, issues map[string]bool
	}
	groups := map[string]*group{}
	answer := store.ReadOnlyRows(ctx, s.Selection, "SELECT relationship_id, issue_key, status, parent_task_id, parent_host_id, parent_cwd FROM relationships WHERE superseded_by IS NULL", nil, func(r store.RowScanner) error {
		var rid, issue, status, parent, host string
		var cwd sql.NullString
		if err := r.Scan(&rid, &issue, &status, &parent, &host, &cwd); err != nil {
			return err
		}
		key := delivery.ProjectKey(delivery.Relationship{Parent: delivery.Endpoint{HostID: host, Cwd: cwd.String}})
		g := groups[key]
		if g == nil {
			g = &group{parents: map[string]bool{}, issues: map[string]bool{}}
			groups[key] = g
		}
		g.count++
		if status == "active" {
			g.active++
		}
		g.parents[parent] = true
		g.issues[issue] = true
		return nil
	})
	out := []any{}
	if !answer.Readable || answer.Detail != "" {
		return obj("available", false, "detail", nullable(answer.Detail), "projects", out)
	}
	keys := []string{}
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := func(m map[string]bool) []any {
		keys := []string{}
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		a := []any{}
		for _, k := range keys {
			a = append(a, k)
		}
		return a
	}
	for _, k := range keys {
		g := groups[k]
		out = append(out, obj("project", k, "assignments", g.count, "active", g.active, "parents", sorted(g.parents), "issues", sorted(g.issues)))
	}
	return obj("available", true, "detail", nil, "projects", out)
}
