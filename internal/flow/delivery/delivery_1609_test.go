package delivery

import (
	"context"
	"errors"
	"testing"
)

func TestCRW1609DeliveryValidAndObserve(t *testing.T) {
	criteria := []string{"one", "two"}
	remote := Check{Kind: RemotePush}
	issues := Check{Kind: IssuesClosed}
	msref := Check{Kind: MilestoneClosed, Ref: "owner/repo#6"}
	// canonical ids, whole criteria only
	valid, problems := Valid(criteria, []Criterion{{ID: "c1", Checks: []Check{remote, issues}}, {ID: "c2", Checks: []Check{msref}}})
	if len(valid) != 2 || len(problems) != 0 {
		t.Fatalf("valid %v problems %v", valid, problems)
	}
	// invalid annotations leave the criterion in the build
	bad := []Criterion{
		{ID: "c3", Checks: []Check{remote}},                  // no such criterion
		{ID: "c1", Checks: nil},                              // empty checks
		{ID: "c1", Checks: []Check{remote, remote}},          // repeated check
		{ID: "c1", Checks: []Check{{Kind: "bogus"}}},         // unknown check
		{ID: "c2", Checks: []Check{{Kind: MilestoneClosed}}}, // missing ref
		{ID: "1c", Checks: []Check{remote}},                  // non-canonical id
	}
	for _, annotation := range bad {
		valid, problems = Valid(criteria, []Criterion{annotation})
		if len(valid) != 0 || len(problems) != 1 {
			t.Fatalf("invalid annotation %+v: %v valid, %v problems", annotation, valid, problems)
		}
	}
	// duplicate id
	if valid, problems = Valid(criteria, []Criterion{{ID: "c1", Checks: []Check{remote}}, {ID: "c1", Checks: []Check{remote}}}); len(problems) != 2 {
		t.Fatalf("duplicates: %v valid %v problems", valid, problems)
	}
	// Observe: remote exact head passes; ancestor of a newer tip passes; not-ancestor pends; read errors unobserved.
	c1 := []Criterion{{ID: "c1", Checks: []Check{remote, issues}}}
	target := Target{Checkout: "/repo", Branch: "dev", Head: "abc123", Issues: []string{"owner/repo#1"}}
	obs := Observe(context.Background(), c1, target, Reader{
		Remote:   func(context.Context, string, string) (string, error) { return "def456", nil },
		Ancestor: func(context.Context, string, string, string) (bool, error) { return true, nil },
		Issue:    func(context.Context, string) (string, string, error) { return "closed", "completed", nil },
	})
	if State(obs) != Passed {
		t.Fatalf("observations %v", obs)
	}
	obs = Observe(context.Background(), c1, target, Reader{
		Remote: func(context.Context, string, string) (string, error) { return "", errors.New("offline") },
		Issue:  func(context.Context, string) (string, string, error) { return "closed", "not_planned", nil },
	})
	if State(obs) != Unobserved {
		t.Fatalf("unobserved must win: %v", obs)
	}
	obs = Observe(context.Background(), c1, target, Reader{
		Remote:   func(context.Context, string, string) (string, error) { return "def456", nil },
		Ancestor: func(context.Context, string, string, string) (bool, error) { return false, nil },
		Issue:    func(context.Context, string) (string, string, error) { return "closed", "completed", nil },
	})
	if State(obs) != Pending {
		t.Fatalf("pending: %v", obs)
	}
	if State(nil) != Unobserved {
		t.Error("empty observations must not pass")
	}
}
