package faults

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type recordingContacts struct {
	answer    map[string]any
	err       error
	calls     int
	recipient string
	store     *store.Store
	now       float64
}

func (c *recordingContacts) Contactable(_ context.Context, s *store.Store, recipient string, now float64) (map[string]any, error) {
	c.calls++
	c.store, c.recipient, c.now = s, recipient, now
	return c.answer, c.err
}

// withContacts installs c for one test and puts back what the binary linked. The faults tests do not
// run in parallel, which is what makes swapping the package's one reading safe.
func withContacts(t *testing.T, c Contacts) {
	t.Helper()
	previous := contacts
	SetContacts(c)
	t.Cleanup(func() { SetContacts(previous) })
}

func relationshipFault(t *testing.T, relationshipStatus string) (*Ledger, context.Context) {
	t.Helper()
	l, ctx := testLedger(t)
	for _, statement := range []string{
		f1Relationship,
		"UPDATE relationships SET status='" + relationshipStatus + "'",
		"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{\"relationship\": \"rel\"}','{\"projectKey\": \"P\"}','P','open','t','t','t')",
	} {
		if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return l, ctx
}

func TestEligibility_asks_the_installed_contact_reading_about_the_relationships_parent(t *testing.T) {
	cases := []struct {
		name         string
		answer       map[string]any
		eligible     bool
		reason       string
		wantContacts bool
	}{
		{"contactable", map[string]any{"contactable": true, "reason": "yes"}, true, "reportable", true},
		{"not contactable", map[string]any{"contactable": false, "reason": "archived"}, false, "no contact: archived", true},
		{"unmeasured", map[string]any{"contactable": nil, "reason": "the host has not been observed"}, false, "contact unmeasured: the host has not been observed", true},
		{"not asked", map[string]any{"contactable": nil, "asked": false, "reason": "no recipient was named"}, true, "reportable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Given: a fault anchored to an active relationship whose parent is "parent", and a reading that answers tc.answer.
			l, ctx := relationshipFault(t, "active")
			reading := &recordingContacts{answer: tc.answer}
			withContacts(t, reading)
			// When: eligibility is read.
			got, err := l.NotificationEligibility(ctx, "abc123", 100000)
			// Then: the reading was asked once, about the parent, on this ledger's store at that moment, and its answer decides.
			if err != nil {
				t.Fatal(err)
			}
			if reading.calls != 1 || reading.recipient != "parent" || reading.store != l.Store || reading.now != 100000 {
				t.Fatalf("asked %d times about %q at %v", reading.calls, reading.recipient, reading.now)
			}
			if got["eligible"] != tc.eligible || got["reason"] != tc.reason {
				t.Fatalf("eligible %v, reason %q; want %v, %q", got["eligible"], got["reason"], tc.eligible, tc.reason)
			}
			contact, _ := got["contact"].(map[string]any)
			if contact["reason"] != tc.answer["reason"] {
				t.Fatalf("the answer the reading gave is not the contact: %v", got["contact"])
			}
		})
	}
}

func TestEligibility_refuses_to_guess_when_no_contact_reading_is_installed(t *testing.T) {
	// Given: a relationship-anchored fault and no reading installed.
	l, ctx := relationshipFault(t, "active")
	withContacts(t, nil)
	// When: eligibility is read.
	got, err := l.NotificationEligibility(ctx, "abc123", 100000)
	// Then: it answers an error and no eligibility, so nothing is sent on a measurement nobody made.
	if !errors.Is(err, errNoContacts) || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestEligibility_returns_the_contact_readings_error_whole(t *testing.T) {
	l, ctx := relationshipFault(t, "active")
	failure := errors.New("query: the table is gone")
	withContacts(t, &recordingContacts{err: failure})
	got, err := l.NotificationEligibility(ctx, "abc123", 100000)
	if err != failure || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestEligibility_does_not_ask_the_contact_reading_when_the_relationship_decides(t *testing.T) {
	for _, status := range []string{"paused", "cancelled", "archived"} {
		t.Run(status, func(t *testing.T) {
			l, ctx := relationshipFault(t, status)
			reading := &recordingContacts{answer: map[string]any{"contactable": true, "reason": "yes"}}
			withContacts(t, reading)
			got, err := l.NotificationEligibility(ctx, "abc123", 100000)
			if err != nil || reading.calls != 0 || got["eligible"] != false || got["reason"] != "the relationship is "+status {
				t.Fatalf("got %v, %v after %d asks", got, err, reading.calls)
			}
		})
	}
	t.Run("no relationship", func(t *testing.T) {
		l, ctx := testLedger(t)
		reading := &recordingContacts{}
		withContacts(t, reading)
		got, err := l.NotificationEligibility(ctx, "no-such-fault", 100000)
		if err != nil || reading.calls != 0 || got["eligible"] != true {
			t.Fatalf("got %v, %v after %d asks", got, err, reading.calls)
		}
	})
}
