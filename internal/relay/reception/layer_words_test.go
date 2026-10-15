package reception

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1476 (bundle n2-layer-words): the text reception returns for a person names the layer (부모,
// 자식), never a relay role id written as a bare word. The relay role id `supervisor` is the 부모
// (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"); the direction keys
// (parent_to_supervisor and the others) are wire values and stay.

func requireNoRoleWords(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Errorf("%s: nothing rendered", what)
		return
	}
	if hits := layername.Find(text); len(hits) > 0 {
		var words []string
		for _, h := range hits {
			start, end := max(0, h.Offset-40), min(len(text), h.Offset+len(h.Word)+40)
			words = append(words, h.Word+" in "+`"`+text[start:end]+`"`)
		}
		t.Errorf("%s names a role id where a person reads a layer (write 부모 / 자식): %s", what, strings.Join(words, "; "))
	}
}

// upwardNoMechanism is the one wording of what a report upward cannot answer (plan D6); the
// supervisor channel's reach ladder carries the same sentence.
const upwardNoMechanism = "a report upward is transported and read back, and nothing here records that the 부모 agreed, applied or verified anything; what became of it is read from the tracking record, confirmed"

// TestReceptionNoMechanismNamesTheParent: a parent_to_supervisor reach ladder that answers agreed:
// yes is refused, and the refusal ends with the shared sentence, which names the 부모.
func TestReceptionNoMechanismNamesTheParent(t *testing.T) {
	t.Parallel()
	id, err := MessageID("parent_to_supervisor", "rel-1", "completion", "event-1")
	if err != nil {
		t.Fatal(err)
	}
	reach := Obj{}
	for _, name := range reachNames {
		state := "unmeasured"
		if name != "transport_accepted" && name != "received" {
			state = "not_applicable"
		}
		if name == "agreed" {
			state = "yes"
		}
		reach = append(reach, O(name, Stage(state, nil, "a reading"))...)
	}
	envelope := O("version", "relay-envelope/1", "direction", "parent_to_supervisor", "kind", "notification", "purpose", "completion",
		"messageId", id, "relationId", "rel-1", "subject", "event-1",
		"sender", O("role", "parent", "taskId", "01parent"), "recipient", O("role", "supervisor", "taskId", "01supervisor"), "reach", reach)
	err = Check(O("version", "relay-packet/1", "envelope", envelope))
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "malformed_receipt" {
		t.Fatalf("an upward ladder answering agreed: yes was not refused as malformed: %v", err)
	}
	if !strings.HasSuffix(refused.Detail, ": "+upwardNoMechanism) {
		t.Errorf("the refusal %q does not end with the shared sentence %q", refused.Detail, upwardNoMechanism)
	}
	requireNoRoleWords(t, "the upward ladder refusal", refused.Detail)
	if noMechanism["parent_to_supervisor"] != upwardNoMechanism {
		t.Errorf("noMechanism[parent_to_supervisor] = %q, want %q", noMechanism["parent_to_supervisor"], upwardNoMechanism)
	}
}
