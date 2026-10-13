package cli_test

import "testing"

// CRW-1333: a manager holding the store seat that registers its management seat lays the store seat
// down (the seat moves to the management seat). The user-grade and delegated-management-grade
// answers it records stay its own: the seat the answer is checked against is the store seat, read
// through the management seat that took it over, and through that seat's handover.

// TestDecisionAnswerAfterTheManagerRegistersItsManagementSeat drives the relay's own command lines:
// linkage-bind of the store seat, linkage-bind --scope-kind management (what crw manage supervisor
// register runs), then a user-grade decision-answer by the manager, by another thread, and by the
// management seat's successor after a handover.
func TestDecisionAnswerAfterTheManagerRegistersItsManagementSeat(t *testing.T) {
	state := crw737Store(t)
	for _, argv := range [][]string{
		{"linkage-bind", "--role", "supervisor", "--scope", "store", "--scope-kind", "store", "--task", "task-sup", "--host", "host"},
		{"linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "task-sup", "--task", "task-sup", "--host", "host"},
	} {
		if got := crw737Run(t, append([]string{"--state", state}, argv...)...); got.code != 0 {
			t.Fatalf("%v: exit %d %s %s", argv, got.code, got.stdout, got.stderr)
		}
	}
	decision := review818Raise(t, state, "Which window does the host update take?")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-other", "--via", "dots", "--authority", "user"), "authority_unverified")
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
		"--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("the registered manager's user-grade answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}

	// The management seat changes hands: the store seat's authority goes with it.
	if got := crw737Run(t, "--state", state, "linkage-handover", "--role", "supervisor", "--scope-kind", "management", "--scope", "task-sup",
		"--expect-task", "task-sup", "--task", "task-next", "--host", "host", "--evidence", "the seat changes hands", "--actor", "test"); got.code != 0 {
		t.Fatalf("handover: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	next := review818Raise(t, state, "Which window does the restart take?")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", next, "--option", "now",
		"--by", "task-sup", "--via", "dots", "--authority", "user"), "authority_unverified")
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", next, "--option", "now",
		"--by", "task-next", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("the successor's user-grade answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}
