package execution

import (
	"reflect"
	"strings"
	"testing"
)

// The limit lives on a pair, and the legacy single model/reasoningEffort declaration is a pair
// written another way, so both spellings read it.
func TestAutoCompactTokenLimitIsReadOnAPairAndOnTheLegacyDeclaration(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		roles doc
	}{
		{"legacy-single", doc{"child": doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 550000}}},
		{"pairs", doc{"child": doc{"pairs": []any{doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 550000}}}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			p := mustLoad(t, rolesPolicy(scenario.roles, true))
			role, ok := p.Role("child")
			if !ok || len(role.Pairs) != 1 || role.Pairs[0].AutoCompactTokenLimit == nil || *role.Pairs[0].AutoCompactTokenLimit != 550000 {
				t.Fatalf("role=%+v ok=%v", role, ok)
			}
			if !role.Allows(model, effort) {
				t.Fatal("the limit changed which pairs the role allows")
			}
		})
	}
}

// A policy that declares no limit is unchanged: the pair carries nothing and the file reads as it
// always did.
func TestAutoCompactTokenLimitAbsentLeavesThePolicyAsItWas(t *testing.T) {
	p := mustLoad(t, rolesPolicy(nil, true))
	role, ok := p.Role("child")
	if !ok || len(role.Pairs) != 1 || role.Pairs[0].AutoCompactTokenLimit != nil {
		t.Fatalf("role=%+v ok=%v", role, ok)
	}
}

// A value that is not a positive integer is refused while the file is read, through the policy error
// every other unusable value is refused with. No new refusal name appears.
func TestAutoCompactTokenLimitIsRefusedWhenItIsNotAPositiveInteger(t *testing.T) {
	for _, value := range []string{`"550000"`, `0`, `-1`, `550000.0`, `1.5e6`, `true`, `null`} {
		t.Run(value, func(t *testing.T) {
			raw := []byte(`{"roles":{"child":{"model":"` + model + `","reasoningEffort":"` + effort + `","autoCompactTokenLimit":` + value + `}}}`)
			_, err := FromBytes(raw, "test-policy")
			refusal := policyError(t, err)
			if !strings.Contains(refusal.Detail, "autoCompactTokenLimit") {
				t.Fatalf("the refusal does not name the field: %q", refusal.Detail)
			}
		})
	}
}

// A limit on the pairs list is refused on the same path, and a pairs entry is still exactly one
// pair of model and effort however the limit is written.
func TestAutoCompactTokenLimitOnAPairsEntryIsReadAndRefusedTheSameWay(t *testing.T) {
	good := doc{"child": doc{"pairs": []any{doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 1}}}}
	p := mustLoad(t, rolesPolicy(good, true))
	role, _ := p.Role("child")
	if role.Pairs[0].AutoCompactTokenLimit == nil || *role.Pairs[0].AutoCompactTokenLimit != 1 {
		t.Fatalf("role=%+v", role)
	}
	bad := doc{"child": doc{"pairs": []any{doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": "550000"}}}}
	_, err := load(t, rolesPolicy(bad, true))
	policyError(t, err)
	// Two entries that name the same model and effort are still the same pair, limit or not.
	twice := doc{"child": doc{"pairs": []any{
		doc{"model": model, "reasoningEffort": effort},
		doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 550000},
	}}}
	_, err = load(t, rolesPolicy(twice, true))
	if !strings.Contains(policyError(t, err).Detail, "twice") {
		t.Fatalf("err=%v", err)
	}
}

// The limit is not part of what a request is judged against; it travels out of the authorization so
// the caller can send it, and it is absent for a supervisor or an exception, which name no pair.
func TestAuthorizeCarriesTheMatchedPairsLimit(t *testing.T) {
	p := mustLoad(t, rolesPolicy(doc{"child": doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 550000}}, true))
	auth := authorize(t, p, Input{Model: model, Effort: effort, Role: "child"})
	if auth.AutoCompactTokenLimit == nil || *auth.AutoCompactTokenLimit != 550000 {
		t.Fatalf("authorized=%+v", auth)
	}
	if role, _ := p.Role("child"); role.Pairs[0].AutoCompactTokenLimit == nil {
		t.Fatal("the limit was dropped from the role")
	}
	// A supervisor declares no pair, so there is nothing for a limit to belong to.
	if _, err := load(t, rolesPolicy(doc{"supervisor": doc{"expectation": "record", "autoCompactTokenLimit": 550000}}, true)); err == nil {
		t.Fatal("a supervisor was allowed to declare a limit")
	}
}

// The policy description is the authorization surface, not an inventory of how a pair runs. The
// limit is deliberately absent from it, so every existing description and refusal is unchanged.
func TestThePolicyDescriptionDoesNotMentionTheLimit(t *testing.T) {
	withLimit := mustLoad(t, rolesPolicy(doc{"child": doc{"model": model, "reasoningEffort": effort, "autoCompactTokenLimit": 550000}}, true))
	without := mustLoad(t, rolesPolicy(doc{"child": doc{"model": model, "reasoningEffort": effort}}, true))
	if !reflect.DeepEqual(withLimit.Summary()["roles"], without.Summary()["roles"]) {
		t.Fatalf("with=%v without=%v", withLimit.Summary()["roles"], without.Summary()["roles"])
	}
}
