package faults

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// f1Inputs lets replay tests inject the clock and entropy before executing the CLI.
// Production callers use the system clock and crypto/rand.
type f1Inputs struct {
	clock   Clock
	entropy io.Reader
}
type f1InputsKey struct{}

func f1Clock(ctx context.Context) Clock {
	if inputs, ok := ctx.Value(f1InputsKey{}).(f1Inputs); ok && inputs.clock != nil {
		return inputs.clock
	}
	return cliClock{}
}

var f1Names = []string{"fault-claim", "fault-operation", "fault-reconcile", "fault-complete", "fault-sweep"}

func executeF1(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	switch name {
	case "fault-claim":
		return l.Claim(ctx, a["--publication"], a["--owner"], a["--takeover"] != "")
	case "fault-operation":
		return l.Operation(ctx, a["--publication"], a["--claim-token"])
	case "fault-reconcile":
		return f1Reconcile(ctx, l, a)
	case "fault-complete":
		return f1Complete(ctx, l, a)
	case "fault-sweep":
		return f1Sweep(ctx, l, a)
	}
	return nil, fmt.Errorf("unknown fault command %q", name)
}
func f1Column(name string, value any) store.Column { return store.Column{Name: name, Value: value} }
func f1Number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	}
	return 0
}
func f1Claim(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	id, owner := a["--publication"], a["--owner"]
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a claim names its owner")
	}
	bytes := make([]byte, 8)
	var entropy io.Reader = rand.Reader
	if inputs, ok := ctx.Value(f1InputsKey{}).(f1Inputs); ok && inputs.entropy != nil {
		entropy = inputs.entropy
	}
	if _, e := io.ReadFull(entropy, bytes); e != nil {
		return nil, e
	}
	token := hex.EncodeToString(bytes)
	moment, stamp := l.Clock.Now(), l.Clock.ISO()
	var lease float64
	var refusal error
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no publication %q", id)
		}
		kind := r.Text("kind")
		if _, ok := executableKind(kind); !ok {
			return f1Unregistered(kind)
		}
		state := r.Text("state")
		if state != "pending" {
			suffix := ""
			if state == "uncertain" {
				suffix = ". An uncertain write is not reclaimed; reconcile it by reporting what you observed"
			}
			return fmt.Errorf("fault_not_claimable: publication %s is %s%s", id, state, suffix)
		}
		fault, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", r.Text("fault_id"))
		if e != nil {
			return e
		}
		if r.Text("kind") == openRecord {
			slot, _, e := issueSlot(ctx, l, fault)
			if e != nil {
				return e
			}
			if slot == "issue" {
				if e = f1CancelClaim(ctx, l, r, stamp, "the fault already owns an issue"); e != nil {
					return e
				}
				refusal = fmt.Errorf("fault_state_conflict: this fault owns an issue, so its create was cancelled")
				return nil
			}
		}
		classified := append(row{}, r...)
		classified = append(classified, f1Column("scope_key", fault.Get("scope_key")), f1Column("product", fault.Get("product")), f1Column("owned_ref", fault.Get("external_ref")))
		held, e := dAttentionReason(ctx, l, classified, moment)
		if e != nil {
			return e
		}
		if held == "held" {
			return fmt.Errorf("fault_budget_spent: %s's %s budget is spent; the write stays pending", fault.Text("product"), kind)
		}
		if held != "ready" {
			reasons := map[string]string{"backingOff": "backing_off", "awaitingRecord": "awaiting_record", "awaitingTarget": "awaiting_target", "scopeKeyContested": "scope_key_contested", "kindUnregistered": "kind_unregistered", "issueOwned": "issue_owned"}
			return fmt.Errorf("fault_not_claimable: publication %s is not claimable: %s", id, reasons[held])
		}
		writer, e := l.one(ctx, "SELECT owner FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id LIMIT 1", id)
		if e != nil {
			return e
		}
		takeover := writer != nil && writer.Text("owner") != owner
		if takeover && a["--takeover"] == "" {
			return fmt.Errorf("fault_writer_conflict: this write belongs to %s; pass takeover to reassign it", quote.Value(writer.Text("owner")))
		}
		attempt := integer(r, "attempts") + 1
		takeoverInt := 0
		if takeover {
			takeoverInt = 1
		}
		res, e := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO fault_publication_attempts(publication_id,attempt,owner,takeover,claimed_at,claimed_ts) VALUES(?,?,?,?,?,?)", id, attempt, owner, takeoverInt, stamp, moment)
		if e != nil {
			return e
		}
		attemptID, e := res.LastInsertId()
		if e != nil {
			return e
		}
		product := fault.Text("product")
		limit, window := int64(20), 3600.0
		if kind == openRecord {
			limit = 5
		}
		override, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind=?", product, kind)
		if e != nil {
			return e
		}
		if override != nil {
			limit = integer(override, "max_count")
			window = f1Number(override.Get("window_seconds"))
		}
		spent, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind=? AND used_ts>?", product, kind, moment-window)
		if e != nil {
			return e
		}
		if integer(spent, "n") >= limit {
			return fmt.Errorf("fault_budget_spent: %s's %s budget is spent", product, kind)
		}
		if _, e = l.exec(ctx, "INSERT INTO fault_budget_uses(product,kind,ref,used_at,used_ts) VALUES(?,?,?,?,?)", product, kind, fmt.Sprintf("%s:%d", id, attemptID), stamp, moment); e != nil {
			return e
		}
		lease = moment + 300
		_, e = l.exec(ctx, "UPDATE fault_publications SET state='claimed',claim_token=?,lease_owner=?,lease_until=?,attempts=?,updated_at=? WHERE publication_id=?", token, owner, lease, attempt, stamp, id)
		return e
	})
	if err == nil && refusal != nil {
		return nil, refusal
	}
	return map[string]any{"publicationId": id, "claimToken": token, "owner": owner, "leaseUntil": lease}, err
}

func f1Operation(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	id := a["--publication"]
	moment, stamp := l.Clock.Now(), l.Clock.ISO()
	var refusal error
	var answer map[string]any
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no publication %q", id)
		}
		if r.Text("state") != "claimed" {
			return fmt.Errorf("fault_not_claimable: an operation is handed out for a claimed publication; this one is %s", r.Text("state"))
		}
		if r.Text("claim_token") != a["--claim-token"] {
			return fmt.Errorf("fault_claim_stale: this claim token is not the current one")
		}
		kind := r.Text("kind")
		spec, ok := executableKind(kind)
		if !ok {
			return f1Unregistered(kind)
		}
		fault, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", r.Text("fault_id"))
		if e != nil {
			return e
		}
		if kind == openRecord && fault.Text("external_ref") != "" {
			if e = f1CancelClaim(ctx, l, r, stamp, "the fault already owns an issue"); e != nil {
				return e
			}
			refusal = fmt.Errorf("fault_state_conflict: this fault owns an issue, so its create was cancelled")
			return nil
		}
		extra, e := l.one(ctx, "SELECT * FROM fault_publication_payloads WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		var project any
		if extra != nil {
			project = extra.Get("project_ref")
		}
		if spec.Target != "" {
			target, why, e := f1OwnedTarget(ctx, l, fault)
			if e != nil {
				return e
			}
			var wantedTeam, wantedProject any
			if target != nil {
				wantedTeam = target.Get("tracker_ref")
				if spec.Target == "team+project" {
					wantedProject = target.Get("project_ref")
				}
				why = "retargeted"
			}
			if wantedTeam != r.Get("tracker_ref") || wantedProject != project || wantedTeam == nil || spec.Target == "team+project" && wantedProject == nil {
				if _, e = l.exec(ctx, "UPDATE fault_publications SET tracker_ref=? WHERE publication_id=?", wantedTeam, id); e != nil {
					return e
				}
				if _, e = l.exec(ctx, "UPDATE fault_publication_payloads SET project_ref=?,updated_at=? WHERE publication_id=?", wantedProject, stamp, id); e != nil {
					return e
				}
				if e = f1Release(ctx, l, r, stamp, "retargeted", nil, ""); e != nil {
					return e
				}
				refusal = fmt.Errorf("fault_not_claimable: the target changed since the claim (%s); the write was re-pointed and returned to pending, not issued", why)
				return nil
			}
		}
		if spec.RequiresIssue && fault.Text("external_ref") == "" {
			return fmt.Errorf("fault_not_claimable: this fault owns no issue yet")
		}
		if kind == "update_record" && extra != nil {
			payload := loadsMap(extra.Text("payload"))
			if payload["op"] == "set_project" {
				target, e := l.one(ctx, "SELECT project_ref,product FROM fault_target_projects WHERE scope_key=?", fault.Text("scope_key"))
				if e != nil {
					return e
				}
				var current any
				if target != nil && target.Text("product") == fault.Text("product") {
					current = target.Get("project_ref")
				}
				var reason string
				if current == nil {
					reason = fmt.Sprintf("the scope no longer targets a project %s owns, so %v is not where the issue belongs", fault.Text("product"), payload["value"])
				} else if current != payload["value"] {
					reason = fmt.Sprintf("the scope now targets %v, not %v", current, payload["value"])
				}
				if reason != "" {
					if e = f1CancelClaim(ctx, l, r, stamp, reason); e != nil {
						return e
					}
					target, _, e := f1OwnedTarget(ctx, l, fault)
					if e != nil {
						return e
					}
					if target != nil && target.Text("project_ref") != "" {
						e = dRelinkOne(ctx, l, r.Text("fault_id"), target.Text("project_ref"), stamp)
					} else {
						e = dUnlinkOne(ctx, l, r.Text("fault_id"), stamp)
					}
					if e != nil {
						return e
					}
					refusal = fmt.Errorf("fault_not_claimable: cancelled before issue: %s", reason)
					return nil
				}
			}
		}
		refusal, e = preIssue(ctx, l, spec, r, fault, moment, stamp)
		if e != nil || refusal != nil {
			return e
		}
		if _, e = l.exec(ctx, "UPDATE fault_publications SET state='issued',issued_at=?,updated_at=? WHERE publication_id=?", stamp, stamp, id); e != nil {
			return e
		}
		if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET issued_at=?,issued_ts=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", stamp, moment, id); e != nil {
			return e
		}
		title := fmt.Sprintf("[%s] %s: %s", fault.Text("product"), fault.Text("fault_class"), domain(fault))
		var payload any
		if extra != nil && extra.Get("payload") != nil {
			payload, e = loads(extra.Text("payload"))
			if e != nil {
				return e
			}
		}
		reference := fault.Get("external_ref")
		if !named(reference) {
			reference = r.Get("external_ref")
		}
		answer = map[string]any{"publicationId": id, "kind": kind, "trackerRef": r.Get("tracker_ref"), "projectRef": project, "externalRef": reference, "title": title, "identityDigest": r.Get("identity_digest"), "protocol": f1Protocol(kind), "note": "the connector's create takes no idempotency key, so a create can succeed and lose its response. This row is now issued: if you cannot report an outcome it becomes uncertain, and no second create is made until somebody reports what they observed.", "payload": payload}
		if spec.Evidence == "block" {
			answer["block"] = f1RenderBlock(r, fault)
			answer["startMarker"] = "<!-- relay-fault:" + id + " -->"
			answer["endMarker"] = "<!-- /relay-fault:" + id + " -->"
		}
		if kind == "update_record" {
			answer["update"] = payload
		}
		return nil
	})
	if err == nil && refusal != nil {
		return nil, refusal
	}
	return answer, err
}
func f1Unregistered(kind string) error {
	return fmt.Errorf("fault_kind_unregistered: kind %s is not registered in this process; load the module that declares it (--kind-module) before acting on its writes", quote.Value(kind))
}

func f1OwnedTarget(ctx context.Context, l *Ledger, fault row) (row, string, error) {
	target, e := l.one(ctx, "SELECT t.tracker_ref,p.project_ref,p.product FROM fault_targets t LEFT JOIN fault_target_projects p ON p.scope_key=t.scope_key WHERE t.scope_key=?", fault.Text("scope_key"))
	if e != nil || target == nil || target.Get("product") == nil {
		return nil, "awaiting_target", e
	}
	contested, e := l.one(ctx, "SELECT 1 FROM fault_ledger WHERE scope_key=? AND product!=? LIMIT 1", fault.Text("scope_key"), fault.Text("product"))
	if e != nil {
		return nil, "", e
	}
	if target.Text("product") != fault.Text("product") || contested != nil {
		return nil, "scope_key_contested", nil
	}
	return target, "", nil
}

func f1CancelClaim(ctx context.Context, l *Ledger, r row, stamp, reason string) error {
	id := r.Text("publication_id")
	attempts := integer(r, "attempts")
	if r.Text("state") == "claimed" {
		if _, e := l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=(SELECT product FROM fault_ledger WHERE fault_id=?) AND kind=? AND ref=(SELECT publication_id || ':' || attempt_id FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 1)", r.Text("fault_id"), r.Text("kind"), id); e != nil {
			return e
		}
		if _, e := l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='cancelled',ended=1,ended_at=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", stamp, id); e != nil {
			return e
		}
		attempts--
	}
	_, e := l.exec(ctx, "UPDATE fault_publications SET state='cancelled',claim_token=NULL,lease_owner=NULL,lease_until=NULL,last_error=?,updated_at=?,attempts=? WHERE publication_id=?", reason, stamp, attempts, id)
	return e
}
func f1Release(ctx context.Context, l *Ledger, r row, stamp, outcome string, next any, hold string) error {
	id := r.Text("publication_id")
	if _, e := l.exec(ctx, "UPDATE fault_publications SET state='pending',claim_token=NULL,lease_owner=NULL,lease_until=NULL,attempts=attempts-1,next_attempt_at=?,updated_at=? WHERE publication_id=?", next, stamp, id); e != nil {
		return e
	}
	if _, e := l.exec(ctx, "UPDATE fault_publication_attempts SET outcome=?,ended=1,ended_at=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", outcome, stamp, id); e != nil {
		return e
	}
	attempt, e := l.one(ctx, "SELECT MAX(attempt_id) AS id FROM fault_publication_attempts WHERE publication_id=?", id)
	if e != nil {
		return e
	}
	fault, e := l.one(ctx, "SELECT product FROM fault_ledger WHERE fault_id=?", r.Text("fault_id"))
	if e != nil {
		return e
	}
	_, e = l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=? AND kind=? AND ref=?", fault.Text("product"), r.Text("kind"), fmt.Sprintf("%s:%d", id, integer(attempt, "id")))
	if e != nil {
		return e
	}
	_, e = l.exec(ctx, "INSERT INTO fault_publication_payloads(publication_id,hold_reason,updated_at) VALUES(?,?,?) ON CONFLICT(publication_id) DO UPDATE SET hold_reason=excluded.hold_reason,updated_at=excluded.updated_at", id, nilIfEmpty(hold), stamp)
	return e
}
func f1Protocol(kind string) []any {
	switch kind {
	case openRecord:
		return []any{"search the tracker for this publication's start marker BEFORE creating anything", "marker found: the create already landed. Complete from that observation and do NOT create again", "marker absent: create one issue IN projectRef whose description carries this block verbatim", "read the created issue back and pass its full text to complete, with its identifier as the external reference and the project it reads back as", "response lost, or you cannot tell: report failure. The row becomes uncertain and no second create is made until somebody attests the request ended and nothing landed"}
	case appendComment:
		return []any{"read the issue's comments and look for this publication's start marker", "marker found: this comment already landed. Complete from that observation", "marker absent: add one comment carrying this block verbatim", "read it back and pass the comment text to complete"}
	case "update_record":
		return []any{"read the owned issue's current fields", "already as requested: complete from that observation", "otherwise apply the one update and read the issue back", "pass what was read back to complete as observed, naming the issue"}
	}
	return []any{kind + ": follow the protocol its registering module documents", "complete from what was read back; a lost response is uncertain, never repeated"}
}
func f1RenderBlock(r, fault row) string {
	summary := strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(r.Text("summary"), "\r\n", "\n"), "\r", "\n"), "\n")
	maxRun, run := 0, 0
	for _, c := range summary {
		if c == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	if maxRun < 2 {
		maxRun = 2
	}
	fence := strings.Repeat("`", maxRun+1)
	id := r.Text("publication_id")
	fields := []string{"<!-- relay-fault:" + id + " -->", "blockFormat: v1", "publicationId: " + id, "faultId: " + r.Text("fault_id"), "product: " + fault.Text("product"), "faultClass: " + fault.Text("fault_class"), "trigger: " + r.Text("trigger_key"), fmt.Sprintf("cycle: %d", integer(r, "cycle")), "identityDigest: " + r.Text("identity_digest"), "summarySha256: " + pyvalue.SHA256Hex(summary), "", fence + "text", summary, fence, "<!-- /relay-fault:" + id + " -->"}
	return strings.Join(fields, "\n")
}

func f1Argument(raw, label string) (string, error) {
	if path, ok := strings.CutPrefix(raw, "@"); ok {
		data, e := os.ReadFile(path)
		if e != nil {
			return "", e
		}
		return strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), nil
	}
	return raw, nil
}
func f1Fields(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	value, e := f1Argument(raw, "observed fields")
	if e != nil {
		return nil, e
	}
	parsed, e := loads(value)
	if e != nil {
		return nil, fmt.Errorf("fault_observation_malformed: the observed fields is not readable JSON: %v", e)
	}
	result, _ := parsed.(map[string]any)
	return result, nil
}
func f1Reconcile(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	id := a["--publication"]
	fields, e := f1Fields(a["--observed-fields"])
	if e != nil {
		return nil, e
	}
	observed, e := f1Argument(a["--observed"], "observed")
	if e != nil {
		return nil, e
	}
	stamp, moment := l.Clock.ISO(), l.Clock.Now()
	var result map[string]any
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no publication %q", id)
		}
		kind := r.Text("kind")
		spec, ok := executableKind(kind)
		if !ok {
			return f1Unregistered(kind)
		}
		fault, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", r.Text("fault_id"))
		if e != nil {
			return e
		}
		state := r.Text("state")
		result = map[string]any{"publicationId": id, "state": state}
		outcome := func(name, detail string) error { result["outcome"] = name; result["detail"] = detail; return nil }
		attested := a["--searched"] != ""
		if spec.Evidence == "block" {
			found := f1ReadBlock(observed, id)
			if found.duplicate {
				return outcome("duplicate", "more than one block for this publication is present; repair it before confirming")
			}
			if found.found {
				if len(found.problems) > 0 {
					result["outcome"] = "malformed"
					result["problems"] = found.problems
					result["detail"] = "a partial block is not proof that nothing landed"
					return nil
				}
				return outcome("present", "this write already landed; complete from this observation")
			}
		} else {
			if fields == nil || fields["issue"] != fault.Get("external_ref") {
				return outcome("absent_unattested", "fields are attested only by a readback naming the issue this fault owns")
			}
			extra, e := l.one(ctx, "SELECT payload FROM fault_publication_payloads WHERE publication_id=?", id)
			if e != nil {
				return e
			}
			if len(f1ConfirmFields(extra, fields)) == 0 {
				return outcome("present", "the owned issue already reads as this update; complete from this observation")
			}
			attested = true
		}
		if !attested {
			return outcome("absent_unattested", "no block was observed, and nobody attested that the search covered where it would be")
		}
		if state == "claimed" {
			return outcome("absent", "nothing was issued under this claim; its holder may write")
		}
		if state == "issued" && r.Get("lease_until") != nil && f1Number(r.Get("lease_until")) > moment {
			return outcome("absent_in_flight", "the write is still held under a live lease; an absence read now proves nothing about a request that may still land")
		}
		if state != "issued" && state != "uncertain" {
			return outcome("absent", "nothing is outstanding")
		}
		prior := a["--prior-ended"] != ""
		if prior && strings.TrimSpace(a["--reason"]) == "" {
			return fmt.Errorf("fault_observation_malformed: an attested end says what ended the request")
		}
		latest, e := l.one(ctx, "SELECT attempt_id,attempt,ended FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 1", id)
		if e != nil {
			return e
		}
		ended := latest != nil && integer(latest, "ended") == 1
		if prior && latest != nil {
			if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET ended=1,ended_at=?,error=COALESCE(error || '; ', '') || ? WHERE attempt_id=?", stamp, "attested end: "+a["--reason"], integer(latest, "attempt_id")); e != nil {
				return e
			}
		}
		if !ended && !prior {
			return outcome("absent_unproven", "nobody attested that the issuing request ended, and one still travelling can land after this search; the write stays uncertain")
		}
		if _, e = l.exec(ctx, "UPDATE fault_publications SET state='pending',claim_token=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE publication_id=?", stamp, id); e != nil {
			return e
		}
		if latest != nil {
			if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='reconciled_absent' WHERE attempt_id=?", integer(latest, "attempt_id")); e != nil {
				return e
			}
		}
		query, _, args, _ := dRepointFaultQuery(r.Text("fault_id"))
		args = append(args, 100)
		selected, e := l.Store.All(ctx, query+" ORDER BY p.rowid LIMIT ?", args...)
		if e != nil {
			return e
		}
		for _, entry := range selected {
			publication := entry.Text("publication_id")
			if _, e = l.exec(ctx, "UPDATE fault_publications SET tracker_ref=?,updated_at=? WHERE publication_id=?", entry.Get("team"), stamp, publication); e != nil {
				return e
			}
			if _, e = l.exec(ctx, "UPDATE fault_publication_payloads SET project_ref=?,updated_at=? WHERE publication_id=?", entry.Get("project"), stamp, publication); e != nil {
				return e
			}
		}
		result["state"] = "pending"
		return outcome("absent", "the attested search found nothing after the request ended, so one further write is permitted")
	})
	return result, err
}

type f1Block struct {
	found, duplicate bool
	fields           map[string]string
	summary          string
	problems         []any
}

func f1ReadBlock(observed, id string) f1Block {
	answer := f1Block{fields: map[string]string{}, problems: []any{}}
	if observed == "" {
		return answer
	}
	lines := strings.Split(observed, "\n")
	start := "<!-- relay-fault:" + id + " -->"
	end := "<!-- /relay-fault:" + id + " -->"
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != start {
			continue
		}
		if answer.found {
			answer.duplicate = true
			answer.problems = append(answer.problems, "more than one block for this publication is present")
			break
		}
		answer.found = true
		i++
		seen := map[string]bool{}
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" && strings.TrimSpace(lines[i]) != end {
			key, value, ok := strings.Cut(lines[i], ":")
			key = strings.TrimSpace(key)
			if ok && key != "" && !strings.Contains(key, " ") {
				if seen[key] {
					answer.problems = append(answer.problems, fmt.Sprintf("duplicate header %q", key))
				}
				seen[key] = true
				answer.fields[key] = strings.TrimSpace(value)
			} else {
				answer.problems = append(answer.problems, "the header region contains a line that is not a header")
			}
			i++
		}
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i >= len(lines) || f1Fence(strings.TrimSpace(lines[i])) == 0 {
			answer.problems = append(answer.problems, "the block carries no fenced summary")
		} else {
			fence := strings.TrimSpace(lines[i])
			size := 0
			for size < len(fence) && fence[size] == '`' {
				size++
			}
			i++
			body := []string{}
			closed := false
			for i < len(lines) {
				line := strings.TrimSpace(lines[i])
				count := 0
				for count < len(line) && line[count] == '`' {
					count++
				}
				if count >= size && f1Fence(line) == count && strings.TrimSpace(line[count:]) == "" {
					closed = true
					i++
					break
				}
				body = append(body, lines[i])
				i++
			}
			if closed {
				answer.summary = strings.Join(body, "\n")
			} else {
				answer.problems = append(answer.problems, "the fenced summary is not closed")
			}
		}
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i >= len(lines) || strings.TrimSpace(lines[i]) != end {
			answer.problems = append(answer.problems, "the block is not terminated")
		}
	}
	return answer
}
func f1Fence(line string) int {
	n := 0
	for n < len(line) && line[n] == '`' {
		n++
	}
	if n < 3 || strings.Contains(line[n:], "`") {
		return 0
	}
	return n
}
func f1ConfirmFields(extra row, observed map[string]any) []string {
	var payload map[string]any
	if extra != nil && extra.Get("payload") != nil {
		payload = loadsMap(extra.Text("payload"))
	}
	op, _ := payload["op"].(string)
	value := payload["value"]
	switch op {
	case "set_project":
		if observed["projectId"] == value {
			return nil
		}
		return []string{fmt.Sprintf("the issue reads project %s, not %s", quote.Value(observed["projectId"]), quote.Value(value))}
	case "reopen":
		if observed["open"] == true {
			return nil
		}
		return []string{"the issue does not read as open"}
	case "add_relation", "add_label":
		key, item := "relations", "relation"
		if op == "add_label" {
			key, item = "labels", "label"
		}
		values, _ := observed[key].([]any)
		for _, v := range values {
			if dumps(v, true) == dumps(value, true) {
				return nil
			}
		}
		return []string{fmt.Sprintf("the issue has no %s %s", item, quote.Value(value))}
	}
	return []string{fmt.Sprintf("unknown update %s", quote.Value(op))}
}
func f1Mismatch(r, fault row, found f1Block) []string {
	if !found.found {
		return []string{"no block for this publication was observed in the readback"}
	}
	if found.duplicate {
		return []string{"more than one block for this publication is present"}
	}
	if len(found.problems) > 0 {
		out := []string{}
		for _, p := range found.problems {
			out = append(out, p.(string))
		}
		return out
	}
	summary := strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(r.Text("summary"), "\r\n", "\n"), "\r", "\n"), "\n")
	expected := map[string]string{"blockFormat": "v1", "publicationId": r.Text("publication_id"), "faultId": r.Text("fault_id"), "product": fault.Text("product"), "faultClass": fault.Text("fault_class"), "trigger": r.Text("trigger_key"), "cycle": fmt.Sprint(integer(r, "cycle")), "identityDigest": r.Text("identity_digest"), "summarySha256": pyvalue.SHA256Hex(summary)}
	problems := []string{}
	for _, key := range []string{"blockFormat", "publicationId", "faultId", "product", "faultClass", "trigger", "cycle", "identityDigest", "summarySha256"} {
		actual, ok := found.fields[key]
		if !ok || actual != expected[key] {
			var value any
			if ok {
				value = actual
			}
			problems = append(problems, fmt.Sprintf("%s reads %s, not %s", key, quote.Value(value), quote.Value(expected[key])))
		}
	}
	observed := strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(found.summary, "\r\n", "\n"), "\r", "\n"), "\n")
	if observed != summary {
		problems = append(problems, "the summary in the record is not the summary this job carries")
	}
	return problems
}
func f1Complete(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	id := a["--publication"]
	observed, e := f1Fields(a["--observed-fields"])
	if e != nil {
		return nil, e
	}
	readback := ""
	if a["--readback"] != "" {
		readback, e = f1Argument(a["--readback"], "readback")
		if e != nil {
			return nil, e
		}
	}
	stamp := l.Clock.ISO()
	var result map[string]any
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no publication %q", id)
		}
		state := r.Text("state")
		if state == "confirmed" {
			result = cView(r)
			result["confirmed"] = false
			result["reason"] = "already confirmed"
			return nil
		}
		if state != "claimed" && state != "issued" && state != "uncertain" {
			return fmt.Errorf("fault_state_conflict: a %s publication has no write outstanding to confirm; claim it and write it again", state)
		}
		if state != "uncertain" && r.Text("claim_token") != a["--claim-token"] {
			return fmt.Errorf("fault_claim_stale: this claim token is not the current one")
		}
		kind := r.Text("kind")
		spec, ok := executableKind(kind)
		if !ok {
			return f1Unregistered(kind)
		}
		fault, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", r.Text("fault_id"))
		if e != nil {
			return e
		}
		extra, e := l.one(ctx, "SELECT * FROM fault_publication_payloads WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		var reference any
		if spec.Confirm != nil {
			expected, e := cPublication(ctx, l, id)
			if e != nil {
				return e
			}
			if problems := spec.Confirm(expected, observed); len(problems) > 0 {
				return fmt.Errorf("fault_readback_mismatch: %s", strings.Join(problems, "; "))
			}
		}
		if spec.Evidence == "block" {
			problems := f1Mismatch(r, fault, f1ReadBlock(readback, id))
			if len(problems) > 0 {
				return fmt.Errorf("fault_readback_mismatch: %s", strings.Join(problems, "; "))
			}
			reference = r.Get("external_ref")
			if a["--external-ref"] != "" {
				reference = a["--external-ref"]
			}
			if kind == appendComment {
				owned := fault.Get("external_ref")
				if !named(reference) {
					reference = owned
				}
				if !named(owned) {
					return fmt.Errorf("fault_state_conflict: this fault owns no issue yet, so a comment on it cannot be confirmed")
				}
				if reference != owned {
					return fmt.Errorf("fault_readback_mismatch: this comment names %s, and the fault owns %s", quote.Value(reference), quote.Value(owned))
				}
			}
			if spec.Creates && !named(reference) {
				return fmt.Errorf("fault_readback_mismatch: a confirmed create must name what it created")
			}
			if kind == openRecord && strings.TrimSpace(a["--project-ref"]) == "" {
				return fmt.Errorf("fault_readback_mismatch: a confirmed issue create must name the project the saved issue reads back as; a create is not confirmed into an unknown project")
			}
		} else {
			if observed == nil || observed["issue"] != fault.Get("external_ref") {
				return fmt.Errorf("fault_readback_mismatch: an update is confirmed from a readback naming the issue this fault owns")
			}
			if problems := f1ConfirmFields(extra, observed); len(problems) > 0 {
				return fmt.Errorf("fault_readback_mismatch: %s", strings.Join(problems, "; "))
			}
			reference = fault.Get("external_ref")
		}
		if _, e = l.exec(ctx, "UPDATE fault_publications SET state='confirmed',external_ref=?,confirmed_at=?,claim_token=NULL,lease_owner=NULL,lease_until=NULL,last_error=NULL,updated_at=? WHERE publication_id=?", reference, stamp, stamp, id); e != nil {
			return e
		}
		if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='confirmed',ended=1,ended_at=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", stamp, id); e != nil {
			return e
		}
		if kind == openRecord {
			if _, e = l.exec(ctx, "UPDATE fault_ledger SET external_ref=?,published_at=?,updated_at=? WHERE fault_id=? AND external_ref IS NULL", reference, stamp, stamp, r.Text("fault_id")); e != nil {
				return e
			}
			if e = f1ObserveLink(ctx, l, r.Text("fault_id"), reference, a["--project-ref"], stamp); e != nil {
				return e
			}
		} else if kind == "update_record" && extra != nil {
			payload := loadsMap(extra.Text("payload"))
			if payload["op"] == "set_project" {
				if e = f1ObserveLink(ctx, l, r.Text("fault_id"), reference, payload["value"], stamp); e != nil {
					return e
				}
			}
		}
		fresh, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		result = cView(fresh)
		result["confirmed"] = true
		return nil
	})
	return result, err
}
func f1ObserveLink(ctx context.Context, l *Ledger, id string, reference, observed any, stamp string) error {
	fault, e := l.one(ctx, "SELECT product,scope_key FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	target, _, e := f1OwnedTarget(ctx, l, fault)
	if e != nil {
		return e
	}
	var wanted any
	if target != nil && target.Text("product") == fault.Text("product") {
		wanted = target.Get("project_ref")
	}
	link, e := l.one(ctx, "SELECT * FROM fault_links WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	moving, e := l.one(ctx, "SELECT 1 FROM fault_publications p LEFT JOIN fault_publication_payloads pp ON pp.publication_id=p.publication_id WHERE p.fault_id=? AND p.kind='update_record' AND p.state IN ('issued','uncertain') AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.op') END)='set_project' AND (CASE WHEN json_valid(pp.payload) THEN json_extract(pp.payload,'$.value') END) IS NOT ? LIMIT 1", id, wanted)
	if e != nil {
		return e
	}
	state := "unlinked"
	if wanted != nil && wanted == observed && moving == nil {
		state = "linked"
	}
	if link == nil {
		_, e = l.exec(ctx, "INSERT INTO fault_links(fault_id,external_ref,project_ref,observed_project_ref,state,revision,updated_at) VALUES(?,?,?,?,?,0,?)", id, reference, wanted, observed, state, stamp)
	} else {
		storedState := state
		if link.Get("project_ref") != nil && link.Get("project_ref") != wanted {
			storedState = "unlinked"
		}
		_, e = l.exec(ctx, "UPDATE fault_links SET external_ref=?,observed_project_ref=?,state=?,updated_at=? WHERE fault_id=?", reference, observed, storedState, stamp, id)
	}
	if e != nil {
		return e
	}
	if state == "unlinked" {
		if wanted == nil {
			return dUnlinkOne(ctx, l, id, stamp)
		}
		return dRelinkOne(ctx, l, id, wanted.(string), stamp)
	}
	return nil
}

// sweepInstallation is the installation fault-sweep's facts name: the running binary's own.
// The package's replay tests name the checkout's Python package, the copy their live-Python
// oracle runs from, so its facts compare byte for byte.
var sweepInstallation = ExecutableInstallation

type sweepInput struct {
	readings any
	after    int64
}
type sweepInputKey struct{}

func validateSweep(ctx context.Context, a map[string]string) (sweepInput, error) {
	input := sweepInput{}
	if a["--readings"] != "" {
		raw, e := f1Argument(a["--readings"], "readings")
		if e != nil {
			return input, e
		}
		value, e := loads(raw)
		if e != nil {
			return input, fmt.Errorf("fault_observation_malformed: the readings is not readable JSON: %v", e)
		}
		input.readings = value
	}
	if a["--readings-after"] != "" {
		var ok bool
		if input.after, ok = integerArg(ctx, "--readings-after", a["--readings-after"]); !ok || input.after < 0 {
			return input, fmt.Errorf("fault_observation_malformed: --readings-after is a non-negative integer, not %s", a["--readings-after"])
		}
	}
	return input, nil
}

func f1Sweep(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	input, ok := ctx.Value(sweepInputKey{}).(sweepInput)
	if !ok {
		var err error
		input, err = validateSweep(ctx, a)
		if err != nil {
			return nil, err
		}
	}
	product := a["--product"]
	if product == "" {
		product = "crw"
	}
	readings := []any{}
	if list, ok := input.readings.([]any); ok {
		readings = list
	} else if input.readings != nil {
		return nil, fmt.Errorf("fault_observation_malformed: readings are a list of reporting-observation/1 objects")
	}
	after := int(input.after)
	hostRecord, err := HostRecordPath()
	if err != nil {
		return nil, err
	}
	installation, e := sweepInstallation()
	if e != nil {
		return nil, e
	}
	sw := &Sweeper{Store: l.Store, MaxAttempts: 6, Now: l.Clock.ISO, HostRecordPath: hostRecord, Installation: installation}
	sw.SupersessionReason = func(ctx context.Context, event string) (string, error) { return f1SupersessionReason(ctx, l, event) }
	sw.Current = func(ctx context.Context, event string) (bool, error) {
		reason, e := f1SupersessionReason(ctx, l, event)
		return reason == "", e
	}
	batch, e := sw.SweepReadings(ctx, product, a["--project"], readings, after)
	if e != nil {
		return nil, e
	}
	recorded, e := sw.RecordAll(ctx, l, batch)
	if e != nil {
		return nil, e
	}
	return map[string]any{"read": recorded.Read, "recorded": recorded.Recorded, "queued": recorded.Queued, "gaps": recorded.Gaps, "readingsNext": batch.ReadingsNext, "readingsTotal": batch.ReadingsTotal, "limits": batch.Limits}, nil
}
