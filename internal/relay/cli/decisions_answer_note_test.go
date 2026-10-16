package cli_test

import "testing"

// CRW-1325: an answer may give a choice and its prose together. The choice is the answer
// decision-apply reads, and the prose is stored on the same decision beside it. These cases go
// through the command and the SQLite store, so what they read back is what was written.

const crw1325Prose = "merge once the retention window closes"

// crw1325AnswerBoth records the supervisor's user-grade answer that gives the merge option and its prose.
func crw1325AnswerBoth(t *testing.T, state, decision string) {
	t.Helper()
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "merge", "--text", crw1325Prose, "--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("decision-answer with an option and text: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
}

// The option is the choice decision-apply reads: a reply that answers the merge option applies the
// decision whose answer gave that option and its prose together.
func TestCRW1325OptionAndTextApplyOnTheChoice(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818RaiseBlocker(t, state)
	crw1325AnswerBoth(t, state, decision)
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "answered" || record["answer_text"] != "merge" {
		t.Fatalf("the answer is %v", record)
	}
	event := review818SeedReply(t, state, "rel-903", "ev-blocked", "answer", 1)
	if got := crw737Run(t, "--state", state, "decision-apply", "--decision", decision, "--event", event); got.code != 0 {
		t.Fatalf("decision-apply: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "applied" {
		t.Fatalf("the applied record is %v", record)
	}
}

// CRW-1325 acceptance: an answer that gives an option and prose is accepted, and both are read back
// from the store: the option as the answer, the prose beside it.
func TestCRW1325OptionAndTextAreStored(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818Raise(t, state, "Which window does the host update take?")
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "now", "--text", "because it is idle", "--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("an option and text: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "answered" || record["answer_text"] != "now" || record["answer_note"] != "because it is idle" {
		t.Fatalf("the answer is %v", record)
	}
}

// An option or prose that the authority and the option set do not allow is refused, and the refusal
// leaves the decision raised.
func TestCRW1325OptionAndTextNeedTheSupervisorAndAKnownOption(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818RaiseBlocker(t, state)
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "merge", "--text", crw1325Prose, "--by", "task-other", "--via", "dots", "--authority", "user"), "authority_unverified")
	crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--option", "bogus", "--text", crw1325Prose, "--by", "task-sup", "--via", "dots", "--authority", "user"), "bad_invocation")
	if record := crw737List(t, state)[0].(map[string]any); record["state"] != "raised" {
		t.Fatalf("a refused answer left the record %v", record["state"])
	}
}

// A text-only answer keeps its meaning: the text is the answer_text, and no note is stored.
func TestCRW1325TextOnlyAnswerStaysReadable(t *testing.T) {
	state := crw737Store(t)
	review818SeedSupervisor(t, state, "task-sup")
	decision := review818Raise(t, state, "Which window does the host update take?")
	if got := crw737Run(t, "--state", state, "decision-answer", "--decision", decision,
		"--text", "between runs", "--by", "task-sup", "--via", "dots", "--authority", "user"); got.code != 0 {
		t.Fatalf("a text-only answer: exit %d %s %s", got.code, got.stdout, got.stderr)
	}
	record := crw737List(t, state)[0].(map[string]any)
	if record["state"] != "answered" || record["answer_text"] != "between runs" || record["answer_note"] != nil {
		t.Fatalf("the text-only answer is %v", record)
	}
}
