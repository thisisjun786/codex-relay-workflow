package callstate

import "testing"

func rows(events ...string) []Row {
	out := make([]Row, len(events))
	for i, e := range events {
		out[i] = Row{Event: e}
	}
	return out
}

func unknown(reason string, before ...string) []Row {
	r := rows(before...)
	return append(r, Row{Event: OutcomeUnknown, Reason: reason})
}

// A call is in recovery hold only when its outcome was left unknown at a dispatch intent for the reason dispatch_unknown and no
// row reopened it; every other unfinished call is for the resume of the run; a call that ended is settled.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		rows []Row
		want Class
	}{
		{"completed", rows(Prepared, ThreadRequested, ThreadStarted, TurnRequested, TurnStarted, Completed), Settled},
		{"refused", rows(Prepared, Refused), Settled},
		{"superseded", rows(Prepared, ThreadRequested, Superseded), Settled},
		{"null_model", rows(Prepared, TurnRequested, NullModel), Settled},
		{"a dispatch intent nobody answered", rows(Prepared, ThreadRequested), Unsettled},
		{"a turn that was requested and never ended", rows(Prepared, ThreadRequested, ThreadStarted, TurnRequested), Unsettled},
		{"a turn that started and never ended", rows(Prepared, ThreadRequested, ThreadStarted, TurnRequested, TurnStarted), Unsettled},
		{"prepared only", rows(Prepared), Unsettled},
		{"held", rows(Prepared, ThreadRequested, ThreadStarted, TurnRequested, TurnStarted, Held), Unsettled},
		{"held before any intent", rows(Prepared, Held), Unsettled},
		{"unknown outcome at a creation", unknown(DispatchUnknown, Prepared, ThreadRequested), RecoveryHold},
		{"unknown outcome at a turn", unknown(DispatchUnknown, Prepared, ThreadRequested, ThreadStarted, TurnRequested), RecoveryHold},
		{"unknown outcome at a started turn", unknown(DispatchUnknown, Prepared, ThreadRequested, ThreadStarted, TurnRequested, TurnStarted), RecoveryHold},
		{"an unknown outcome of another reason", unknown("other", Prepared, ThreadRequested), Unsettled},
		{"an unknown outcome with no intent", unknown(DispatchUnknown, Prepared), Unsettled},
		{"the hold was cleared", append(unknown(DispatchUnknown, Prepared, ThreadRequested), Row{Event: RecoveryCleared}), Unsettled},
		{"cleared and adopted", append(unknown(DispatchUnknown, Prepared, ThreadRequested), Row{Event: RecoveryCleared}, Row{Event: ThreadStarted}), Unsettled},
		{"cleared, adopted and finished", append(unknown(DispatchUnknown, Prepared, ThreadRequested), Row{Event: RecoveryCleared}, Row{Event: ThreadStarted}, Row{Event: Completed}), Settled},
		{"a new attempt after an unknown outcome", append(unknown(DispatchUnknown, Prepared, ThreadRequested), Row{Event: Prepared}), Unsettled},
		{"an interrupt request changes nothing", append(unknown(DispatchUnknown, Prepared, ThreadRequested, ThreadStarted, TurnRequested), Row{Event: Interrupt}), RecoveryHold},
		{"no rows", nil, Settled},
		{"a session step nobody bound", rows(Prepared, StepPublished), Unsettled},
		{"a session call held, then going on in place with a new step", rows(Prepared, StepPublished, ThreadStarted, TurnStarted, Held, StepPublished), Unsettled},
		{"a session call whose attempt was lost", rows(Prepared, StepPublished, ThreadStarted, TurnStarted, SessionAttemptLost), Unsettled},
		{"a session call that completed after a lost attempt", rows(Prepared, StepPublished, ThreadStarted, TurnStarted, SessionAttemptLost, StepPublished, ThreadStarted, TurnStarted, Completed), Settled},
		{"a session step after a refusal reopens the call", rows(Prepared, Refused, StepPublished), Unsettled},
	}
	for _, c := range cases {
		if got := Classify(c.rows); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
