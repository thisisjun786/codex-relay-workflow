package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// Subset ported for todo 24; todo 22 owns and extends omission observation.
func observationSelectors(reading map[string]any) (map[string]any, bool) {
	if reading == nil {
		return nil, false
	}
	selectors, ok := reading["selectors"].(map[string]any)
	if !ok {
		return nil, false
	}
	for _, key := range []string{"state", "markerRoot", "workspace", "assignment", "session", "turn"} {
		if strings.TrimSpace(pyvalue.Str(evidence.Or(selectors[key], ""))) == "" {
			return nil, false
		}
	}
	return selectors, true
}
func ObservationObligation(reading map[string]any) *Obligation {
	if reading == nil || reading["schema"] != "reporting-observation/1" || reading["reportingState"] != "unreported" || reading["owed"] == false {
		return nil
	}
	relation, ok := reading["relationshipId"].(string)
	if !ok || strings.TrimSpace(relation) == "" {
		return nil
	}
	selectors, ok := reading["selectors"].(map[string]any)
	if !ok {
		return nil
	}
	turn, ok := selectors["turn"].(string)
	if !ok || strings.TrimSpace(turn) == "" {
		return nil
	}
	generation := reading["executionGeneration"]
	basis := map[string]any{"table": nil, "schema": "reporting-observation/1", "reason": reading["reason"], "turn": turn}
	return &Obligation{Schema: "supervisor-obligation/1", ID: hash32("unreported|" + relation + "|" + turn), Kind: "unreported", RelationID: relation, Subject: turn, Generation: generation, Basis: basis, Detail: "an admitted turn settled without a report, so what it owed is still owed"}
}
func validateObservation(o Obligation, reading map[string]any, directory string) error {
	mismatch := ""
	quoted := pyvalue.Quote
	switch {
	case reading["schema"] != "reporting-observation/1":
		mismatch = "its schema is " + quoted(reading["schema"]) + ", not \"reporting-observation/1\""
	case reading["reportingState"] != "unreported":
		mismatch = "it reports " + quoted(reading["reportingState"]) + ", not \"unreported\""
	case reading["relationshipId"] != o.RelationID:
		mismatch = "it names relationship " + quoted(reading["relationshipId"]) + " and the obligation is for " + quoted(o.RelationID)
	default:
		selectors, _ := observationSelectors(reading)
		if selectors["turn"] != o.Subject {
			mismatch = "its selectors name turn " + quoted(selectors["turn"]) + " and the obligation is about turn " + quoted(o.Subject)
		} else {
			derived := ObservationObligation(reading)
			if derived == nil {
				mismatch = "it raises no obligation at all"
			} else {
				a, _ := json.Marshal(o)
				b, _ := json.Marshal(derived)
				if string(a) != string(b) {
					mismatch = "the obligation's executionGeneration differ from what it raises"
				}
			}
		}
	}
	if mismatch != "" {
		return Refusal{"contradictory_observation", "the reading staged with this omission contradicts it: " + mismatch + ". Its selectors would become the evidence pointer of a report about something else, so nothing was composed or recorded"}
	}
	selectors, _ := observationSelectors(reading)
	if filepath.Clean(fmt.Sprint(selectors["state"])) != filepath.Clean(directory) {
		return Refusal{"contradictory_observation", "the reading was taken against the store in " + quoted(selectors["state"]) + " and this report is staged in " + quoted(directory) + ". Its recheck would read another store than its packet points at, so nothing was composed or recorded; stage it from a reading taken against this store"}
	}
	return nil
}
func observationRecheck(program string, reading map[string]any) any {
	selectors, ok := observationSelectors(reading)
	if !ok {
		return nil
	}
	if reading["source"] == "relay_store" {
		return programCommand(program, "--state", pyvalue.Str(selectors["state"]), "reporting-derive", "--relationship", pyvalue.Str(reading["relationshipId"]), "--turn", pyvalue.Str(selectors["turn"]))
	}
	return programCommand(program, "--state", pyvalue.Str(selectors["state"]), "reporting-show", "--marker-root", pyvalue.Str(selectors["markerRoot"]), "--workspace", pyvalue.Str(selectors["workspace"]), "--assignment", pyvalue.Str(selectors["assignment"]), "--session", pyvalue.Str(selectors["session"]), "--turn", pyvalue.Str(selectors["turn"]))
}

func (c *Channel) composeReading(ctx context.Context, o Obligation, r Resolution, at string, reading map[string]any) (Packet, error) {
	p, err := c.Compose(ctx, o, r, at)
	if err != nil {
		return nil, err
	}
	if reading == nil {
		return p, nil
	}
	line := c.command("supervisor-show", "--message", p.ID())
	p["evidence"] = []string{line}
	p["envelope"].(map[string]any)["evidence"] = []string{line}
	return p, nil
}
