package ownership

import (
	"regexp"
	"strings"
)

// inboxEntryName is the decision-25 entry grammar (cutover.md Wire format, Python
// inbox.is_entry_name): at most 200 characters of [A-Za-z0-9._-] or %XX, not starting
// with '.'.
var inboxEntryName = regexp.MustCompile(`^(?:[A-Za-z0-9._-]|%[0-9A-F]{2})+$`)

// IsInboxEntry reports whether a name in S/takeover-inbox is a decision-25 entry, the one
// classification of the replay (internal/relay/inbox). Every other name,
// like an unpublished '.'-temporary, is ignored by the replay and never unlinked.
func IsInboxEntry(name string) bool {
	return !strings.HasPrefix(name, ".") && len(name) <= 200 && inboxEntryName.MatchString(name)
}
