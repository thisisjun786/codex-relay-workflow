// Package delivery holds the parts of delivery.py that more than one relay command reads.
// It starts with preview_message, which `show --event E --message` needs (todo 20) and the
// sender will reuse (todo 21). Self-contained: it reads the store through store.Store and
// depends on nothing in internal/relay/cli.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Delivery kinds (delivery.py COMPLETION, REVISION, MERGE_TURN_GRANT).
const (
	Completion     = "completion_event"
	Revision       = "revision_request"
	MergeTurnGrant = "merge_turn_grant"
)

// ManifestLines is MANIFEST_LINES: how many deliverables or findings a plain message lists.
const ManifestLines = 10

// ErrRendererNotPorted marks a preview whose bytes come from report.py's composed work-report
// renderer and required_for_candidate (todo 24) or from the merge-turn grant notice for grants
// mergeturn.py queues (todo 26); a preview is refused rather than rendered differently.
var ErrRendererNotPorted = errors.New("this preview needs the composed work-report renderer (todo 24) or the merge-turn grant notice (todo 26), which are not ported yet")

// ErrNoDelivery is DeliveryService.get's refusal for an event with no delivery row.
var ErrNoDelivery = errors.New("no delivery queued")

// Preview is DeliveryService.preview_message: what the NEXT attempt would say, never evidence of
// what any attempt did say. Reads use ctx, so a caller inside a transaction sees its writes.
func Preview(ctx context.Context, s *store.Store, event string) (string, error) {
	row, err := s.One(ctx, "SELECT * FROM deliveries WHERE event_id = ?", event)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", fmt.Errorf("%w for event %s", ErrNoDelivery, pyRepr(event))
	}
	receipt, err := s.One(ctx, "SELECT * FROM events WHERE event_id = ?", event)
	if err != nil {
		return "", err
	}
	record := object{}
	if receipt != nil {
		if decoded, ok := decode(text(receipt.Get("receipt"))).(object); ok {
			record = decoded
		}
	}
	count, _ := receipt2int(row.Get("attempt_count"))
	request, err := store.RequestID(event, int(count)+1)
	if err != nil {
		return "", fmt.Errorf("request id: %w", err)
	}
	report, err := s.One(ctx, "SELECT 1 FROM work_reports WHERE event_id = ? LIMIT 1", event)
	if err != nil {
		return "", err
	}
	kind := text(row.Get("kind"))
	if report != nil || kind == MergeTurnGrant {
		return "", ErrRendererNotPorted
	}
	if kind == Revision {
		return renderRevision(event, text(row.Get("relationship_id")), record, request), nil
	}
	return renderCompletion(event, text(row.Get("relationship_id")), record, request), nil
}

func receipt2int(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}

func text(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	}
	return ""
}

// renderCompletion is DeliveryService._render_completion with no work report.
func renderCompletion(event, relationship string, record object, request string) string {
	lines := []string{
		"[codex-session-relay] verification request",
		"requestId: " + request,
		"eventId: " + event,
		"relationshipId: " + relationship,
		"executionGeneration: " + pyStr(record.get("executionGeneration")),
		"attempt: " + pyStr(record.get("attempt")),
		"outcome: " + pyStr(record.get("outcome")),
		"revisionHash: " + pyStr(record.get("revisionHash")),
	}
	if manifest, ok := record.get("manifest").([]any); ok && len(manifest) > 0 {
		lines = append(lines, fmt.Sprintf("deliverables: %d", len(manifest)))
		for _, item := range manifest[:min(len(manifest), ManifestLines)] {
			entry, _ := item.(object)
			line := "  " + pyStr(entry.get("path")) + "  sha256=" + pyStr(entry.get("sha256"))
			if size := entry.get("bytes"); size != nil {
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
	if ref := record.get("manifestRef"); truthy(ref) {
		lines = append(lines, "manifestRef: "+pyStr(ref))
	}
	if criteria, ok := record.get("criteria").([]any); ok && len(criteria) > 0 {
		lines = append(lines, "criteria claimed by the child:")
		for _, item := range criteria[:min(len(criteria), ManifestLines)] {
			entry, _ := item.(object)
			lines = append(lines, "  "+pyStr(entry.get("id"))+": "+pyStr(entry.get("verdict")))
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

// renderRevision is DeliveryService._render_revision with no work report.
func renderRevision(event, relationship string, record object, request string) string {
	lines := []string{
		"[codex-session-relay] revision request",
		"requestId: " + request,
		"eventId: " + event,
		"relationshipId: " + relationship,
		"executionGeneration: " + known(record.get("executionGeneration")) + "  (new)",
		"supersedesEvent: " + known(record.get("supersedesEvent")),
		"supersedesRevisionHash: " + known(record.get("supersedesRevisionHash")),
		"verdict: " + known(record.get("verdict")),
	}
	findings, _ := record.get("criteria").([]any)
	lines = append(lines, "", violatedHeading(record))
	for _, item := range findings[:min(len(findings), ManifestLines)] {
		finding, _ := item.(object)
		id := finding.get("id")
		if !truthy(id) {
			id = "(no id recorded)"
		}
		verdict := finding.get("verdict")
		if !truthy(verdict) {
			verdict = "no disposition recorded"
		}
		line := "  " + unheaded(inline(id)) + restorationLabel(finding) + ": " + inline(verdict)
		if note := finding.get("note"); truthy(note) {
			line += " \u2014 " + inline(note)
		} else {
			line += " \u2014 " + noNote
		}
		lines = append(lines, line)
	}
	if overflow := overflowLine(findings, event, true); overflow != "" {
		lines = append(lines, inline(overflow))
	}
	lines = append(lines, whatChangedLines(record)...)
	lines = append(lines, fixScopeLines(record)...)
	lines = append(lines, "", "PRESERVE: everything FIX SCOPE does not name, verified findings included,",
		"  work this request does not mention and any other task in-flight beside it")
	lines = append(lines, reverifyLines(record)...)
	lines = append(lines, returnLines(relationship, record.get("executionGeneration"))...)
	lines = append(lines, "", "Full record: codex-session-relay show --event "+event)
	return strings.Join(lines, "\n")
}

// overflowLine is _overflow_line.
func overflowLine(items []any, event string, nameBlock bool) string {
	if len(items) <= ManifestLines {
		return ""
	}
	hidden := items[ManifestLines:]
	detail := ""
	if nameBlock {
		for _, item := range hidden {
			if finding, _ := item.(object); truthy(finding.get("restoration")) {
				detail = ", including the restoration block on " + pyStr(finding.get("id"))
				break
			}
		}
	}
	return fmt.Sprintf("  ... %d more%s; see 'codex-session-relay show --event %s'", len(hidden), detail, event)
}

// restorationLabel is restoration.label.
func restorationLabel(finding object) string {
	if truthy(finding.get("restoration")) {
		return " [restoration block]"
	}
	return ""
}

const (
	fix, evidenceOwed, met = "needs_changes", "unverified", "verified"
	notRecorded            = "not recorded"
	noNote                 = "no note recorded; ask the parent"
)

// headings is report.HEADINGS.
var headings = []string{"VIOLATED CRITERION", "WHAT CHANGED", "FIX SCOPE", "PRESERVE", "REVERIFY AND RETURN",
	"TASK", "SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT", "DECISION BOUNDARY", "VERDICT"}

// known is report.known.
func known(value any) string {
	if value == nil || value == "" {
		return notRecorded
	}
	return inline(value)
}

// inline is report.inline: str.splitlines boundaries become " / ".
func inline(value any) string {
	text := pyStr(value)
	parts := splitlines(text)
	if len(parts) <= 1 && (len(parts) == 0 || parts[0] == text) {
		return text
	}
	var kept []string
	for _, part := range parts {
		if strings.TrimSpace(part) != "" && strings.TrimFunc(part, isPySpace) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " / ")
}

// splitlines is str.splitlines() without keepends.
func splitlines(text string) []string {
	var parts []string
	var current strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			parts = append(parts, current.String())
			current.Reset()
		case '\r':
			parts = append(parts, current.String())
			current.Reset()
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// isPySpace is str.isspace for one character.
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// unheaded is report.unheaded.
func unheaded(value string) string {
	probe := strings.TrimFunc(value, isPySpace)
	probe = strings.TrimLeft(probe, "-*#>")
	probe = strings.TrimFunc(probe, isPySpace)
	probe = strings.ToUpper(strings.Trim(probe, "*`_"))
	for _, name := range headings {
		if probe == name || strings.HasPrefix(probe, name+":") {
			return `"` + value + `"`
		}
	}
	return value
}

// correctionSource is report.correction_source with no review: the verdict's own findings.
func correctionSource(record object) []object {
	var found []object
	items, _ := record.get("criteria").([]any)
	for _, item := range items {
		if finding, ok := item.(object); ok && truthy(finding.get("id")) {
			found = append(found, finding)
		}
	}
	return found
}

type counts struct{ fix, owed, met, undecided int }

func countFindings(findings []object) counts {
	var c counts
	for _, finding := range findings {
		switch finding.get("verdict") {
		case fix:
			c.fix++
		case evidenceOwed:
			c.owed++
		case met:
			c.met++
		default:
			c.undecided++
		}
	}
	return c
}

func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func notRecordedFor(what string) string { return notRecorded + ": " + what + "; ask the parent" }

func series(words []string) string {
	if len(words) == 1 {
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// violatedHeading is report.violated_heading with no review.
func violatedHeading(record object) string {
	findings := correctionSource(record)
	if len(findings) == 0 {
		return "VIOLATED CRITERION: " + notRecordedFor("the verdict named no criterion")
	}
	c := countFindings(findings)
	var parts []string
	for _, part := range []struct {
		n    int
		text string
	}{{c.fix, "marked needs_changes"}, {c.owed, "marked unverified"}, {c.met, "marked verified"}, {c.undecided, "without a disposition"}} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.text))
		}
	}
	return "VIOLATED CRITERION: of the " + plural(len(findings), "recorded finding") + ", " + strings.Join(parts, ", ")
}

// whatChangedLines is report.what_changed_lines, plain, with no review.
func whatChangedLines(record object) []string {
	return []string{"",
		"WHAT CHANGED: submission " + known(record.get("supersedesEvent")) + " ruled " + known(record.get("verdict")) +
			" and superseded, generation " + known(record.get("executionGeneration")) + " opened",
		"  superseded revision " + known(record.get("supersedesRevisionHash")) + ", ruled in turn " + known(record.get("verdictTurnId")),
	}
}

// fixScopeLines is report.fix_scope_lines, plain, with no review.
func fixScopeLines(record object) []string {
	findings := correctionSource(record)
	if len(findings) == 0 {
		return []string{"", "FIX SCOPE: " + notRecordedFor("no criterion was named, so nothing bounds a change")}
	}
	c := countFindings(findings)
	unruled := "FIX SCOPE: no finding is marked needs_changes, so nothing is ruled violated"
	var heading string
	switch {
	case c.fix > 0:
		heading = "FIX SCOPE: only the " + plural(c.fix, "finding") + " marked needs_changes, shown or not; " +
			series([]string{"verified", "unverified"}) + " findings are out of scope"
	case c.owed > 0:
		heading = unruled + "; change nothing but the evidence REVERIFY AND RETURN asks for"
	case c.undecided > 0:
		heading = unruled + "; change nothing until the parent settles the findings below"
	default:
		heading = "FIX SCOPE: " + notRecordedFor("what to change, since every finding is marked verified")
	}
	lines := []string{"", heading}
	if c.undecided > 0 {
		pronoun := "them"
		if c.undecided == 1 {
			pronoun = "it"
		}
		lines = append(lines, "  "+plural(c.undecided, "finding")+" without a disposition: whether to change "+pronoun+" is not recorded; ask the parent first")
	}
	return append(lines, "  anything wider, anything that would discard preserved work, or anything needing authority you were not given comes back to the parent")
}

// reverifyLines is report.reverify_lines with no review and no proof section.
func reverifyLines(record object) []string {
	findings := correctionSource(record)
	var clauses []string
	if len(findings) == 0 {
		clauses = []string{"what to re-check is " + notRecordedFor("the verdict named no criterion")}
	} else {
		c := countFindings(findings)
		if c.fix > 0 {
			clauses = append(clauses, "re-check each finding FIX SCOPE names")
		}
		if c.owed > 0 {
			clauses = append(clauses, "show evidence for the "+plural(c.owed, "finding")+" marked unverified")
		}
		if c.undecided > 0 {
			clauses = append(clauses, "settle with the parent whether to change the "+plural(c.undecided, "finding")+" without a disposition")
		}
		if len(clauses) == 0 {
			clauses = append(clauses, "what to re-check is "+notRecordedFor("every finding is marked verified"))
		}
	}
	return []string{"", "REVERIFY AND RETURN: " + strings.Join(append(clauses, "then hand back as below"), "; ")}
}

// returnLines is report.return_lines.
func returnLines(relationship string, generation any) []string {
	shown := "<not recorded; ask the parent>"
	if generation != nil && generation != "" {
		shown = inline(generation)
	}
	return []string{"",
		"There is nothing to acknowledge. Contract v1 defines no acknowledgement for this",
		"direction and the relay refuses one by kind, so there is no proof to compute and",
		"no acknowledgement to send.",
		"Answer with your next completion receipt under the new generation:",
		"  emit --relationship " + relationship + " --generation " + shown + " --attempt <n>",
		"       --outcome ready_for_review --turn-thread <your task id> --turn-id <your turn>",
		"       --artifact <path> [--continues-anchor <this generation dispatch turn>]",
	}
}
