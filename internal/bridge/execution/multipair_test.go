package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// CRW-454: a role may declare several model/effort pairs (the child runs on Sonnet or on SOL).
// The host's file is parsed here and the relay's role gate reads the result, so these tests pin
// what a two-pair role means and what an existing one-pair file keeps meaning.

const (
	childSonnet = "anthropic/claude-sonnet-5-5"
	childSol    = "gpt-6.1-sol"
)

func pairDoc(model, effort string) doc { return doc{"model": model, "reasoningEffort": effort} }

func pairList(pairs ...doc) []any {
	out := []any{}
	for _, p := range pairs {
		out = append(out, p)
	}
	return out
}

// bothPairs is the two pairs of the child role in the order the file declares them.
func bothPairs() []any {
	return []any{
		map[string]any{"model": childSonnet, "reasoningEffort": "xhigh"},
		map[string]any{"model": childSol, "reasoningEffort": "xhigh"},
	}
}

// twoPairChild is the host's policy once the child may run on either pair; the parent keeps its one.
func twoPairChild(withAllowed bool) doc {
	policy := doc{"roles": doc{
		"supervisor": doc{"expectation": "record"},
		"parent":     doc{"model": parentModel, "reasoningEffort": parentEffort},
		"child":      doc{"expectation": "pair", "pairs": pairList(pairDoc(childSonnet, "xhigh"), pairDoc(childSol, "xhigh"))},
	}}
	if withAllowed {
		policy["allowed"] = []any{
			doc{"model": parentModel, "efforts": []any{parentEffort}},
			doc{"model": childSonnet, "efforts": []any{"xhigh"}},
			doc{"model": childSol, "efforts": []any{"xhigh"}},
		}
	}
	return policy
}

func TestEveryPairOfARoleIsAuthorizedAsTheRolePair(t *testing.T) {
	for _, withAllowed := range []bool{true, false} {
		p := mustLoad(t, twoPairChild(withAllowed))
		for _, pair := range [][2]string{{childSonnet, "xhigh"}, {childSol, "xhigh"}} {
			got := authorize(t, p, Input{Model: pair[0], Effort: pair[1], Role: "child"})
			if got.Provenance != "role_pair" || got.Model != pair[0] || got.Effort != pair[1] {
				t.Fatalf("allowed=%v %v: %+v", withAllowed, pair, got)
			}
			want := map[string]any{"role": "child", "expectation": "pair", "model": pair[0], "reasoningEffort": pair[1], "pairs": bothPairs(), "overriddenBy": nil}
			if !reflect.DeepEqual(got.Receipt["roleExpectation"], want) {
				t.Fatalf("allowed=%v %v: receipt %#v", withAllowed, pair, got.Receipt["roleExpectation"])
			}
		}
	}
}

func TestAnotherPairIsRefusedWithEveryPairOfTheRoleListed(t *testing.T) {
	p := mustLoad(t, twoPairChild(true))
	for _, request := range [][2]string{{childSonnet, "high"}, {childSol, "high"}, {parentModel, parentEffort}, {unapproved, "xhigh"}} {
		r := refused(t, second(p.Authorize(Input{Model: request[0], Effort: request[1], Role: "child"})))
		if r.Code != RoleMismatch || r.Field != "pair" {
			t.Fatalf("%v: %+v", request, r)
		}
		if !reflect.DeepEqual(r.Allowed, bothPairs()) {
			t.Fatalf("%v: allowed %#v", request, r.Allowed)
		}
		if !reflect.DeepEqual(r.Requested, map[string]any{"model": request[0], "reasoningEffort": request[1]}) {
			t.Fatalf("%v: requested %#v", request, r.Requested)
		}
		for _, name := range []string{childSonnet, childSol} {
			if !strings.Contains(r.Detail, name) {
				t.Fatalf("%v: detail does not name %s: %s", request, name, r.Detail)
			}
		}
	}
}

// Two pairs of one role may use different efforts, and a name belongs to the model beside it.
func TestAnEffortNameBelongsToItsModelWithinOneRole(t *testing.T) {
	p := mustLoad(t, doc{
		"allowed": []any{doc{"model": childSonnet, "efforts": []any{"xhigh"}}, doc{"model": childSol, "efforts": []any{"high"}}},
		"roles":   doc{"child": doc{"pairs": pairList(pairDoc(childSonnet, "xhigh"), pairDoc(childSol, "high"))}},
	})
	authorize(t, p, Input{Model: childSonnet, Effort: "xhigh", Role: "child"})
	authorize(t, p, Input{Model: childSol, Effort: "high", Role: "child"})
	for _, crossed := range [][2]string{{childSonnet, "high"}, {childSol, "xhigh"}} {
		if r := refused(t, second(p.Authorize(Input{Model: crossed[0], Effort: crossed[1], Role: "child"}))); r.Code != RoleMismatch {
			t.Fatalf("%v: %+v", crossed, r)
		}
	}
}

func TestAMultiPairChildLeavesTheOtherRolesAsTheyWere(t *testing.T) {
	here := canonicalTemp(t)
	document := twoPairChild(true)
	document["exceptions"] = doc{"one-task": doc{"model": unapproved, "reasoningEffort": "high", "cwd": []any{here}, "role": "child"}}
	p := mustLoad(t, document)

	// The child's second pair is not the parent's pair, and the parent refuses as it always did.
	r := refused(t, second(p.Authorize(Input{Model: childSol, Effort: "xhigh", Role: "parent"})))
	if r.Code != RoleMismatch || r.Field != "model" || !sameStrings(r.Allowed, parentModel) {
		t.Fatalf("%+v", r)
	}
	parent := authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "parent"})
	if want := map[string]any{"role": "parent", "expectation": "pair", "model": parentModel, "reasoningEffort": parentEffort, "overriddenBy": nil}; !reflect.DeepEqual(parent.Receipt["roleExpectation"], want) {
		t.Fatalf("parent receipt %#v", parent.Receipt["roleExpectation"])
	}
	supervisor := authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "supervisor"})
	if want := map[string]any{"role": "supervisor", "expectation": "record", "model": nil, "reasoningEffort": nil, "overriddenBy": nil}; !reflect.DeepEqual(supervisor.Receipt["roleExpectation"], want) {
		t.Fatalf("supervisor receipt %#v", supervisor.Receipt["roleExpectation"])
	}

	// An exception answers instead of the role: the receipt names no matched pair, still lists the role's pairs.
	excepted := authorize(t, p, Input{Model: unapproved, Effort: "high", CWD: here, Exception: "one-task", Role: "child"})
	want := map[string]any{"role": "child", "expectation": "pair", "model": nil, "reasoningEffort": nil, "pairs": bothPairs(), "overriddenBy": "one-task"}
	if excepted.Provenance != "exception" || !reflect.DeepEqual(excepted.Receipt["roleExpectation"], want) {
		t.Fatalf("%+v", excepted)
	}
}

func TestAMultiPairRoleIsDescribedByItsList(t *testing.T) {
	roles := mustLoad(t, twoPairChild(true)).Summary()["roles"].(map[string]any)
	if want := map[string]any{"role": "child", "expectation": "pair", "model": nil, "reasoningEffort": nil, "pairs": bothPairs()}; !reflect.DeepEqual(roles["child"], want) {
		t.Fatalf("child %#v", roles["child"])
	}
	if want := map[string]any{"role": "parent", "expectation": "pair", "model": parentModel, "reasoningEffort": parentEffort}; !reflect.DeepEqual(roles["parent"], want) {
		t.Fatalf("parent %#v", roles["parent"])
	}
}

// A list of one pair is the one-pair form written another way: the same description and the
// same answer to every request (the digests differ because the bytes do).
func TestAListOfOnePairIsTheLegacySingleForm(t *testing.T) {
	legacy := mustLoad(t, rolesPolicy(nil, true))
	listed := mustLoad(t, rolesPolicy(doc{
		"supervisor": doc{"expectation": "record"},
		"parent":     doc{"pairs": pairList(pairDoc(parentModel, parentEffort))},
		"child":      doc{"pairs": pairList(pairDoc(model, effort))},
	}, true))
	if !reflect.DeepEqual(legacy.Summary()["roles"], listed.Summary()["roles"]) {
		t.Fatalf("legacy %#v listed %#v", legacy.Summary()["roles"], listed.Summary()["roles"])
	}
	for _, role := range []string{"", "supervisor", "parent", "child"} {
		for _, pair := range [][2]string{{model, effort}, {parentModel, parentEffort}, {supersededParent[0], supersededParent[1]}, {model, "high"}, {unapproved, "high"}} {
			in := Input{Model: pair[0], Effort: pair[1], Role: role}
			want, wantErr := legacy.Authorize(in)
			got, gotErr := listed.Authorize(in)
			if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) {
				t.Fatalf("%s %v: legacy %v listed %v", role, pair, wantErr, gotErr)
			}
			if wantErr != nil {
				continue
			}
			want.Receipt["digest"], got.Receipt["digest"] = nil, nil
			if want.Provenance != got.Provenance || !reflect.DeepEqual(want.Receipt, got.Receipt) {
				t.Fatalf("%s %v: legacy %+v listed %+v", role, pair, want, got)
			}
		}
	}
}

func TestAPolicyThatListsPairsBadlyDoesNotLoad(t *testing.T) {
	child := func(entry doc) doc {
		return rolesPolicy(doc{"child": entry}, true)
	}
	allowedLacksSol := rolesPolicy(doc{"child": doc{"pairs": pairList(pairDoc(model, effort), pairDoc(childSol, "max"))}}, true)
	rows := []struct {
		name     string
		document doc
		want     string
	}{
		{"empty list", child(doc{"pairs": []any{}}), "non-empty list of"},
		{"not a list", child(doc{"pairs": "x"}), "non-empty list of"},
		{"duplicate pair", child(doc{"pairs": pairList(pairDoc(model, effort), pairDoc(model, effort))}), "twice"},
		{"pair the allowed list does not approve", allowedLacksSol, "allowed list does not approve"},
		{"both forms", child(doc{"model": model, "reasoningEffort": effort, "pairs": pairList(pairDoc(model, effort))}), "both pairs and"},
		{"pair that is not an object", child(doc{"pairs": []any{"x"}}), "a pair of role"},
		{"pair without an effort", child(doc{"pairs": pairList(doc{"model": model})}), "is missing"},
		{"pair with another key", child(doc{"pairs": pairList(doc{"model": model, "reasoningEffort": effort, "note": "x"})}), "a pair of role"},
		{"pair with a model that is not text", child(doc{"pairs": pairList(doc{"model": 7, "reasoningEffort": effort})}), "a pair of role"},
		{"pair with a blank effort", child(doc{"pairs": pairList(doc{"model": model, "reasoningEffort": "  "})}), "a pair of role"},
		{"supervisor with pairs", rolesPolicy(doc{"supervisor": doc{"pairs": pairList(pairDoc(model, effort))}}, true), "cannot declare"},
		{"record child with pairs", child(doc{"expectation": "record", "pairs": pairList(pairDoc(model, effort))}), "expectation must be 'pair'"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := load(t, row.document)
			if err == nil {
				t.Fatal("the policy loaded")
			}
			if got := policyError(t, err).Error(); !strings.Contains(got, row.want) {
				t.Fatalf("want %q in %q", row.want, got)
			}
		})
	}
}

// An existing policy file is read as it always was: the digest is the SHA-256 of its bytes, the
// description of each role is the one the host has always reported, and the answers do not move.
func TestAnExistingPolicyFileKeepsItsDigestSummaryAndAnswers(t *testing.T) {
	raw := strings.ReplaceAll("{'allowed':[{'model':'anthropic/claude-opus-5-5','efforts':['xhigh']},{'model':'anthropic/claude-sonnet-5-5','efforts':['xhigh']}],'roles':{'supervisor':{'expectation':'record'},'parent':{'model':'anthropic/claude-opus-5-5','reasoningEffort':'xhigh'},'child':{'model':'anthropic/claude-sonnet-5-5','reasoningEffort':'xhigh'}}}", "'", "\"")
	sum := sha256.Sum256([]byte(raw))
	const pinned = "22c9df7f99ad7893860e0eccf431a177f55165f90aa8e8ffeaeaf63e4d43df4f"
	if hex.EncodeToString(sum[:]) != pinned {
		t.Fatal("the fixture bytes changed")
	}
	p, err := FromBytes([]byte(raw), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	summary := p.Summary()
	if summary["digest"] != pinned || summary["mode"] != "allowlist" {
		t.Fatalf("%v", summary)
	}
	want := map[string]any{
		"supervisor": map[string]any{"role": "supervisor", "expectation": "record", "model": nil, "reasoningEffort": nil},
		"parent":     map[string]any{"role": "parent", "expectation": "pair", "model": parentModel, "reasoningEffort": parentEffort},
		"child":      map[string]any{"role": "child", "expectation": "pair", "model": childSonnet, "reasoningEffort": "xhigh"},
	}
	if !reflect.DeepEqual(summary["roles"], want) {
		t.Fatalf("roles %#v", summary["roles"])
	}
	if !reflect.DeepEqual(p.RoleOrder(), []string{"supervisor", "parent", "child"}) {
		t.Fatal(p.RoleOrder())
	}
	got := authorize(t, p, Input{Model: childSonnet, Effort: "xhigh", Role: "child"})
	if wantReceipt := map[string]any{"role": "child", "expectation": "pair", "model": childSonnet, "reasoningEffort": "xhigh", "overriddenBy": nil}; got.Provenance != "role_pair" || !reflect.DeepEqual(got.Receipt["roleExpectation"], wantReceipt) {
		t.Fatalf("%+v", got)
	}
	r := refused(t, second(p.Authorize(Input{Model: parentModel, Effort: "xhigh", Role: "child"})))
	if r.Code != RoleMismatch || r.Field != "model" || !sameStrings(r.Allowed, childSonnet) || r.Detail != "role \"child\" runs model \""+childSonnet+"\" on this host" {
		t.Fatalf("%+v", r)
	}
	r = refused(t, second(p.Authorize(Input{Model: childSonnet, Effort: "high", Role: "child"})))
	if r.Code != RoleMismatch || r.Field != "reasoning_effort" || !sameStrings(r.Allowed, "xhigh") {
		t.Fatalf("%+v", r)
	}
}
