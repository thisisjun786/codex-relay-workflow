package linkage

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test26_CCL1_existing_linkage_commands_are_registered(t *testing.T) {
	// CCL-1 is split by owner: merge-turn commands belong to 26B, capacity/region to 27.
	for _, name := range []string{"linkage-bind", "linkage-supervise", "linkage-peer", "linkage-attach", "linkage-outstanding", "linkage-completion", "linkage-handover", "linkage-directive", "linkage-settle", "linkage-down", "linkage-up", "linkage-counterpart"} {
		var stdout, stderr byteBuffer
		code := registry.ExecuteAs(context.Background(), "codex-session-relay", []string{name, "--help"}, &stdout, &stderr, nil)
		if code != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", name, code, stdout.String(), stderr.String())
		}
	}
}

type byteBuffer struct{ data []byte }

func (b *byteBuffer) Write(p []byte) (int, error) { b.data = append(b.data, p...); return len(p), nil }
func (b *byteBuffer) Len() int                    { return len(b.data) }
func (b *byteBuffer) String() string              { return string(b.data) }

func Test26_CoordinationExact_matches_python_source(t *testing.T) {
	// Go callers provide strings; Python's refusal of another type was internal to it.
	for _, input := range []string{"alpha", " ", "", "a|b", "\u00e9", "a'b", "line\nbreak"} {
		got, err := registry.CoordinationExact(input, "a target")
		answer := map[string]any{"output": got}
		if err != nil {
			answer = refusalOf(t, err)
		}
		golden.CheckJSON(t, strconv.Quote(input), answer)
	}
}

func Test26_CoordinationID_matches_python_source(t *testing.T) {
	for i, fields := range [][]string{{"owner/repo", "dev"}, {"owner", "repo|dev"}, {"\u00e9", "dev"}} {
		golden.CheckJSON(t, strconv.Itoa(i), registry.CoordinationID("mrg", fields...))
	}
}

func Test26_CoordinationRefusal_matches_python_record_and_error(t *testing.T) {
	r := registry.CoordinationRefusal{Reason: contract.RefusalUnregisteredScope, Detail: "target must not contain '|'", Domain: registry.DomainMergeTarget, Subject: "dev", Challenger: "other"}
	golden.CheckJSON(t, "record", decode(t, r.Record()))
	golden.CheckJSON(t, "error", refusalOf(t, r.Error()))
}

func Test26_CoordinationConflicts_upsert_and_read_match_python(t *testing.T) {
	w := newWorld(t)
	r := registry.CoordinationRefusal{Reason: contract.RefusalUnregisteredScope, Detail: "target must not contain '|'", Domain: registry.DomainMergeTarget, Subject: "dev", Challenger: "other"}
	if err := w.r.RecordCoordinationRefusal(w.ctx, r, "2023-11-14T22:13:20Z"); err == nil {
		t.Fatal("refusal was not returned")
	}
	r.Detail = "new detail"
	if err := w.r.RecordCoordinationRefusal(w.ctx, r, "2023-11-14T22:13:21Z"); err == nil {
		t.Fatal("refusal was not returned")
	}
	rows, err := w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, "dev")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]any, 0, len(rows))
	for _, row := range rows {
		got = append(got, decode(t, row))
	}
	golden.CheckJSON(t, "conflicts", got)
}

// refusalOf is a refusal's reason and detail, as a step records them.
func refusalOf(t *testing.T, err error) map[string]any {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("not a refusal: %v", err)
	}
	return map[string]any{"reason": refused.Reason, "detail": refused.Detail}
}
