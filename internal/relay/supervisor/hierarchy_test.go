package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type staticLinkage struct{ reading map[string]any }

func (s staticLinkage) Up(_ context.Context, _ string) (map[string]any, error) { return s.reading, nil }

func Test24_SCH_1_LiveHierarchy(t *testing.T) {
	t.Parallel()
	supervisorMirror(t, "OnlyTheLiveHierarchyDecidesWhoIsTold.test_the_project_owner_sends_and_the_initiative_owner_receives", "setup", func(c *Channel, _ *store.Store) []any {
		got, err := c.Resolve(context.Background(), captureRelationID(t, c))
		if err != nil {
			t.Fatal(err)
		}
		return []any{got.Sender, got.Recipient, got.ProjectKey, got.InitiativeKey, got.Source}
	})

	for _, tc := range []struct {
		id      string
		reading map[string]any
		result  bool
		gap     bool
	}{
		{"OnlyTheLiveHierarchyDecidesWhoIsTold.test_a_project_nobody_supervises_has_nowhere_to_report_and_still_owes_one", map[string]any{"state": "resolved", "readable": true, "contention": []any{}, "gaps": []any{map[string]any{"gap": "no_supervisor", "scopeKind": "project", "scopeKey": "PRJ-1"}}, "levels": []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent-task"}, "depth": 1}}}, false, true},
		{"OnlyTheLiveHierarchyDecidesWhoIsTold.test_an_unreadable_hierarchy_is_not_a_missing_supervisor", map[string]any{"state": "unreadable", "readable": false, "levels": []any{}, "gaps": []any{}, "contention": []any{}}, false, false},
		{"OnlyTheLiveHierarchyDecidesWhoIsTold.test_two_candidates_above_are_not_resolved_by_choosing_one", map[string]any{"state": "ambiguous", "readable": true, "levels": []any{}, "gaps": []any{}, "contention": []any{map[string]any{"contention": "competing_owners", "scopeKind": "initiative", "scopeKey": "INI-1", "candidates": []any{"01a", "01b"}}}}, false, false},
		{"OnlyTheLiveHierarchyDecidesWhoIsTold.test_a_drifting_owner_holds_the_report_rather_than_picking_a_side", map[string]any{"state": "resolved", "readable": true, "levels": []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent-task"}, "depth": 1}}, "gaps": []any{}, "contention": []any{map[string]any{"contention": "owner_drift", "linkId": "lnk-1", "recorded": "01parent-task", "live": "01someone-else"}}}, false, false},
		{"OnlyTheLiveHierarchyDecidesWhoIsTold.test_a_retained_refusal_row_does_not_silence_a_project_for_good", map[string]any{"state": "resolved", "readable": true, "levels": []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent-task"}, "depth": 1}, map[string]any{"scopeKind": "initiative", "scopeKey": "INI-1", "owner": map[string]any{"taskId": "01supervisor-task"}, "depth": 2}}, "gaps": []any{}, "contention": []any{map[string]any{"reason": "role_already_bound", "scopeKind": "project", "scopeKey": "PRJ-1", "at": "2026-01-01T00:00:00Z"}}}, true, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			snapshot := "setup"
			if tc.gap {
				snapshot = "event"
			}
			supervisorMirror(t, tc.id, snapshot, func(c *Channel, _ *store.Store) []any {
				c.Linkage = staticLinkage{tc.reading}
				got, err := c.Resolve(context.Background(), captureRelationID(t, c))
				if tc.result {
					if err != nil {
						t.Fatal(err)
					}
					return []any{got.Recipient}
				}
				var refusal Refusal
				if !errors.As(err, &refusal) {
					t.Fatalf("expected refusal: %v", err)
				}
				if tc.gap {
					return []any{refusal.Reason, strings.Contains(refusal.Detail, "nobody to report to"), "standing"}
				}
				return []any{refusal.Reason}
			})
		})
	}
	levels := []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent"}}, map[string]any{"scopeKind": "initiative", "scopeKey": "INI-1", "owner": map[string]any{"taskId": "01supervisor"}}}
	cases := []struct {
		name           string
		reading        map[string]any
		reason, detail string
		want           Resolution
	}{
		{name: "resolved", reading: map[string]any{"readable": true, "state": "resolved", "levels": levels}, want: Resolution{"01parent", "01supervisor", "PRJ-1", "INI-1", "linkage"}},
		{name: "no_supervisor", reading: map[string]any{"readable": true, "state": "resolved", "levels": levels[:1], "gaps": []any{map[string]any{"gap": "no_supervisor"}}}, reason: "unregistered_scope", detail: "nobody to report to"},
		{name: "unreadable", reading: map[string]any{"readable": false, "state": "unreadable"}, reason: "relation_unreadable"},
		{name: "competing_owners", reading: map[string]any{"readable": true, "state": "ambiguous", "contention": []any{map[string]any{"contention": "competing_owners"}}}, reason: "duplicate_scope_owner"},
		{name: "owner_drift", reading: map[string]any{"readable": true, "state": "resolved", "contention": []any{map[string]any{"contention": "owner_drift"}}, "levels": levels[:1]}, reason: "relation_owner_drift"},
		{name: "retained_refusal", reading: map[string]any{"readable": true, "state": "resolved", "contention": []any{map[string]any{"reason": "role_already_bound"}}, "levels": levels}, want: Resolution{"01parent", "01supervisor", "PRJ-1", "INI-1", "linkage"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Channel{Linkage: staticLinkage{tc.reading}}
			got, err := c.Resolve(context.Background(), "rel-1")
			if tc.reason != "" {
				var refusal Refusal
				if !errors.As(err, &refusal) || refusal.Reason != tc.reason || !strings.Contains(refusal.Detail, tc.detail) {
					t.Fatalf("got %+v, %v; want %s containing %q", got, err, tc.reason, tc.detail)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func Test24_SCH_4_WrongRecipient(t *testing.T) {
	t.Parallel()
	supervisorMirror(t, "OnlyTheLiveHierarchyDecidesWhoIsTold.test_an_unreadable_hierarchy_is_not_a_missing_supervisor", "setup", func(c *Channel, _ *store.Store) []any {
		c.Linkage = staticLinkage{map[string]any{"state": "unreadable", "readable": false, "levels": []any{}, "gaps": []any{}, "contention": []any{}}}
		_, err := c.Resolve(context.Background(), "irrelevant")
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("expected refusal: %v", err)
		}
		return []any{refusal.Reason}
	})

	c := Channel{Linkage: staticLinkage{map[string]any{"readable": true, "state": "resolved", "levels": []any{map[string]any{"scopeKind": "project", "scopeKey": "PRJ-1", "owner": map[string]any{"taskId": "01parent"}}, map[string]any{"scopeKind": "initiative", "scopeKey": "INI-1", "owner": map[string]any{"taskId": "01supervisor"}}}}}}
	_, err := c.ResolveRecipient(context.Background(), "rel-1", "01someone-else")
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "recipient_not_authorized" || !strings.Contains(refusal.Detail, "01supervisor") {
		t.Fatalf("wrong recipient: %v", err)
	}
}
