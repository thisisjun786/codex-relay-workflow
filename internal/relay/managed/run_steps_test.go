package managed

import (
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func TestRecipientsWithAddsTheChildOnlyWhenAbsent(t *testing.T) {
	t.Parallel()
	req := map[string]any{"allowedRecipients": []any{"parent", "other"}}
	if got, want := recipientsWith(req, "child"), []string{"parent", "other", "child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recipients %q, want %q", got, want)
	}
	if got, want := recipientsWith(req, "other"), []string{"parent", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recipients %q, want %q", got, want)
	}
	if got := len(req["allowedRecipients"].([]any)); got != 2 {
		t.Fatalf("the request's recipients were changed: %d", got)
	}
}

func TestSettingsWithRoleCopiesAndNamesTheRole(t *testing.T) {
	t.Parallel()
	settings := map[string]any{"model": "gpt-5", "citedRole": "stale"}
	got := settingsWithRole(settings, "child")
	if want := map[string]any{"model": "gpt-5", "citedRole": "child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings %v, want %v", got, want)
	}
	if settings["citedRole"] != "stale" || len(settings) != 2 {
		t.Fatalf("the input was changed: %v", settings)
	}
	if got := settingsWithRole(nil, "parent"); !reflect.DeepEqual(got, map[string]any{"citedRole": "parent"}) {
		t.Fatalf("settings of nothing: %v", got)
	}
}

func TestCriteriaEntriesKeepTheRequestsOrderAndFields(t *testing.T) {
	t.Parallel()
	req := map[string]any{"criteria": []any{
		map[string]any{"id": "c2", "title": "second", "required": false},
		map[string]any{"id": "c1", "title": "first", "required": true},
	}}
	want := []any{
		delivery.Obj{{Key: "id", Value: "c2"}, {Key: "title", Value: "second"}, {Key: "required", Value: false}},
		delivery.Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: "first"}, {Key: "required", Value: true}},
	}
	if got := criteriaEntries(req); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}
	if got := criteriaEntries(map[string]any{"criteria": []any{}}); got == nil || len(got) != 0 {
		t.Fatalf("entries of no criteria: %#v", got)
	}
}
