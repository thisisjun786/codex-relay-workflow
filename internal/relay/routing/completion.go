package routing

import (
	"crypto/sha256"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"slices"
	"strings"
)

// ReadReading validates completion.read_reading. Missing is unobservable, not absent.
func ReadReading(value any) (Object, error) {
	v := &validator{}
	r := v.object(value, "completion-reading/1", strings.Fields("schema product subject claims requires observed evidence exceptions origin observedAt"), "a completion reading")
	claimNames := []string{"linearDone", "prMerged", "sessionEnded"}
	claims := v.closed(absent(r["claims"], Object{}), claimNames, "claims")
	requires := v.closed(absent(r["requires"], Object{}), Checks, "requires")
	observed := v.closed(absent(r["observed"], Object{}), Checks, "observed")
	ev := v.closed(absent(r["evidence"], Object{}), Checks, "evidence")
	exceptions := absent(r["exceptions"], []any{})
	if _, ok := exceptions.([]any); !ok {
		v.fail("exceptions is a list")
	}
	reading := Object{"schema": "completion-reading/1", "product": v.product(r["product"], "product", false), "subject": v.issue(r["subject"], "subject", false)}
	c := Object{}
	for _, claim := range claimNames {
		c[claim] = v.flag(claims[claim], "claims."+claim, false)
	}
	reading["claims"] = c
	reading["origin"] = v.choice(absent(r["origin"], "observed"), []string{"observed", "simulated"}, "origin", false)
	reading["observedAt"] = v.text(r["observedAt"], "observedAt", true, 64)
	req, obs, evid := Object{}, Object{}, Object{}
	for _, check := range Checks {
		required := absent(requires[check], "unknown")
		if _, ok := required.(bool); !ok && required != "unknown" {
			v.fail("requires." + check + " is true, false or 'unknown'")
		}
		req[check] = required
		vocabulary := []string{"present", "absent", "unobservable"}
		if check == "acceptance" {
			vocabulary = []string{"passed", "failed", "absent", "unobservable"}
		}
		obs[check] = v.choice(absent(observed[check], "unobservable"), vocabulary, "observed."+check, false)
		entry := v.closed(absent(ev[check], Object{}), []string{"fix", "verification"}, "evidence."+check)
		refs := Object{}
		for _, key := range []string{"fix", "verification"} {
			if entry[key] != nil {
				name := "evidence." + check + "." + key
				ref := v.closed(entry[key], []string{"ref", "source"}, name)
				refs[key] = Object{"ref": v.text(ref["ref"], name+".ref", false, 600), "source": v.text(ref["source"], name+".source", false, 600)}
			}
		}
		evid[check] = refs
	}
	exc := []any{}
	for i, x := range list(exceptions) {
		name := fmt.Sprintf("exceptions[%d]", i)
		e := v.closed(x, strings.Fields("check kind ref approvedBy"), name)
		exc = append(exc, Object{"check": v.choice(e["check"], Checks, name+".check", false), "kind": v.choice(e["kind"], []string{"scope_reduction", "follow_up"}, name+".kind", false), "ref": v.text(e["ref"], name+".ref", false, 600), "approvedBy": v.text(e["approvedBy"], name+".approvedBy", true, 600)})
	}
	reading["requires"], reading["observed"], reading["evidence"], reading["exceptions"] = req, obs, evid, exc
	return reading, v.err
}
func completionException(reading Object, check string, bindings Object) (string, string) {
	var chosen Object
	for _, x := range list(reading["exceptions"]) {
		e := pyjson.Map(x)
		if e["check"] == check {
			chosen = e
		}
	}
	if chosen == nil {
		return "", ""
	}
	if chosen["kind"] == "scope_reduction" {
		if chosen["approvedBy"] != nil {
			return "excepted", fmt.Sprintf("scope reduction approved by %s (%s)", chosen["approvedBy"], chosen["ref"])
		}
		return "exception_unverified", "a scope reduction without a named approval"
	}
	ref, subject := chosen["ref"], reading["subject"]
	if ref == subject {
		return "exception_unverified", "a subject cannot be its own follow-up"
	}
	b := pyjson.Map(bindings[pyjson.Text(ref)])
	if b == nil || b["kind"] != "issue" {
		return "exception_unverified", fmt.Sprintf("%s is not a bound issue of %s", ref, reading["product"])
	}
	if !openState(b["state"]) {
		return "exception_unverified", fmt.Sprintf("%s is %s, so it carries nothing forward", ref, b["state"])
	}
	for _, x := range list(b["followUpOf"]) {
		e := pyjson.Map(x)
		if e["issue"] == subject && contains(e["checks"], check) {
			return "excepted", fmt.Sprintf("%s took over %s of %s", ref, check, subject)
		}
	}
	return "exception_unverified", fmt.Sprintf("%s does not record taking over %s of %s", ref, check, subject)
}
func completionObserved(reading Object, check string, context Object) (string, string) {
	required := pyjson.Map(reading["requires"])[check]
	if required == false {
		if contains(context["openMismatches"], check) {
			return "requirement_changed_without_approval", "the requirement was dropped after a mismatch with no approved exception, so the mismatch stands"
		}
		return "consistent", "not required"
	}
	if required == "unknown" {
		return "unverified", "whether this is required is unknown"
	}
	observed := pyjson.Text(pyjson.Map(reading["observed"])[check])
	if observed == "unobservable" {
		return "unverified", "the result could not be observed"
	}
	if observed == "failed" || observed == "absent" {
		return "mismatch", "required and " + observed
	}
	return "consistent", "required and " + observed
}
func completionCheck(reading Object, check string, context Object) (string, string) {
	claim := map[string]string{"acceptance": "linearDone", "install": "prMerged", "realUse": "prMerged", "handoff": "sessionEnded"}[check]
	var verdict, reason string
	if pyjson.Map(reading["claims"])[claim] == true {
		verdict, reason = completionObserved(reading, check, context)
	} else if contains(context["openMismatches"], check) {
		verdict = "claim_withdrawn_without_closure"
		reason = claim + " is no longer claimed, and nothing closed the open mismatch, so it stands"
	} else {
		return "not_applicable", "nothing claims " + claim
	}
	if !slices.Contains([]string{"mismatch", "requirement_changed_without_approval", "claim_withdrawn_without_closure", "unverified"}, verdict) {
		return verdict, reason
	}
	ex, why := completionException(reading, check, pyjson.Map(context["bindings"]))
	if ex == "" {
		return verdict, reason
	}
	if ex == "exception_unverified" && (verdict == "unverified" || verdict == "claim_withdrawn_without_closure") {
		return verdict, reason + "; the claimed exception is unverified: " + why
	}
	return ex, why
}

// EvaluateCompletion compares a validated reading with explicit context. It never writes
// a tracker state, assumes installation is required, or promotes a child goal to completion.
func EvaluateCompletion(reading, context Object) Object {
	checks, recurrences := []any{}, []any{}
	mismatch, unverified, pending := false, false, false
	consider := func(verdict string) {
		switch verdict {
		case "mismatch", "exception_unverified", "requirement_changed_without_approval", "claim_withdrawn_without_closure":
			mismatch = true
		case "unverified":
			unverified = true
		}
	}
	for _, check := range Checks {
		verdict, reason := completionCheck(reading, check, context)
		entry := Object{"check": check, "verdict": verdict, "reason": reason}
		ev := pyjson.Map(pyjson.Map(reading["evidence"])[check])
		if verdict == "consistent" && contains(context["openMismatches"], check) {
			missing := []any{}
			for _, key := range []string{"fix", "verification"} {
				if _, ok := ev[key]; !ok {
					missing = append(missing, key)
				}
			}
			closure := Object{"ready": len(missing) == 0, "missing": missing}
			for k, v := range ev {
				closure[k] = v
			}
			entry["closure"] = closure
			if len(missing) > 0 {
				pending = true
			}
		}
		if verdict == "excepted" && contains(context["openMismatches"], check) {
			entry["closure"] = Object{"ready": true, "missing": []any{}, "exception": reason}
		}
		consider(verdict)
		checks = append(checks, entry)
	}
	for _, x := range list(context["recurrences"]) {
		item := pyjson.Map(x)
		recurrences = append(recurrences, Object{"check": "recurrence", "verdict": "mismatch", "faultId": item["faultId"], "reason": item["reason"]})
		mismatch = true
	}
	if reason := pyjson.Text(context["recurrenceUnknown"]); reason != "" {
		recurrences = append(recurrences, Object{"check": "recurrence", "verdict": "unverified", "faultId": nil, "reason": reason})
		unverified = true
	}
	overall := "consistent"
	switch {
	case mismatch:
		overall = "mismatch"
	case unverified:
		overall = "unverified"
	case pending:
		overall = "closure_pending"
	}
	return Object{"subject": reading["subject"], "product": reading["product"], "origin": reading["origin"], "verdict": overall, "checks": checks, "recurrences": recurrences}
}

func ReadingKey(reading Object) (string, error) {
	s, err := Canonical(reading)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("reading:%x", sum)[:32], nil
}
