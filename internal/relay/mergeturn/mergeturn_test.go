package mergeturn

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func setup(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	r := &registry.Registry{Store: db}
	for _, pair := range [][2]string{{"p1", "A"}, {"p2", "B"}} {
		_, err = db.Querier(ctx).ExecContext(ctx, "INSERT INTO scope_bindings (binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)", pair[0], "parent", "project", pair[1], pair[0], "host", "active", 1, "now", "now")
		if err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Store: db, Registry: r, Now: func() string { return "2023-11-14T22:13:20.000000+00:00" }}
}
func claim(t *testing.T, s *Service, project, holder string, ready bool) map[string]any {
	t.Helper()
	v, err := s.Request(context.Background(), "/repo", "main", project, holder, "host", "head-1", ready)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// matchesGoldenJSON compares an answer's whole JSON, target keys named in order of appearance,
// with the golden under key.
func matchesGoldenJSON(t *testing.T, key string, got any) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err = json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, key, stableTargets(t, actual))
}

func Test26_target_derivation_matches_the_golden(t *testing.T) {
	for _, pair := range [][2]string{{"owner/repo", "dev"}, {"/repo", "main"}, {"owner/other", "release/1.2"}, {"owner/repo", "main"}} {
		got, err := TargetKey(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		golden.CheckJSON(t, pair[0]+" "+pair[1], got)
	}
}

func Test26_python_claim_whole_JSON(t *testing.T) {
	s := setup(t)
	matchesGoldenJSON(t, "claim", claim(t, s, "A", "p1", true))
}
func Test26_python_target_whole_JSON(t *testing.T) {
	s := setup(t)
	claim(t, s, "A", "p1", true)
	target, err := s.Target(context.Background(), "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	matchesGoldenJSON(t, "target", target)
}
func Test26_python_waiter_whole_JSON(t *testing.T) {
	s := setup(t)
	claim(t, s, "A", "p1", true)
	matchesGoldenJSON(t, "waiter", claim(t, s, "B", "p2", true))
}
func Test26_python_ack_whole_JSON(t *testing.T) {
	s := setup(t)
	a := claim(t, s, "A", "p1", true)
	g := a["grant"].(map[string]any)["grantId"].(string)
	v, err := s.Acknowledge(context.Background(), a["turnId"].(string), "p1", g, "read and checked")
	if err != nil {
		t.Fatal(err)
	}
	matchesGoldenJSON(t, "ack", v)
}
func Test26_python_release_whole_JSON(t *testing.T) {
	s := setup(t)
	a := claim(t, s, "A", "p1", true)
	claim(t, s, "B", "p2", true)
	g := a["grant"].(map[string]any)["grantId"].(string)
	if _, err := s.Acknowledge(context.Background(), a["turnId"].(string), "p1", g, "read and checked"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Release(context.Background(), a["turnId"].(string), "p1", "returned", "deferring", "")
	if err != nil {
		t.Fatal(err)
	}
	matchesGoldenJSON(t, "release", v)
}
func Test26_MTN_6_racing_claims_one_holder(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	results := make(chan struct {
		v   map[string]any
		err error
	}, 2)
	for _, p := range [][2]string{{"A", "p1"}, {"B", "p2"}} {
		go func(p [2]string) {
			v, err := s.Request(ctx, "/repo", "main", p[0], p[1], "host", "head-1", true)
			results <- struct {
				v   map[string]any
				err error
			}{v, err}
		}(p)
	}
	states := map[any]int{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		states[result.v["state"]]++
	}
	if states[Holding] != 1 || states[Waiting] != 1 {
		t.Fatal(states)
	}
}
func Test26_MTN_9_time_does_not_release_holder(t *testing.T) {
	s := setup(t)
	a := claim(t, s, "A", "p1", true)
	s.Now = func() string { return "2126-01-01T00:00:00.000000+00:00" }
	b := claim(t, s, "B", "p2", true)
	if a["state"] != Holding || b["state"] != Waiting {
		t.Fatal(a, b)
	}
}
func Test26_MTN_10_grant_has_candidate_and_no_ack(t *testing.T) {
	s := setup(t)
	a := claim(t, s, "A", "p1", true)
	g, ok := a["grant"].(map[string]any)
	if !ok || g["candidateHead"] != "head-1" || g["acknowledgedAt"] != nil {
		t.Fatal(a)
	}
}
func Test26_request_nonowner_refuses(t *testing.T) {
	s := setup(t)
	_, err := s.Request(context.Background(), "/repo", "main", "A", "p2", "host", "head-1", false)
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "scope_role_mismatch" {
		t.Fatal(err)
	}
	rows, err := s.Store.All(context.Background(), "SELECT * FROM merge_turns")
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}
