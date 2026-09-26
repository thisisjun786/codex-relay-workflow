package managed

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"testing"
)

func Test27_MRS_5_EnsureSettingsKeepsEqualAndRefusesDrift(t *testing.T) {
	r, _ := fixtureReservation(t)
	ctx := context.Background()
	first := map[string]any{"cwd": "/parent", "model": "claude"}
	source, err := EnsureSettings(ctx, r.Store, "parent", first, "creation_result", r.now())
	if err != nil || source != "creation_result" {
		t.Fatal(source, err)
	}
	source, err = EnsureSettings(ctx, r.Store, "parent", first, "recovery", r.now())
	if err != nil || source != "creation_result" {
		t.Fatal(source, err)
	}
	_, err = EnsureSettings(ctx, r.Store, "parent", map[string]any{"cwd": "/parent", "model": "other"}, "recovery", r.now())
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "relationship_conflict" {
		t.Fatal(err)
	}
	record, err := r.Store.AuthorizedSettings(ctx, "parent")
	if err != nil || record.Source != "creation_result" {
		t.Fatal(record, err)
	}
	source, err = EnsureSettings(ctx, r.Store, "absent", first, "recovery", r.now())
	if err != nil || source != "recovery" {
		t.Fatal(source, err)
	}
}
