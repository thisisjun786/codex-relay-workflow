package delivery

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func within(root, candidate string) bool {
	root, candidate = store.Normpath(root), store.Normpath(candidate)
	if root == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

// assertAssignmentDelivery is scope.assert_assignment_delivery: a delivery belongs to ONE
// assignment and goes to that assignment's own endpoint.
func assertAssignmentDelivery(r Relationship, kind, recipient string, recipientThread *string, eventRelationship *string, manifestPaths []string) error {
	rid := r.ID
	if eventRelationship != nil && *eventRelationship != rid {
		return refuse(RecipientNotAuthorized, "event belongs to relationship %s, not %s", pyvalue.StrRepr(*eventRelationship), pyvalue.StrRepr(rid))
	}
	var expected string
	switch kind {
	case Revision:
		expected = r.Child.TaskID
	case Completion, MergeTurnGrant:
		expected = r.Parent.TaskID
	default:
		return refuse(RecipientNotAuthorized, "%s is not a delivery direction this contract defines, so there is no authorized recipient for it", pyvalue.StrRepr(kind))
	}
	if recipient != expected {
		direction := "parent"
		if kind == Revision {
			direction = "child"
		}
		return refuse(RecipientNotAuthorized, "a %s for %s goes to its own %s %s, not to %s", kind, pyvalue.StrRepr(rid), direction, pyvalue.StrRepr(expected), pyvalue.StrRepr(recipient))
	}
	if recipientThread != nil && *recipientThread != recipient {
		return refuse(RecipientNotAuthorized, "the native thread %s is not the recipient task %s", pyvalue.StrRepr(*recipientThread), pyvalue.StrRepr(recipient))
	}
	if !slices.Contains(r.AllowedRecipients, recipient) {
		return refuse(RecipientNotAuthorized, "recipient %s is not in the relationship's allowed recipients", pyvalue.StrRepr(recipient))
	}
	for _, p := range manifestPaths {
		ok := false
		for _, root := range r.ArtifactRoots {
			if within(root, p) {
				ok = true
				break
			}
		}
		if !ok {
			return refuse(ScopeEscape, "%s lies outside every authorized root %s", pyvalue.StrRepr(p), pyvalue.Repr(r.ArtifactRoots))
		}
	}
	return nil
}
