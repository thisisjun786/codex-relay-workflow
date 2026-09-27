package faults

import "testing"

func Test22_FLT_15_BudgetIsChargedOncePerRef(t *testing.T) {
	l, c := testLedger(t)
	for i := 0; i < 10; i++ {
		ref := string(rune('a' + i))
		answer, err := l.ConsumeBudget(c, "crw", "notification", ref)
		if err != nil || answer["consumed"] != true || answer["reason"] != "consumed" {
			t.Fatalf("charge %d: %+v %v", i, answer, err)
		}
	}
	repeat, err := l.ConsumeBudget(c, "crw", "notification", "a")
	if err != nil || repeat["consumed"] != true || repeat["reason"] != "this ref was already consumed" {
		t.Fatalf("repeat: %+v %v", repeat, err)
	}
	held, err := l.ConsumeBudget(c, "crw", "notification", "next")
	if err != nil || held["consumed"] != false || held["reason"] != "budget_spent" {
		t.Fatalf("budget: %+v %v", held, err)
	}
}
