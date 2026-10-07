package policystore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// twoPairsOneLimited is a document whose child role lists two pairs, the first carrying the
// auto-compaction limit this repository now accepts on a pair.
const twoPairsOneLimited = "{\"roles\": {\"child\": {\"pairs\": [" +
	"{\"model\": \"inferhub/deepseek-v4.1-flash\", \"reasoningEffort\": \"none\", \"autoCompactTokenLimit\": 550000}, " +
	"{\"model\": \"gpt-6.1-sol\", \"reasoningEffort\": \"xhigh\"}]}}}\n"

// legacyLimited is the documented single declaration carrying the limit.
const legacyLimited = "{\"roles\": {\"child\": {\"model\": \"inferhub/deepseek-v4.1-flash\", \"reasoningEffort\": \"none\", \"autoCompactTokenLimit\": 550000}}}\n"

// setRolePairs rewrites a role's whole pairs list, so a pair it retains must keep every property
// that pair declared. The auto-compaction limit belongs to the pair, and a change that rebuilt the
// retained pair without it would publish a policy whose limit is silently gone: the operator would
// see a valid document and the provider-limit failure this feature exists to prevent would return.
func TestSetRolePairsKeepsTheLimitOfAPairItRetains(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{
		{Model: "inferhub/deepseek-v4.1-flash", Effort: "none"},
		{Model: "gpt-6.1-sol", Effort: "xhigh"},
	}}
	result := Check([]byte(twoPairsOneLimited), digestOf(twoPairsOneLimited), change)
	if !result.Valid {
		t.Fatalf("retaining a pair the document already declares was refused: %+v", result)
	}
	candidate, err := candidateOf(t, twoPairsOneLimited, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"autoCompactTokenLimit\": 550000") {
		t.Fatalf("the retained pair lost its auto-compaction limit: %s", candidate)
	}
}

// The editor also has to be able to move a legacy single declaration to the pairs form. A limit
// written on the legacy declaration belongs to the one pair it declares, so the candidate must
// carry it inside that pair rather than beside the list, which the parser refuses.
func TestSetRolePairsMovesALegacyLimitIntoItsPair(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{
		{Model: "inferhub/deepseek-v4.1-flash", Effort: "none"},
		{Model: "gpt-6.1-sol", Effort: "xhigh"},
	}}
	result := Check([]byte(legacyLimited), digestOf(legacyLimited), change)
	if !result.Valid {
		t.Fatalf("adding a second pair to a legacy declaration that carries the limit was refused: %+v", result)
	}
	candidate, err := candidateOf(t, legacyLimited, change)
	if err != nil {
		t.Fatal(err)
	}
	document, err := decode([]byte(candidate))
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := document.Get("roles").(pyjson.Object)
	entry, _ := roles.Get("child").(pyjson.Object)
	if _, present := entry.Lookup("autoCompactTokenLimit"); present {
		t.Fatalf("the limit was left beside the pairs list, where the parser refuses it: %s", candidate)
	}
	pairs, _ := entry.Get("pairs").([]any)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %v", pairs)
	}
	first, _ := pairs[0].(pyjson.Object)
	limit, present := first.Lookup("autoCompactTokenLimit")
	if !present || canonical(limit) != "550000" {
		t.Fatalf("the first pair does not carry the legacy limit: %s", candidate)
	}
}

// The read projection is what an operator sees before proposing a change, so a limit the file
// declares has to be visible on the pair it belongs to rather than silently dropped.
func TestReadingShowsTheLimitOnThePairThatDeclaresIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "execution-policy.json")
	if err := os.WriteFile(file, []byte(twoPairsOneLimited), 0o644); err != nil {
		t.Fatal(err)
	}
	reading := Read(Located{State: Registered, Path: file})
	role, ok := roleOf(reading, "child")
	if !ok {
		t.Fatalf("the child role is not projected: %+v", reading.Roles)
	}
	if len(role.Pairs) != 2 || role.Pairs[0].AutoCompactTokenLimit != 550000 {
		t.Fatalf("the projected pairs lost the limit: %+v", role.Pairs)
	}
	if role.Pairs[1].AutoCompactTokenLimit != 0 {
		t.Fatalf("a pair that declares no limit reports one: %+v", role.Pairs)
	}
}

// A supplied limit is judged where it is supplied. Zero and null are not "no limit": they are
// values that are not positive integers, and the same rule the policy file is held to applies to a
// request that states one. Collapsing them into the absent representation would let the check
// approve a document whose pair silently kept an older limit, or dropped the invalid one.
func TestSetRolePairsRefusesASuppliedLimitThatIsNotAPositiveInteger(t *testing.T) {
	for _, supplied := range []string{"0", "-1", "null", "1.5", "\"550000\""} {
		t.Run(supplied, func(t *testing.T) {
			raw := "{\"kind\":\"setRolePairs\",\"role\":\"child\",\"pairs\":[{\"model\":\"inferhub/deepseek-v4.1-flash\",\"reasoningEffort\":\"none\",\"autoCompactTokenLimit\":" + supplied + "}]}"
			var change Change
			if err := json.Unmarshal([]byte(raw), &change); err != nil {
				t.Fatalf("a supplied limit must be judged by Check, not fail the decode: %v", err)
			}
			result := Check([]byte(legacyLimited), digestOf(legacyLimited), change)
			if result.Valid {
				t.Fatalf("autoCompactTokenLimit %s was accepted: %+v", supplied, result)
			}
		})
	}
}
