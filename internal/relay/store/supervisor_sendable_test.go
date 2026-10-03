package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// The truth table of the supervisor_messages "can be sent" condition. Every builder in
// supervisor_sendable.go is run over one seeded store that holds a row for each combination of
// state, hold, eligibility time and lease (each time before, at and after now), and the rows it
// selects are compared with a plain-Go reading of the same words. The Go predicates are held to the
// same rows, so the SQL fragments and the Go checks cannot drift apart.

const sendableNow = 100.0

type sendableProbe struct {
	id          string
	state       string
	hold        *string
	next, lease *float64
}

func (p sendableProbe) unsent() bool {
	return p.state == "queued" || p.state == "deferred_busy" || p.state == "withheld_pre_send"
}
func (p sendableProbe) due() bool { return p.next == nil || *p.next <= sendableNow }
func (p sendableProbe) claimable() bool {
	return p.unsent() && p.hold == nil && p.due()
}
func (p sendableProbe) inFlight() bool {
	return p.state == "sending" && p.lease != nil && *p.lease > sendableNow
}
func (p sendableProbe) stranded() bool {
	return p.state == "sending" && p.lease != nil && *p.lease <= sendableNow
}

func sendableLabel(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

func seedSendableMatrix(t *testing.T, s *Store) []sendableProbe {
	t.Helper()
	ctx := context.Background()
	empty, held := "", "held"
	var probes []sendableProbe
	for _, state := range []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "dispatched", "read"} {
		for _, hold := range []*string{nil, &empty, &held} {
			for _, next := range []*float64{nil, ptrFloat(50), ptrFloat(sendableNow), ptrFloat(150)} {
				for _, lease := range []*float64{nil, ptrFloat(50), ptrFloat(sendableNow), ptrFloat(150)} {
					holdLabel := "null"
					if hold != nil {
						holdLabel = fmt.Sprintf("%q", *hold)
					}
					p := sendableProbe{id: fmt.Sprintf("%s|hold=%s|next=%s|lease=%s", state, holdLabel, sendableLabel(next), sendableLabel(lease)), state: state, hold: hold, next: next, lease: lease}
					var holdArg, nextArg, leaseArg any
					if hold != nil {
						holdArg = *hold
					}
					if next != nil {
						nextArg = *next
					}
					if lease != nil {
						leaseArg = *lease
					}
					_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, next_eligible_at, hold_reason, lease_until, staged_at, updated_at) VALUES (?,?,'report','rel','p','k','parent','sup','s','{}',?,?,?,?,'t','t')", p.id, "obl-"+p.id, state, nextArg, holdArg, leaseArg)
					must(t, err)
					probes = append(probes, p)
				}
			}
		}
	}
	return probes
}

func ptrFloat(v float64) *float64 { return &v }

func selectedIDs(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.All(context.Background(), query, args...)
	must(t, err)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Get("message_id").(string))
	}
	slices.Sort(ids)
	return ids
}

func expectedIDs(probes []sendableProbe, want func(sendableProbe) bool) []string {
	var ids []string
	for _, p := range probes {
		if want(p) {
			ids = append(ids, p.id)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestSupervisorSendableSQL_selects_the_rows_its_words_describe(t *testing.T) {
	t.Parallel()
	// Given: one row for every combination of state, hold, eligibility time and lease.
	s := recordStore(t)
	probes := seedSendableMatrix(t, s)
	if len(probes) != 7*3*4*4 {
		t.Fatalf("seeded %d rows", len(probes))
	}
	const sel = "SELECT message_id FROM supervisor_messages WHERE "
	cases := []struct {
		name     string
		fragment string
		args     []any
		want     func(sendableProbe) bool
	}{
		{"unsent", SupervisorUnsentSQL(""), nil, sendableProbe.unsent},
		{"restatable", SupervisorRestatableSQL(""), nil, func(p sendableProbe) bool { return p.unsent() || p.state == "sending" }},
		{"due", SupervisorDueSQL(""), []any{sendableNow}, sendableProbe.due},
		{"claimable", SupervisorClaimableSQL(""), []any{sendableNow}, sendableProbe.claimable},
		{"ahead", SupervisorAheadSQL(""), []any{sendableNow, sendableNow}, func(p sendableProbe) bool { return p.claimable() || p.inFlight() }},
		{"ahead under the claim's lock", SupervisorAheadInClaimSQL(""), []any{sendableNow, sendableNow}, func(p sendableProbe) bool {
			return p.hold == nil && (p.unsent() && p.due() || p.inFlight())
		}},
		{"attemptable", SupervisorAttemptableSQL(""), []any{sendableNow, sendableNow}, func(p sendableProbe) bool { return p.claimable() || p.stranded() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// When: the fragment is the whole WHERE clause.
			got := selectedIDs(t, s, sel+c.fragment, c.args...)
			// Then: exactly the rows the plain-Go reading names are selected.
			want := expectedIDs(probes, c.want)
			if !slices.Equal(got, want) {
				t.Fatalf("selected %d rows, want %d\nonly selected: %v\nonly expected: %v", len(got), len(want), difference(got, want), difference(want, got))
			}
			if len(want) == 0 || len(want) == len(probes) {
				t.Fatalf("the table does not separate this condition: %d of %d rows", len(want), len(probes))
			}
			// And: the same fragment under a table alias selects the same rows.
			aliased := selectedIDs(t, s, "SELECT m.message_id AS message_id FROM supervisor_messages m WHERE "+aliasedFragment(c.name, "m"), c.args...)
			if !slices.Equal(aliased, want) {
				t.Fatalf("aliased fragment selected %d rows, want %d", len(aliased), len(want))
			}
		})
	}
}

func aliasedFragment(name, alias string) string {
	switch name {
	case "unsent":
		return SupervisorUnsentSQL(alias)
	case "restatable":
		return SupervisorRestatableSQL(alias)
	case "due":
		return SupervisorDueSQL(alias)
	case "claimable":
		return SupervisorClaimableSQL(alias)
	case "ahead":
		return SupervisorAheadSQL(alias)
	case "ahead under the claim's lock":
		return SupervisorAheadInClaimSQL(alias)
	}
	return SupervisorAttemptableSQL(alias)
}

func difference(a, b []string) []string {
	var out []string
	for _, id := range a {
		if !slices.Contains(b, id) && len(out) < 5 {
			out = append(out, id)
		}
	}
	return out
}

func TestSupervisorMessagesRow_predicates_agree_with_the_SQL_on_every_row(t *testing.T) {
	t.Parallel()
	// Given: the truth table, and the rows the SQL fragments select from it.
	s := recordStore(t)
	probes := seedSendableMatrix(t, s)
	ctx := context.Background()
	unsent := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+SupervisorUnsentSQL(""))
	claimable := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+SupervisorClaimableSQL(""), sendableNow)
	for _, p := range probes {
		// When: the row is read back as a value.
		row, err := s.SupervisorMessage(ctx, p.id)
		must(t, err)
		// Then: its Go predicates answer as the SQL does.
		if got, want := row.Unsent(), slices.Contains(unsent, p.id); got != want || SupervisorUnsent(row.State) != want {
			t.Fatalf("%s: Unsent()=%v SupervisorUnsent=%v, SQL says %v", p.id, got, SupervisorUnsent(row.State), want)
		}
		if got, want := row.ClaimableAt(sendableNow), slices.Contains(claimable, p.id); got != want {
			t.Fatalf("%s: ClaimableAt=%v, SQL says %v", p.id, got, want)
		}
	}
}

func TestSupervisorOlderThanSQL_orders_by_staging_time_then_id(t *testing.T) {
	t.Parallel()
	// Given: four messages, two staged at the same instant.
	s := recordStore(t)
	ctx := context.Background()
	for _, m := range []struct{ id, at string }{{"m-a", "t1"}, {"m-b", "t2"}, {"m-c", "t2"}, {"m-d", "t3"}} {
		_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,'report','rel','p','k','parent','sup','s','{}','queued',?,?)", m.id, "obl-"+m.id, m.at, m.at)
		must(t, err)
	}
	// When: the messages older than the one staged at t2 with id m-c are selected.
	got := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+SupervisorOlderThanSQL(""), "t2", "t2", "m-c")
	// Then: it is the earlier one and the same-instant one with the smaller id, never the message itself.
	if want := []string{"m-a", "m-b"}; !slices.Equal(got, want) {
		t.Fatalf("older than m-c: %v, want %v", got, want)
	}
}

func TestSupervisorNeverSentSQL_needs_an_unsent_state_and_no_attempt_that_may_have_gone(t *testing.T) {
	t.Parallel()
	// Given: messages with each kind of attempt, and one in a state that is not unsent.
	s := recordStore(t)
	ctx := context.Background()
	insert := func(id, state string) {
		_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,'report','rel','p','k','parent','sup','s','{}',?,'t','t')", id, "obl-"+id, state)
		must(t, err)
	}
	attempt := func(id string, no int, sent string, retrySafe int) {
		_, err := s.DB.ExecContext(ctx, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, observed_at) VALUES (?,?,?,'m','withheld_pre_send',?,?,'{}','t','t')", fmt.Sprintf("%s-a%d", id, no), id, no, sent, retrySafe)
		must(t, err)
	}
	insert("untouched", "queued")
	insert("safe-no-send", "deferred_busy")
	attempt("safe-no-send", 1, "no", 1)
	insert("unsafe-no-send", "withheld_pre_send")
	attempt("unsafe-no-send", 1, "no", 0)
	insert("sent", "queued")
	attempt("sent", 1, "yes", 1)
	insert("unknown", "queued")
	attempt("unknown", 1, "unknown", 0)
	insert("one-safe-one-sent", "queued")
	attempt("one-safe-one-sent", 1, "no", 1)
	attempt("one-safe-one-sent", 2, "yes", 1)
	insert("dispatched", "dispatched")
	// When: the condition selects, and when it guards an UPDATE (it names the table, so it must work there).
	got := selectedIDs(t, s, "SELECT message_id FROM supervisor_messages WHERE "+SupervisorNeverSentSQL())
	result, err := s.DB.ExecContext(ctx, "UPDATE supervisor_messages SET updated_at='moved' WHERE "+SupervisorNeverSentSQL())
	must(t, err)
	moved, err := result.RowsAffected()
	must(t, err)
	// Then: only an unsent message whose attempts all sent nothing and were shown retry-safe is selected.
	if want := []string{"safe-no-send", "untouched"}; !slices.Equal(got, want) {
		t.Fatalf("never sent: %v, want %v", got, want)
	}
	if moved != 2 {
		t.Fatalf("UPDATE moved %d rows, want 2", moved)
	}
}
