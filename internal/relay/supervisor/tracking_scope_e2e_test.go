package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/routing"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type trackingClock struct{}

func (trackingClock) ISO() string  { return "2026-10-14T00:00:00.000000+00:00" }
func (trackingClock) Now() float64 { return 1792000000 }

func trackingNS(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

// TestTrackingRefScopeRunsThroughRegistryRoutingReceptionAndSupervisor is CRW-1345's end condition at
// package level: on a fresh temporary store, a scope keyed by a tracking ref ("o/r#7", under the
// milestone "o/r#3" of the repository "o/r") is bound in the registry, completed in product routing,
// and read back by reception and the supervisor channel, and no Linear identifier is given anywhere.
func TestTrackingRefScopeRunsThroughRegistryRoutingReceptionAndSupervisor(t *testing.T) {
	const (
		issue, project, milestone = "o/r#7", "o/r", "o/r#3"
		parentTask, childTask     = "01parent", "01child"
		supervisorTask            = "01supervisor"
	)
	ctx := faults.WithInputs(context.Background(), trackingClock{}, bytes.NewReader(bytes.Repeat([]byte{7}, 4096)))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Registry: the milestone's supervisor, the repository's owner and the issue's child are bound
	// by their tracking refs, and the relationship is registered and attached to the repository.
	reg := &registry.Registry{Store: s, Now: trackingClock{}.ISO, Policy: registry.ResolveRolePolicy(map[string]string{})}
	parentEP := registry.Endpoint{TaskID: parentTask, HostID: "host", Cwd: trackingNS("/parent"), CXCSession: trackingNS("cxc-parent")}
	childEP := registry.Endpoint{TaskID: childTask, HostID: "host", Cwd: trackingNS("/work"), CXCSession: trackingNS("cxc-child")}
	supervisorEP := registry.Endpoint{TaskID: supervisorTask, HostID: "host", Cwd: trackingNS("/supervisor")}
	if _, err = reg.RegisterSupervision(ctx, milestone, project, supervisorEP, parentEP, "execution"); err != nil {
		t.Fatalf("supervision of %s by %s: %v", project, milestone, err)
	}
	if _, err = reg.BindScope(ctx, "supervisor", milestone, supervisorEP); err != nil {
		t.Fatalf("binding the milestone %s: %v", milestone, err)
	}
	if _, err = reg.BindScope(ctx, "parent", project, parentEP); err != nil {
		t.Fatalf("binding the project %s: %v", project, err)
	}
	if _, err = reg.BindScope(ctx, "child", issue, childEP); err != nil {
		t.Fatalf("binding the issue %s: %v", issue, err)
	}
	relationship, err := reg.Register(ctx, registry.Registration{Parent: parentEP, Child: childEP, IssueKey: issue,
		ArtifactRoots: []string{"/work"}, AllowedRecipients: []string{parentTask}, DispatchRequestID: "dispatch-1", DispatchTurnID: trackingNS("turn-1")})
	if err != nil {
		t.Fatalf("registering %s: %v", issue, err)
	}
	if _, err = reg.AttachIssue(ctx, relationship.ID, project); err != nil {
		t.Fatalf("attaching %s to %s: %v", issue, project, err)
	}
	for scope, want := range map[[2]string]string{{"issue", issue}: childTask, {"project", project}: parentTask, {"initiative", milestone}: supervisorTask} {
		owner, err := reg.Owner(ctx, scope[0], scope[1])
		if err != nil || owner == nil {
			t.Fatalf("owner of %s %s: %v (%v)", scope[0], scope[1], owner, err)
		}
		if got := owner.Get("taskId"); got != want {
			t.Fatalf("owner of %s %s = %v, want %s", scope[0], scope[1], got, want)
		}
	}

	// Routing: the issue is bound as read back from the tracker, and a completion reading about it is
	// recorded.
	router := routing.New(s, trackingClock{})
	if _, err = router.RegisterProduct(ctx, map[string]any{"schema": "product-registry/1", "product": "crw", "workspace": "ws", "team": project, "familyLabel": "crw"}); err != nil {
		t.Fatalf("registering the product: %v", err)
	}
	if _, err = router.Bind(ctx, map[string]any{"schema": "product-binding/1", "product": "crw", "kind": "issue", "ref": issue, "title": "t", "state": "open", "source": "the tracker, read back",
		"followUpOf": []any{map[string]any{"issue": "o/r#5", "checks": []any{"install"}}}}); err != nil {
		t.Fatalf("binding %s in routing: %v", issue, err)
	}
	answer, err := router.CheckCompletion(ctx, map[string]any{"schema": "completion-reading/1", "product": "crw", "subject": issue,
		"claims": map[string]any{"prMerged": true}, "requires": map[string]any{"install": true}, "observed": map[string]any{"install": "present"}})
	if err != nil {
		t.Fatalf("completion of %s: %v", issue, err)
	}
	if got := pyjson.Map(answer)["subject"]; got != issue {
		t.Fatalf("completion answered for %v, want %s", got, issue)
	}

	// Reception: the child reads its assignment's relationship, which names the tracking ref.
	packet := reception.O("envelope", reception.O("direction", "parent_to_child", "relationId", relationship.ID))
	read, err := reception.Reader{Policy: reg.Policy}.Read(ctx, s, childTask, packet, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := reception.Get(read, "record")
	if reception.Get(record, "issue") != issue || reception.Get(record, "relationId") != relationship.ID {
		t.Fatalf("reception read %v, want the relationship %s of %s (notes %v)", record, relationship.ID, issue, reception.Get(read, "notes"))
	}
	if reception.Get(record, "relationRevision") == nil {
		t.Fatalf("reception read no revision of the execution link: notes %v", reception.Get(read, "notes"))
	}

	// Supervisor: the channel walks issue, project and milestone and names the milestone's supervisor.
	channel := &Channel{Store: s, Linkage: StoreLinkage{s}}
	resolution, err := channel.Resolve(ctx, relationship.ID)
	if err != nil {
		t.Fatalf("resolving the report from %s: %v", issue, err)
	}
	if resolution.Sender != parentTask || resolution.Recipient != supervisorTask || resolution.ProjectKey != project || resolution.InitiativeKey != milestone {
		t.Fatalf("resolution = %+v", resolution)
	}

	// Nowhere in the store is a Linear identifier: every key recorded is a tracking ref or the repository.
	for _, query := range []string{"SELECT issue_key FROM relationships", "SELECT scope_key FROM scope_bindings", "SELECT upper_key FROM scope_links UNION SELECT lower_key FROM scope_links"} {
		rows, err := s.All(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s read nothing", query)
		}
		for _, row := range rows {
			for _, f := range []string{"issue_key", "scope_key", "upper_key", "lower_key"} {
				if key, ok := row.Get(f).(string); ok && !strings.HasPrefix(key, project) {
					t.Fatalf("%s: %q is not a tracking ref of %s", query, key, project)
				}
			}
		}
	}
}
