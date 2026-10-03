package faults

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

var f2Names = []string{"fault-fail", "fault-adopt", "fault-move", "fault-update"}

func executeF2(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	switch name {
	case "fault-fail":
		return l.Fail(ctx, a["--publication"], a["--claim-token"], a["--error"], a["--ended"] != "")
	case "fault-adopt", "fault-move", "fault-update":
		return f2Write(ctx, l, name, a)
	}
	return nil, fmt.Errorf("unknown fault command %q", name)
}
func f2Fail(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	id := a["--publication"]
	stamp, moment := l.Clock.ISO(), l.Clock.Now()
	var state string
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no publication %q", id)
		}
		state = r.Text("state")
		if state != "claimed" && state != "issued" {
			return fmt.Errorf("fault_not_claimable: only a claimed or issued publication fails; this one is %s", state)
		}
		if r.Text("claim_token") != a["--claim-token"] {
			return fmt.Errorf("fault_claim_stale: this claim token is not the current one")
		}
		if state == "issued" {
			state = "uncertain"
			if _, e = l.exec(ctx, "UPDATE fault_publications SET state=?,last_error=?,claim_token=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE publication_id=?", state, a["--error"], stamp, id); e != nil {
				return e
			}
			ended := 0
			var endedAt any
			if _, ok := a["--ended"]; ok {
				ended = 1
				endedAt = stamp
			}
			if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='uncertain',error=?,ended=?,ended_at=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", a["--error"], ended, endedAt, id); e != nil {
				return e
			}
			return f2Notify(ctx, l, r, "uncertain", stamp)
		}
		backoff := float64(30)
		for i := int64(1); i < integer(r, "attempts"); i++ {
			backoff *= 2
			if backoff >= 900 {
				backoff = 900
				break
			}
		}
		var next any = moment + backoff
		if integer(r, "attempts") >= 8 {
			state = "failed"
			next = nil
		} else {
			state = "pending"
		}
		if _, e = l.exec(ctx, "UPDATE fault_publications SET state=?,last_error=?,next_attempt_at=?,claim_token=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE publication_id=?", state, a["--error"], next, stamp, id); e != nil {
			return e
		}
		if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='failed_before_issue',error=?,ended=1,ended_at=? WHERE attempt_id=(SELECT MAX(attempt_id) FROM fault_publication_attempts WHERE publication_id=?)", a["--error"], stamp, id); e != nil {
			return e
		}
		if state == "failed" {
			return f2Notify(ctx, l, r, "failed", stamp)
		}
		return nil
	})
	detail := ""
	if state == "uncertain" {
		detail = "an issued write may have landed, so it is uncertain rather than retried"
	}
	return map[string]any{"publicationId": id, "state": state, "error": a["--error"], "detail": detail}, err
}
func f2Write(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	id := a["--fault"]
	var scope map[string]any
	var value any
	if name == "fault-update" {
		op := a["--op"]
		if op != "set_project" && op != "reopen" && op != "add_relation" && op != "add_label" {
			return nil, fmt.Errorf("fault_observation_malformed: op %s is not one of set_project, reopen, add_relation, add_label", quote.Value(op))
		}
		if raw, ok := a["--value"]; ok {
			var e error
			value, e = f2JSON(raw, "value")
			if e != nil {
				return nil, e
			}
		}
		switch op {
		case "set_project":
			if !named(value) {
				return nil, fmt.Errorf("fault_observation_malformed: set_project names a project")
			}
		case "reopen":
			if value != nil {
				return nil, fmt.Errorf("fault_observation_malformed: reopen takes no value")
			}
		case "add_relation":
			m, ok := value.(map[string]any)
			if !ok || !named(m["type"]) || !named(m["issue"]) {
				return nil, fmt.Errorf("fault_observation_malformed: add_relation is {type, issue}")
			}
		case "add_label":
			if !named(value) {
				return nil, fmt.Errorf("fault_observation_malformed: add_label names a label")
			}
		}
	} else {
		v, e := f2JSON(a["--scope"], "scope")
		if e != nil {
			return nil, e
		}
		var ok bool
		scope, ok = v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("fault_observation_malformed: scope is an object")
		}
		for key, v := range scope {
			if (key == "workspace" || key == "projectKey") && v != nil && !named(v) {
				return nil, fmt.Errorf("fault_observation_malformed: scope.%s is a non-blank string", key)
			}
			switch v.(type) {
			case nil, string, json.Number, bool:
			default:
				return nil, fmt.Errorf("fault_observation_malformed: scope.%s is not a JSON scalar", key)
			}
			if v == nil {
				delete(scope, key)
			}
		}
		for _, key := range []string{"workspace", "projectKey"} {
			if v, ok := scope[key]; ok && !named(v) {
				return nil, fmt.Errorf("fault_observation_malformed: scope.%s is a non-blank string", key)
			}
		}
		if name == "fault-adopt" {
			if strings.TrimSpace(a["--external-ref"]) == "" {
				return nil, fmt.Errorf("fault_observation_malformed: an adoption names the issue it adopts")
			}
			if !named(scope["projectKey"]) {
				return nil, fmt.Errorf("fault_observation_malformed: an adoption's scope carries the owner's projectKey")
			}
		}
	}
	var answer any
	e := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		alias, e := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", id)
		if e != nil {
			return e
		}
		if alias != nil {
			id = alias.Text("fault_id")
		}
		fault, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", id)
		if e != nil {
			return e
		}
		if fault == nil {
			return fmt.Errorf("fault_unknown: no fault %q", a["--fault"])
		}
		stamp := l.Clock.ISO()
		switch name {
		case "fault-move":
			answer, e = f2Move(ctx, l, fault, scope, stamp)
		case "fault-adopt":
			answer, e = f2Adopt(ctx, l, fault, a["--external-ref"], scope, stamp)
		case "fault-update":
			answer, e = f2Update(ctx, l, fault, a["--op"], value, stamp)
		}
		return e
	})
	return answer, e
}
func f2JSON(raw, label string) (any, error) {
	if path, ok := strings.CutPrefix(raw, "@"); ok {
		data, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		raw = string(data)
	}
	v, e := loads(raw)
	if e != nil {
		return nil, fmt.Errorf("fault_observation_malformed: the %s is not readable JSON: %v", label, e)
	}
	return v, nil
}
func f2Move(ctx context.Context, l *Ledger, f row, scope map[string]any, stamp string) (any, error) {
	id := f.Text("fault_id")
	old := loadsMap(f.Text("scope"))
	current, _ := old["workspace"].(string)
	wanted, _ := scope["workspace"].(string)
	var alias any
	if wanted != current {
		if current != "unassigned" || wanted == "" || wanted == "unassigned" {
			return nil, fmt.Errorf("fault_scope_conflict: a fault moves inside its workspace, or out of unassigned; this one is in %s and was asked to move to %s", quote.Value(old["workspace"]), quote.Value(scope["workspace"]))
		}
		signature := loadsMap(f.Text("signature"))
		candidate := FaultIDInWorkspace(f.Text("product"), f.Text("fault_class"), signature, wanted)
		alias = candidate
		existing, e := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", candidate)
		if e != nil {
			return nil, e
		}
		resolved := candidate
		if existing != nil {
			resolved = existing.Text("fault_id")
		}
		other, e := l.one(ctx, "SELECT 1 FROM fault_ledger WHERE fault_id=?", resolved)
		if e != nil {
			return nil, e
		}
		if resolved != id && other != nil {
			return nil, fmt.Errorf("fault_scope_conflict: workspace '%s' already records this failure as fault %s; two records never stand for one failure in one workspace", wanted, resolved)
		}
		if candidate != id && existing == nil {
			_, e = l.exec(ctx, "INSERT INTO fault_aliases(alias_id,fault_id,created_at) VALUES(?,?,?)", candidate, id, stamp)
			if e != nil {
				return nil, e
			}
		}
	}
	changed := dumps(old, false) != dumps(scope, false)
	repointed := 0
	if changed {
		var e error
		repointed, e = l.rescopeCount(ctx, f, scope, stamp)
		if e != nil {
			return nil, e
		}
	}
	fresh, e := l.one(ctx, "SELECT scope_key FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return nil, e
	}
	_, base, _, where := dRepointFaultQuery(id)
	pending, e := l.one(ctx, "SELECT COUNT(*) AS n"+base, where...)
	if e != nil {
		return nil, e
	}
	return map[string]any{"faultId": id, "scopeKey": fresh.Text("scope_key"), "moved": changed, "repointed": repointed, "repointPending": integer(pending, "n"), "alias": alias}, nil
}
func f2Adopt(ctx context.Context, l *Ledger, f row, ref string, scope map[string]any, stamp string) (any, error) {
	id := f.Text("fault_id")
	if f.Get("external_ref") != nil {
		if f.Text("external_ref") != ref {
			return nil, fmt.Errorf("fault_adopt_conflict: this fault already owns '%s'", f.Text("external_ref"))
		}
		return f2AdoptionAnswer(ctx, l, id, ref, nil)
	}
	stored, e := l.one(ctx, "SELECT external_ref FROM fault_adoptions WHERE fault_id=?", id)
	if e != nil {
		return nil, e
	}
	if stored != nil && stored.Text("external_ref") != ref {
		return nil, fmt.Errorf("fault_adopt_conflict: this fault already adopts '%s'", stored.Text("external_ref"))
	}
	create, e := l.one(ctx, "SELECT * FROM fault_publications WHERE fault_id=? AND kind='open_record'", id)
	if e != nil {
		return nil, e
	}
	if create != nil {
		switch create.Text("state") {
		case "issued", "uncertain", "confirmed":
			return nil, fmt.Errorf("fault_adopt_conflict: this fault's create is %s; reconcile it first, or two records would stand for one fault", create.Text("state"))
		}
	}
	if _, e = f2Move(ctx, l, f, scope, stamp); e != nil {
		return nil, e
	}
	if create != nil {
		switch create.Text("state") {
		case "pending", "failed", "claimed":
			attempts := integer(create, "attempts")
			if create.Text("state") == "claimed" {
				attempt, e := l.one(ctx, "SELECT attempt_id FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 1", create.Text("publication_id"))
				if e != nil {
					return nil, e
				}
				if attempt != nil {
					refKey := fmt.Sprintf("%s:%d", create.Text("publication_id"), integer(attempt, "attempt_id"))
					if _, e = l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=? AND kind=? AND ref=?", f.Text("product"), openRecord, refKey); e != nil {
						return nil, e
					}
					if _, e = l.exec(ctx, "UPDATE fault_publication_attempts SET outcome='cancelled',ended=1,ended_at=? WHERE attempt_id=?", stamp, integer(attempt, "attempt_id")); e != nil {
						return nil, e
					}
				}
				attempts--
			}
			if _, e = l.exec(ctx, "UPDATE fault_publications SET state='cancelled',claim_token=NULL,lease_owner=NULL,lease_until=NULL,last_error=?,updated_at=?,attempts=? WHERE publication_id=?", "adopted "+ref, stamp, attempts, create.Text("publication_id")); e != nil {
				return nil, e
			}
		}
	}
	if _, e = l.exec(ctx, "INSERT INTO fault_adoptions(fault_id,external_ref,scope,state,created_at,updated_at) VALUES(?,?,?,'pending',?,?) ON CONFLICT(fault_id) DO NOTHING", id, ref, dumps(scope, false), stamp, stamp); e != nil {
		return nil, e
	}
	var publication any
	if f.Text("state") == Open || create != nil {
		if _, e = l.exec(ctx, "UPDATE fault_ledger SET external_ref=?,updated_at=? WHERE fault_id=? AND external_ref IS NULL", ref, stamp, id); e != nil {
			return nil, e
		}
		if _, e = l.exec(ctx, "UPDATE fault_adoptions SET state='materialized',updated_at=? WHERE fault_id=?", stamp, id); e != nil {
			return nil, e
		}
		if e = l.insertPublication(ctx, id, appendComment, triggerOpen, stamp, ""); e != nil {
			return nil, e
		}
		publication = map[string]any{"publicationId": publicationID(id, appendComment, triggerOpen), "kind": appendComment, "trigger": triggerOpen, "queued": true, "awaitingTarget": false, "awaitingRecord": false, "reason": "queued"}
		fresh, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", id)
		if e != nil {
			return nil, e
		}
		target, e := l.one(ctx, "SELECT project_ref FROM fault_target_projects WHERE scope_key=? AND product=?", fresh.Text("scope_key"), fresh.Text("product"))
		if e != nil {
			return nil, e
		}
		if target == nil || target.Get("project_ref") == nil {
			e = dUnlinkOne(ctx, l, id, stamp)
		} else {
			e = dRelinkOne(ctx, l, id, target.Text("project_ref"), stamp)
		}
		if e != nil {
			return nil, e
		}
	}
	return f2AdoptionAnswer(ctx, l, id, ref, publication)
}
func f2AdoptionAnswer(ctx context.Context, l *Ledger, id, ref string, publication any) (any, error) {
	f, e := l.one(ctx, "SELECT external_ref FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return nil, e
	}
	stored, e := l.one(ctx, "SELECT state FROM fault_adoptions WHERE fault_id=?", id)
	if e != nil {
		return nil, e
	}
	var state any
	if stored != nil {
		state = stored.Get("state")
	}
	rows, e := l.Store.All(ctx, "SELECT publication_id FROM fault_publications WHERE fault_id=? AND kind='open_record' AND state='cancelled'", id)
	if e != nil {
		return nil, e
	}
	cancelled := []any{}
	for _, r := range rows {
		cancelled = append(cancelled, r.Text("publication_id"))
	}
	if f.Get("external_ref") != nil {
		ref = f.Text("external_ref")
	}
	return map[string]any{"faultId": id, "externalRef": ref, "state": state, "cancelled": cancelled, "publication": publication}, nil
}
func f2Update(ctx context.Context, l *Ledger, f row, op string, value any, stamp string) (any, error) {
	id := f.Text("fault_id")
	if op == "set_project" {
		target, e := l.one(ctx, "SELECT p.project_ref FROM fault_target_projects p WHERE p.scope_key=? AND p.product=?", f.Text("scope_key"), f.Text("product"))
		if e != nil {
			return nil, e
		}
		var wanted any
		if target != nil {
			wanted = target.Get("project_ref")
		}
		if value != wanted {
			detail := quote.Value(wanted)
			if target == nil {
				detail += ": awaiting_target"
			}
			return nil, fmt.Errorf("fault_state_conflict: set_project puts the owned issue in the project its scope targets (%s), never another; set_target() or move() changes where it belongs", detail)
		}
		if f.Get("external_ref") == nil {
			return map[string]any{"publicationId": nil, "kind": "update_record", "trigger": nil, "queued": false, "linkState": "none", "reason": "this fault owns no issue"}, nil
		}
		before, e := l.one(ctx, "SELECT publication_id FROM fault_publications WHERE fault_id=? AND kind='update_record' ORDER BY rowid DESC LIMIT 1", id)
		if e != nil {
			return nil, e
		}
		if e = dRelinkOne(ctx, l, id, value.(string), stamp); e != nil {
			return nil, e
		}
		r, e := l.one(ctx, "SELECT publication_id,trigger_key,state FROM fault_publications WHERE fault_id=? AND kind='update_record' ORDER BY rowid DESC LIMIT 1", id)
		if e != nil {
			return nil, e
		}
		if r != nil && r.Text("state") == pending && (before == nil || before.Text("publication_id") != r.Text("publication_id")) {
			return map[string]any{"publicationId": r.Text("publication_id"), "kind": "update_record", "trigger": r.Text("trigger_key"), "queued": true, "awaitingTarget": false, "awaitingRecord": false, "reason": "queued"}, nil
		}
		link, e := l.one(ctx, "SELECT state FROM fault_links WHERE fault_id=?", id)
		if e != nil {
			return nil, e
		}
		state := "unlinked"
		if link != nil {
			state = link.Text("state")
		}
		return map[string]any{"publicationId": nil, "kind": "update_record", "trigger": nil, "queued": false, "linkState": state, "reason": "nothing new was queued: the issue already reads back in that project, or a write to another project may still land and its readback decides"}, nil
	}
	if f.Get("external_ref") == nil {
		return nil, nil
	}
	trigger := fmt.Sprintf("update:%s:%s:%d", op, pyvalue.SHA256Hex(dumps(value, true))[:12], integer(f, "cycle"))
	publication := publicationID(id, "update_record", trigger)
	prior, e := l.one(ctx, "SELECT state FROM fault_publications WHERE publication_id=?", publication)
	if e != nil {
		return nil, e
	}
	if prior != nil && prior.Text("state") != "cancelled" {
		return map[string]any{"publicationId": publication, "kind": "update_record", "trigger": trigger, "queued": false, "awaitingTarget": false, "awaitingRecord": false, "reason": "this reason was already queued"}, nil
	}
	if e = l.insertPublication(ctx, id, "update_record", trigger, stamp, ""); e != nil {
		return nil, e
	}
	payload := dumps(map[string]any{"op": op, "value": value}, false)
	if _, e = l.exec(ctx, "UPDATE fault_publication_payloads SET payload=?,updated_at=? WHERE publication_id=?", payload, stamp, publication); e != nil {
		return nil, e
	}
	reason := "queued"
	if prior != nil {
		reason = "revived"
	}
	return map[string]any{"publicationId": publication, "kind": "update_record", "trigger": trigger, "queued": true, "awaitingTarget": false, "awaitingRecord": false, "reason": reason}, nil
}
func f2Notify(ctx context.Context, l *Ledger, r row, state, stamp string) error {
	id := r.Text("fault_id")
	fault, e := l.one(ctx, "SELECT product FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return e
	}
	reason := fmt.Sprintf("write:%s:%s", r.Text("publication_id"), state)
	notification := pyvalue.SHA256Hex(fmt.Sprintf("%s|decision|%s", id, reason))[:idWidth]
	_, e = l.exec(ctx, "INSERT INTO fault_notifications(notification_id,fault_id,product,kind,reason,cycle,ref,state,created_at,updated_at) VALUES(?,?,?,'decision',?,?,NULL,'pending',?,?) ON CONFLICT(notification_id) DO UPDATE SET state=excluded.state,last_error=NULL,updated_at=excluded.updated_at WHERE fault_notifications.state='withdrawn'", notification, id, fault.Text("product"), reason, integer(r, "cycle"), stamp, stamp)
	return e
}
