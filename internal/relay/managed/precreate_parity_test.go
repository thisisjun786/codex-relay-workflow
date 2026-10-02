package managed

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The refusals asked before creation are the ones registration and the settings record give after it, with
// the same reason, detail and error type. These tests hold each pair together: the pre-creation ask is
// compared with what the real Register and EnsureSettings return for the same fields, and with the literal
// answer, so neither can drift from the other unseen.

func parityStore(t *testing.T) (*store.Store, *registry.Registry) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &registry.Registry{Store: s, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }}
}

func TestPrecreate_SeparatorRefusalIsRegistrationsAnswer(t *testing.T) {
	cases := []struct {
		name                   string
		parent, host, issue    string
		project                string
		wantReason, wantDetail string
		admittedByBoth         bool
	}{
		{name: "issue key", parent: "parent", host: "host", issue: "REL|X", wantDetail: "issue_key must not contain '|', which is the field separator"},
		{name: "parent task id", parent: "par|ent", host: "host", issue: "REL-X", wantDetail: "parent_task_id must not contain '|', which is the field separator"},
		{name: "parent task id and issue key", parent: "par|ent", host: "host", issue: "REL|X", wantDetail: "parent_task_id must not contain '|', which is the field separator"},
		{name: "host id under a project", parent: "parent", host: "ho|st", issue: "REL-X", project: scopeProject, wantReason: "unregistered_scope", wantDetail: "the child host id must not contain '|', which is the field separator"},
		{name: "host id without a project", parent: "parent", host: "ho|st", issue: "REL-X", admittedByBoth: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			_, reg := parityStore(t)
			if c.project != "" {
				if _, err := reg.BindScopeAs(ctx, "parent", c.project, registry.Endpoint{TaskID: c.parent, HostID: "host"}, "active"); err != nil {
					t.Fatal(err)
				}
			}
			req := map[string]any{
				"issueKey": c.issue, "projectKey": c.project,
				"parent": map[string]any{"taskId": c.parent, "hostId": c.host},
				"child":  map[string]any{"hostId": c.host},
			}
			asked := separatorRefusal(req)
			_, registered := reg.Register(ctx, registry.Registration{
				Parent: registry.Endpoint{TaskID: c.parent, HostID: c.host}, Child: registry.Endpoint{TaskID: "child-new", HostID: c.host},
				IssueKey: c.issue, ArtifactRoots: []string{t.TempDir()}, AllowedRecipients: []string{c.parent, "child-new"},
				DispatchRequestID: "dispatch-1", ProjectKey: c.project,
			})
			if c.admittedByBoth {
				if asked != nil || registered != nil {
					t.Fatalf("asked %v, registered %v: neither refuses", asked, registered)
				}
				return
			}
			if asked == nil || registered == nil {
				t.Fatalf("asked %v, registered %v: both refuse", asked, registered)
			}
			askedReason, askedDetail := answer(asked)
			registeredReason, registeredDetail := answer(registered)
			if askedReason != registeredReason || askedDetail != registeredDetail {
				t.Fatalf("asked %q %q, registration %q %q", askedReason, askedDetail, registeredReason, registeredDetail)
			}
			if askedReason != c.wantReason || askedDetail != c.wantDetail {
				t.Fatalf("answer %q %q, want %q %q", askedReason, askedDetail, c.wantReason, c.wantDetail)
			}
		})
	}
}

func TestPrecreate_SettingsConflictIsEnsureSettingsAnswer(t *testing.T) {
	ctx := context.Background()
	s, _ := parityStore(t)
	recorded := map[string]any{"model": "gpt-5", "citedRole": "parent"}
	// Nothing recorded: no conflict, and asking writes nothing.
	if err := SettingsConflict(ctx, s, "parent", recorded); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM authorized_settings").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("asking wrote %d rows (%v)", rows, err)
	}
	if _, err := EnsureSettings(ctx, s, "parent", recorded, "earlier_registration", "2026-09-26T00:00:00.000000+00:00"); err != nil {
		t.Fatal(err)
	}
	// The recorded value again: neither refuses.
	if err := SettingsConflict(ctx, s, "parent", map[string]any{"citedRole": "parent", "model": "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	// A different value: both refuse with the same answer.
	other := map[string]any{"model": "gpt-other", "citedRole": "parent"}
	asked := SettingsConflict(ctx, s, "parent", other)
	_, written := EnsureSettings(ctx, s, "parent", other, "managed_start", "2026-09-26T00:00:00.000000+00:00")
	if asked == nil || written == nil {
		t.Fatalf("asked %v, written %v: both refuse", asked, written)
	}
	askedReason, askedDetail := answer(asked)
	writtenReason, writtenDetail := answer(written)
	if askedReason != writtenReason || askedDetail != writtenDetail {
		t.Fatalf("asked %q %q, EnsureSettings %q %q", askedReason, askedDetail, writtenReason, writtenDetail)
	}
	if askedReason != "relationship_conflict" || askedDetail != "'parent' already has execution settings that differ from this record; ensure_only does not overwrite them" {
		t.Fatalf("answer %q %q", askedReason, askedDetail)
	}
}
