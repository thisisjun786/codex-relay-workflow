package delivery

import (
	"path"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func pyNormpath(p string) string {
	cleaned := path.Clean(p)
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "/" + cleaned
	}
	return cleaned
}

func within(root, candidate string) bool {
	root, candidate = pyNormpath(root), pyNormpath(candidate)
	if root == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func reprList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = store.PyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// assertAssignmentDelivery is scope.assert_assignment_delivery: a delivery belongs to ONE
// assignment and goes to that assignment's own endpoint.
func assertAssignmentDelivery(r Relationship, kind, recipient string, recipientThread *string, eventRelationship *string, manifestPaths []string) error {
	rid := r.ID
	if eventRelationship != nil && *eventRelationship != rid {
		return refuse(RecipientNotAuthorized, "event belongs to relationship %s, not %s", store.PyRepr(*eventRelationship), store.PyRepr(rid))
	}
	var expected string
	switch kind {
	case Revision:
		expected = r.Child.TaskID
	case Completion, MergeTurnGrant:
		expected = r.Parent.TaskID
	default:
		return refuse(RecipientNotAuthorized, "%s is not a delivery direction this contract defines, so there is no authorized recipient for it", store.PyRepr(kind))
	}
	if recipient != expected {
		direction := "parent"
		if kind == Revision {
			direction = "child"
		}
		return refuse(RecipientNotAuthorized, "a %s for %s goes to its own %s %s, not to %s", kind, store.PyRepr(rid), direction, store.PyRepr(expected), store.PyRepr(recipient))
	}
	if recipientThread != nil && *recipientThread != recipient {
		return refuse(RecipientNotAuthorized, "the native thread %s is not the recipient task %s", store.PyRepr(*recipientThread), store.PyRepr(recipient))
	}
	if !slices.Contains(r.AllowedRecipients, recipient) {
		return refuse(RecipientNotAuthorized, "recipient %s is not in the relationship's allowed recipients", store.PyRepr(recipient))
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
			return refuse(ScopeEscape, "%s lies outside every authorized root %s", store.PyRepr(p), reprList(r.ArtifactRoots))
		}
	}
	return nil
}
