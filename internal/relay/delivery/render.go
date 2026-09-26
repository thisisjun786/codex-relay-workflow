package delivery

import (
	"fmt"
	"strings"
)

// Message kinds (delivery.py).
const (
	Completion     = "completion_event"
	Revision       = "revision_request"
	MergeTurnGrant = "merge_turn_grant"
	manifestLines  = 10
	notRecorded    = "not recorded"
	noNote         = "no note recorded; ask the parent"
)

var headings = []string{"VIOLATED CRITERION", "WHAT CHANGED", "FIX SCOPE", "PRESERVE", "REVERIFY AND RETURN", "TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT", "DECISION BOUNDARY", "VERDICT"}

func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return fmt.Sprint(t)
	case float64:
		return pyFloat(t)
	}
	return pyReprValue(v)
}

// splitlines is str.splitlines.
func splitlines(text string) []string {
	var parts []string
	var cur strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			parts = append(parts, cur.String())
			cur.Reset()
			if r == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// inline is report.inline: a record's text kept on the one line it is spliced into.
func inline(v any) string {
	text := pyStr(v)
	parts := splitlines(text)
	if len(parts) <= 1 && (len(parts) == 0 || parts[0] == text) {
		return text
	}
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " / ")
}

func known(v any) string {
	if v == nil || v == "" {
		return notRecorded
	}
	return inline(v)
}

func unheaded(v any) string {
	text := pyStr(v)
	probe := strings.TrimSpace(text)
	probe = strings.TrimLeft(probe, "-*#>")
	probe = strings.Trim(strings.TrimSpace(probe), "*`_")
	probe = strings.ToUpper(probe)
	for _, name := range headings {
		if probe == name || strings.HasPrefix(probe, name+":") {
			return `"` + text + `"`
		}
	}
	return text
}

func notRecordedBecause(what string) string { return notRecorded + ": " + what + "; ask the parent" }

func findingsWord(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func series(words []string) string {
	if len(words) == 1 {
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

func correctionFindings(receipt Obj) []Obj {
	var out []Obj
	list, _ := get(receipt, "criteria")
	items, _ := list.([]any)
	for _, item := range items {
		if o, ok := item.(Obj); ok && truthy(func() any { v, _ := get(o, "id"); return v }()) {
			out = append(out, o)
		}
	}
	return out
}

type verdictCounts struct{ fix, owed, met, undecided int }

func countFindings(findings []Obj) verdictCounts {
	var c verdictCounts
	for _, f := range findings {
		switch v, _ := get(f, "verdict"); v {
		case "needs_changes":
			c.fix++
		case "unverified":
			c.owed++
		case "verified":
			c.met++
		default:
			c.undecided++
		}
	}
	return c
}

func violatedHeading(receipt Obj) string {
	findings := correctionFindings(receipt)
	if len(findings) == 0 {
		return "VIOLATED CRITERION: " + notRecordedBecause("the verdict named no criterion")
	}
	c := countFindings(findings)
	var parts []string
	for _, p := range []struct {
		n    int
		text string
	}{{c.fix, "marked needs_changes"}, {c.owed, "marked unverified"}, {c.met, "marked verified"}, {c.undecided, "without a disposition"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.text))
		}
	}
	return "VIOLATED CRITERION: of the " + findingsWord(len(findings), "recorded finding") + ", " + strings.Join(parts, ", ")
}

func whatChangedLines(receipt Obj) []string {
	g := func(k string) any { v, _ := get(receipt, k); return v }
	return []string{"",
		fmt.Sprintf("WHAT CHANGED: submission %s ruled %s and superseded, generation %s opened", known(g("supersedesEvent")), known(g("verdict")), known(g("executionGeneration"))),
		fmt.Sprintf("  superseded revision %s, ruled in turn %s", known(g("supersedesRevisionHash")), known(g("verdictTurnId")))}
}

func fixScopeLines(receipt Obj) []string {
	findings := correctionFindings(receipt)
	if len(findings) == 0 {
		return []string{"", "FIX SCOPE: " + notRecordedBecause("no criterion was named, so nothing bounds a change")}
	}
	c := countFindings(findings)
	unruled := "FIX SCOPE: no finding is marked needs_changes, so nothing is ruled violated"
	var heading string
	switch {
	case c.fix > 0:
		heading = fmt.Sprintf("FIX SCOPE: only the %s marked needs_changes, shown or not; %s findings are out of scope", findingsWord(c.fix, "finding"), series([]string{"verified", "unverified"}))
	case c.owed > 0:
		heading = unruled + "; change nothing but the evidence REVERIFY AND RETURN asks for"
	case c.undecided > 0:
		heading = unruled + "; change nothing until the parent settles the findings below"
	default:
		heading = "FIX SCOPE: " + notRecordedBecause("what to change, since every finding is marked verified")
	}
	lines := []string{"", heading}
	if c.undecided > 0 {
		pronoun := "them"
		if c.undecided == 1 {
			pronoun = "it"
		}
		lines = append(lines, fmt.Sprintf("  %s without a disposition: whether to change %s is not recorded; ask the parent first", findingsWord(c.undecided, "finding"), pronoun))
	}
	return append(lines, "  anything wider, anything that would discard preserved work, or anything needing authority you were not given comes back to the parent")
}

func reverifyLines(receipt Obj) []string {
	findings := correctionFindings(receipt)
	var clauses []string
	if len(findings) == 0 {
		clauses = []string{"what to re-check is " + notRecordedBecause("the verdict named no criterion")}
	} else {
		c := countFindings(findings)
		if c.fix > 0 {
			clauses = append(clauses, "re-check each finding FIX SCOPE names")
		}
		if c.owed > 0 {
			clauses = append(clauses, "show evidence for the "+findingsWord(c.owed, "finding")+" marked unverified")
		}
		if c.undecided > 0 {
			clauses = append(clauses, "settle with the parent whether to change the "+findingsWord(c.undecided, "finding")+" without a disposition")
		}
		if len(clauses) == 0 {
			clauses = append(clauses, "what to re-check is "+notRecordedBecause("every finding is marked verified"))
		}
	}
	return []string{"", "REVERIFY AND RETURN: " + strings.Join(append(clauses, "then hand back as below"), "; ")}
}

func returnLines(rid string, generation any) []string {
	shown := "<not recorded; ask the parent>"
	if generation != nil && generation != "" {
		shown = inline(generation)
	}
	return []string{"",
		"There is nothing to acknowledge. Contract v1 defines no acknowledgement for this",
		"direction and the relay refuses one by kind, so there is no proof to compute and",
		"no acknowledgement to send.",
		"Answer with your next completion receipt under the new generation:",
		fmt.Sprintf("  emit --relationship %s --generation %s --attempt <n>", rid, shown),
		"       --outcome ready_for_review --turn-thread <your task id> --turn-id <your turn>",
		"       --artifact <path> [--continues-anchor <this generation dispatch turn>]"}
}

func restorationLabel(f Obj) string {
	if v, _ := get(f, "restoration"); truthy(v) {
		return " [restoration block]"
	}
	return ""
}

func overflowLine(items []any, eventID string, nameBlock bool) string {
	if len(items) <= manifestLines {
		return ""
	}
	hidden := items[manifestLines:]
	detail := ""
	if nameBlock {
		for _, h := range hidden {
			if o, ok := h.(Obj); ok {
				if v, _ := get(o, "restoration"); truthy(v) {
					id, _ := get(o, "id")
					detail = ", including the restoration block on " + pyStr(id)
					break
				}
			}
		}
	}
	return fmt.Sprintf("  ... %d more%s; see 'codex-session-relay show --event %s'", len(hidden), detail, eventID)
}

func renderCompletion(row Row, record Obj, request string) string {
	g := func(k string) any { v, _ := get(record, k); return v }
	event := row.S("event_id")
	lines := []string{
		"[codex-session-relay] verification request",
		"requestId: " + request,
		"eventId: " + event,
		"relationshipId: " + row.S("relationship_id"),
		"executionGeneration: " + pyStr(g("executionGeneration")),
		"attempt: " + pyStr(g("attempt")),
		"outcome: " + pyStr(g("outcome")),
		"revisionHash: " + pyStr(g("revisionHash")),
	}
	if manifest, _ := g("manifest").([]any); len(manifest) > 0 {
		lines = append(lines, fmt.Sprintf("deliverables: %d", len(manifest)))
		for _, entry := range manifest[:min(len(manifest), manifestLines)] {
			o, _ := entry.(Obj)
			line := "  " + pyStr(func() any { v, _ := get(o, "path"); return v }()) + "  sha256=" + pyStr(func() any { v, _ := get(o, "sha256"); return v }())
			if size, ok := get(o, "bytes"); ok && size != nil {
				line += "  bytes=" + pyStr(size)
			}
			lines = append(lines, line)
		}
		if overflow := overflowLine(manifest, event, false); overflow != "" {
			lines = append(lines, overflow)
		}
	} else {
		lines = append(lines, "deliverables: none (execution-only outcome)")
	}
	if ref := g("manifestRef"); truthy(ref) {
		lines = append(lines, "manifestRef: "+pyStr(ref))
	}
	if criteria, _ := g("criteria").([]any); len(criteria) > 0 {
		lines = append(lines, "criteria claimed by the child:")
		for _, item := range criteria[:min(len(criteria), manifestLines)] {
			o, _ := item.(Obj)
			id, _ := get(o, "id")
			verdict, _ := get(o, "verdict")
			lines = append(lines, "  "+pyStr(id)+": "+pyStr(verdict))
		}
		if overflow := overflowLine(criteria, event, false); overflow != "" {
			lines = append(lines, overflow)
		}
	}
	lines = append(lines,
		"",
		"To respond, from inside your own turn:",
		"  claim     --event "+event+" --turn <your turn id>",
		"  ack-proof --event "+event+" --turn <your turn id>",
		"  ack       --event "+event+" --ack-turn <your turn id> --ack-proof <proof>",
		"  verdict   --event "+event+" --verdict <verified|needs_changes|unverified|aborted> --verdict-turn <your turn id>",
		"",
		"The proof is sha256(eventId|<your own turn id>). This message does not and cannot",
		"contain that turn id, which is what distinguishes acknowledging from echoing.",
		"Full record: codex-session-relay show --event "+event,
	)
	return strings.Join(lines, "\n")
}

func renderRevision(row Row, record Obj, request string) string {
	g := func(k string) any { v, _ := get(record, k); return v }
	event := row.S("event_id")
	lines := []string{
		"[codex-session-relay] revision request",
		"requestId: " + request,
		"eventId: " + event,
		"relationshipId: " + row.S("relationship_id"),
		"executionGeneration: " + known(g("executionGeneration")) + "  (new)",
		"supersedesEvent: " + known(g("supersedesEvent")),
		"supersedesRevisionHash: " + known(g("supersedesRevisionHash")),
		"verdict: " + known(g("verdict")),
	}
	findings, _ := g("criteria").([]any)
	lines = append(lines, "", violatedHeading(record))
	for _, item := range findings[:min(len(findings), manifestLines)] {
		o, _ := item.(Obj)
		id, _ := get(o, "id")
		if !truthy(id) {
			id = "(no id recorded)"
		}
		verdict, _ := get(o, "verdict")
		if !truthy(verdict) {
			verdict = "no disposition recorded"
		}
		note, _ := get(o, "note")
		tail := " — " + noNote
		if truthy(note) {
			tail = " — " + inline(note)
		}
		lines = append(lines, "  "+unheaded(inline(id))+restorationLabel(o)+": "+inline(verdict)+tail)
	}
	if overflow := overflowLine(findings, event, true); overflow != "" {
		lines = append(lines, inline(overflow))
	}
	lines = append(lines, whatChangedLines(record)...)
	lines = append(lines, fixScopeLines(record)...)
	lines = append(lines, "", "PRESERVE: everything FIX SCOPE does not name, verified findings included,", "  work this request does not mention and any other task in-flight beside it")
	lines = append(lines, reverifyLines(record)...)
	lines = append(lines, returnLines(row.S("relationship_id"), g("executionGeneration"))...)
	lines = append(lines, "", "Full record: codex-session-relay show --event "+event)
	return strings.Join(lines, "\n")
}
