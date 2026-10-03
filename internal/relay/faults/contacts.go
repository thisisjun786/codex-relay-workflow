package faults

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Contacts is the supervisor channel's reading of whether the task a notice would go to can be told
// now (supervision.contactable): that task's last host observation and how current it is. The channel
// owns the reading. This package asks for it here because the supervisor package imports this one, and
// it asks wherever eligibility is read: the fault commands, which build their own Ledger, and the
// deliverer.
type Contacts interface {
	// Contactable answers for recipient at now (epoch seconds), reading s on ctx's transaction. The
	// answer carries "contactable" (true, false, or nil when it could not be measured) and a "reason"; an
	// empty recipient is answered as not asked.
	Contactable(ctx context.Context, s *store.Store, recipient string, now float64) (map[string]any, error)
}

var contacts Contacts

// SetContacts installs the reading. The supervisor package installs its own from init, so every program
// that links both has it; call this only from init. Not parallel-safe, like the command registry.
func SetContacts(c Contacts) { contacts = c }

var errNoContacts = errors.New("no supervisor contact reading is installed, so whether the level above can be told is unknown")

// contactable asks the installed reading. With none installed it refuses rather than guess: a missing
// host measurement never authorizes a send.
func contactable(ctx context.Context, l *Ledger, recipient string, now float64) (map[string]any, error) {
	if contacts == nil {
		return nil, errNoContacts
	}
	return contacts.Contactable(ctx, l.Store, recipient, now)
}
