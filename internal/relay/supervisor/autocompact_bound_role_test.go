package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// The settings fence cancels a claim when a re-read of the recipient's authorized settings differs
// from what the send read. The role the gate confirmed is part of what the send reads: the pair's
// auto-compaction limit is resolved from it, so a re-read that lands on a different binding must
// differ here too, or the transport would run on the pair the first read confirmed while the store
// now says the task is bound to another role.
func TestTheSettingsFenceSeesAChangedBoundRole(t *testing.T) {
	data := delivery.Obj{{Key: "model", Value: "anthropic/claude-opus-5"}, {Key: "reasoningEffort", Value: "xhigh"}}
	same := func(role string) *delivery.TaskSettings {
		return &delivery.TaskSettings{Data: append(delivery.Obj{}, data...), BoundRole: role}
	}
	if !sameSettings24(same("child"), same("child")) {
		t.Fatal("two reads of the same binding differ")
	}
	if sameSettings24(same("child"), same("parent")) {
		t.Fatal("a re-read that landed on another bound role reads as the same settings")
	}
	if sameSettings24(same("child"), same("")) {
		t.Fatal("a re-read that found no bound role reads as the same settings")
	}
}
