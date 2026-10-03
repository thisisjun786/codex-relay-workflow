package reception

import (
	"context"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Reader's policy is injected by the CLI. No library reads a host-record or environment path.
type Reader struct{ Policy registry.RolePolicy }

func (r Reader) Read(ctx context.Context, s *store.Store, receiver string, packet, observation, ledger any) (Obj, error) {
	region := Get(packet, "envelope")
	role := roles[pyjson.Text(Get(region, "direction"))][1]
	own := role + "TaskId"
	record, provenance, notes := O(own, receiver), O(own, "receiver"), []string{}
	for _, k := range []string{"repository", "prNumber", "headSha", "artifactPath", "artifactDigest"} {
		if v := Get(observation, k); v != nil {
			Set(&record, k, v)
			Set(&provenance, k, "observation: "+pyjson.Text(Get(observation, "source")))
		}
	}
	answer := func() Obj { return O("record", record, "provenance", provenance, "notes", notes) }
	if s == nil {
		notes = append(notes, "the store could not be opened read-only, so nothing but the receiver's own id was read")
		return answer(), nil
	}
	fields, sources := Obj{}, Obj{}
	if e := r.readStore(ctx, s, receiver, role, region, ledger, &fields, &sources, &notes); e != nil {
		notes = append(notes, "the store could not be read: "+e.Error())
		return answer(), nil
	}
	for _, f := range fields {
		Set(&record, f.Key, f.Value)
	}
	for _, f := range sources {
		Set(&provenance, f.Key, f.Value)
	}
	return answer(), nil
}
func (r Reader) readStore(ctx context.Context, s *store.Store, receiver, role string, region, ledger any, fields, sources *Obj, notes *[]string) error {
	answer := func(k string, v any, source string) { Set(fields, k, v); Set(sources, k, source) }
	if role != "parent" && role != "child" {
		*notes = append(*notes, "the relay store keeps no relationship row for a "+role+", so a packet sent to one is read back through the supervisor channel, not here")
		return nil
	}
	held, e := s.All(ctx, "SELECT * FROM relationships WHERE "+role+"_task_id = ? ORDER BY updated_at DESC, relationship_id", receiver)
	if e != nil {
		return e
	}
	live := []store.Row{}
	for _, row := range held {
		if row.Get("status") == "active" || row.Get("status") == "paused" {
			live = append(live, row)
		}
	}
	var row store.Row
	how := "the receiver's relationship the packet named"
	if len(live) == 1 {
		row = live[0]
		how = "the receiver's one live relationship"
	} else {
		for _, one := range held {
			if one.Get("relationship_id") == Get(region, "relationId") {
				row = one
				break
			}
		}
		if row == nil && firstAssignment(region) {
			opened := []store.Row{}
			for _, one := range live {
				generation, e := s.One(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", one.Get("relationship_id"), one.Get("execution_generation"))
				if e != nil {
					return e
				}
				if generation.Get("dispatch_request_id") == Get(region, "relationId") {
					opened = append(opened, one)
				}
			}
			if len(opened) == 1 {
				row = opened[0]
				how = "the receiver's live relationship whose current generation the packet's dispatch opened"
			}
		}
	}
	if row == nil {
		*notes = append(*notes, fmt.Sprintf("the receiver holds %d relationship(s), %d live, and the packet names none of them, so no relationship was read", len(held), len(live)))
		return nil
	}
	rid := pyjson.Text(row.Get("relationship_id"))
	answer("relationId", rid, "relationships ("+how+")")
	for _, k := range []string{"parent", "child"} {
		if k != role {
			answer(k+"TaskId", row.Get(k+"_task_id"), "relationships."+k+"_task_id")
		}
	}
	answer("issue", row.Get("issue_key"), "relationships.issue_key")
	answer("relationStatus", row.Get("status"), "relationships.status")
	answer("generation", row.Get("execution_generation"), "relationships.execution_generation")
	opened, e := s.One(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, row.Get("execution_generation"))
	if e != nil {
		return e
	}
	if opened != nil {
		answer("dispatchRequestId", opened.Get("dispatch_request_id"), "generations (the current generation)")
	} else {
		*notes = append(*notes, "no generations row opened the current generation")
	}
	reg := &registry.Registry{Store: s}
	attachment, e := reg.Attachment(ctx, rid)
	if e != nil {
		return e
	}
	if attachment == nil {
		answer("relationRevision", nil, "relationship_scope: unscoped, so no link and no revision")
	} else if link := Get(attachment, "link"); link == nil {
		*notes = append(*notes, "the relationship is scoped to "+pyvalue.Str(Get(attachment, "projectKey"))+" but no execution link joins it, so its revision is unread")
	} else {
		if Get(link, "revision") == nil {
			return fmt.Errorf("IndexError: No item with that key")
		}
		status := pyjson.Text(Get(link, "status"))
		if status != "active" && status != "paused" || truth(Get(link, "supersededBy")) {
			detail := ""
			if truth(Get(link, "supersededBy")) {
				detail = ", superseded by " + pyvalue.Str(Get(link, "supersededBy"))
			}
			*notes = append(*notes, "the execution link "+pyjson.Text(Get(link, "linkId"))+" is "+status+detail+", so it no longer answers for this relationship and its revision is unread")
		} else if Get(Get(link, "lower"), "taskId") != row.Get("child_task_id") || Get(Get(link, "upper"), "taskId") != row.Get("parent_task_id") {
			*notes = append(*notes, "the execution link "+pyjson.Text(Get(link, "linkId"))+" now joins "+pyvalue.Str(Get(Get(link, "upper"), "taskId"))+" and "+pyvalue.Str(Get(Get(link, "lower"), "taskId"))+", not this relationship's tasks, so it has been handed over and its revision is unread")
		} else {
			answer("relationRevision", Get(link, "revision"), "scope_links "+pyjson.Text(Get(link, "linkId")))
		}
	}
	if e = readCriteria(ctx, s, rid, answer, notes); e != nil {
		return e
	}
	if e = r.readSettings(ctx, reg, row, answer, notes); e != nil {
		return e
	}
	start, e := tenureStart(ctx, s, rid, row.Get("execution_generation"), notes)
	if e != nil {
		return e
	}
	var dispatch any
	if start != nil {
		answer("tenureGeneration", start, "journal (the registration that began the current tenure)")
		begun, e := s.One(ctx, "SELECT dispatch_request_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, start)
		if e != nil {
			return e
		}
		if begun == nil {
			*notes = append(*notes, fmt.Sprintf("no generations row holds generation %v, which the registration that began the current tenure opened, so its dispatch is unread", start))
		} else {
			dispatch = begun.Get("dispatch_request_id")
		}
	}
	if dispatch != nil {
		answer("tenureDispatchRequestId", dispatch, "generations (the registration that began the current tenure)")
	}
	entry := Get(Get(ledger, "assignments"), rid)
	if truth(entry) && truth(Get(entry, "mode")) && dispatch != nil && equal(Get(entry, "dispatchRequestId"), dispatch) {
		source := "ledger: assignment " + pyvalue.Str(Get(entry, "messageId"))
		answer("mode", Get(entry, "mode"), source)
		answer("workflow", Get(entry, "workflow"), source)
	} else if truth(entry) {
		*notes = append(*notes, "the reception ledger's assignment for "+rid+" was accepted under dispatch "+pyvalue.Str(Get(entry, "dispatchRequestId"))+", not the one whose registration began the current tenure, so this tenure's mode and workflow are unread")
	} else if ledger != nil {
		*notes = append(*notes, "the reception ledger holds no accepted assignment for "+rid+", so the mode and the workflow are unread")
	}
	return nil
}
func tenureStart(ctx context.Context, s *store.Store, rid string, current any, notes *[]string) (any, error) {
	rows, e := s.All(ctx, "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq DESC", "relationship_tenure_reopened", rid)
	if e != nil {
		return nil, e
	}
	now, numeric := evidence.PyInt(current)
	if f, ok := current.(float64); ok {
		now, numeric = int64(f), true
	}
	var start int64
	for _, row := range rows {
		v, e := registry.DecodeJSON(pyjson.Text(row.Get("detail")))
		generation, ok := evidence.PyInt(Get(v, "executionGeneration"))
		if e != nil || !ok {
			*notes = append(*notes, "a returning registration of "+rid+" is journalled without a readable generation, so the current tenure is unread")
			return nil, nil
		}
		if !numeric {
			return nil, fmt.Errorf("the current generation %s is not a number", quote.Value(current))
		}
		if generation <= now {
			start = max(start, generation)
		}
	}
	if start == 0 {
		registered, e := s.One(ctx, "SELECT 1 FROM journal WHERE kind = ? AND subject = ? LIMIT 1", "relationship_registered", rid)
		if e != nil {
			return nil, e
		}
		if registered == nil {
			*notes = append(*notes, "the journal holds no registration of "+rid+", so the current tenure is unread")
			return nil, nil
		}
		start = 1
	}
	row, e := s.One(ctx, "SELECT MAX(execution_generation) AS start FROM generations WHERE relationship_id = ? AND execution_generation <= ? AND reason IS NULL", rid, current)
	if e != nil {
		return nil, e
	}
	counted := any(int64(1))
	if row.Get("start") != nil {
		counted = row.Get("start")
	}
	if !equal(start, counted) {
		*notes = append(*notes, fmt.Sprintf("the registration journal says the current tenure of %s began at generation %v and the generation rows say %v, so the tenure is unread", rid, start, counted))
		return nil, nil
	}
	return start, nil
}
func readCriteria(ctx context.Context, s *store.Store, rid string, answer func(string, any, string), notes *[]string) error {
	rows, e := s.All(ctx, "SELECT criterion_id, title, required, source_ref, set_digest, recorded_at FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", rid)
	if e != nil {
		return e
	}
	mode, e := s.One(ctx, "SELECT mode FROM verification_mode WHERE relationship_id = ?", rid)
	if e != nil {
		return e
	}
	if len(rows) == 0 && mode == nil {
		*notes = append(*notes, "no criteria set is registered for "+rid)
		return nil
	}
	criteria := []delivery.Criterion{}
	sources, digests := map[any]bool{}, map[any]bool{}
	for _, row := range rows {
		required, ok := evidence.PyInt(row.Get("required"))
		if !ok || required != 0 && required != 1 {
			*notes = append(*notes, "the registered criteria are not one valid set: stored required flag for "+quote.Value(row.Get("criterion_id"))+" is not 0 or 1")
			return nil
		}
		criteria = append(criteria, delivery.Criterion{ID: pyjson.Text(row.Get("criterion_id")), Title: pyjson.Text(row.Get("title")), Required: required == 1})
		sources[row.Get("source_ref")] = true
		digests[row.Get("set_digest")] = true
	}
	if len(criteria) == 0 || mode.Get("mode") != "managed" || len(sources) != 1 || len(digests) != 1 || !digests[delivery.SetDigest(criteria)] {
		*notes = append(*notes, "the registered criteria are not one valid set: criteria for "+quote.Value(rid)+" are stored in a form ensure_registered will not replace or repair")
		return nil
	}
	answer("criteriaDigest", rows[0].Get("set_digest"), "canonical_criteria (managed set)")
	return nil
}
func (r Reader) readSettings(ctx context.Context, reg *registry.Registry, row store.Row, answer func(string, any, string), notes *[]string) error {
	child, parent := pyjson.Text(row.Get("child_task_id")), pyjson.Text(row.Get("parent_task_id"))
	settings := map[string]Obj{}
	for _, task := range []string{child, parent} {
		raw, e := reg.Store.One(ctx, "SELECT settings FROM authorized_settings WHERE task_id = ?", task)
		if e != nil {
			return e
		}
		if raw == nil {
			about := ""
			if task == parent {
				about = ", the task a callback answers"
			}
			*notes = append(*notes, "no settings are recorded for "+task+about)
			continue
		}
		rawSettings := pyjson.Text(raw.Get("settings"))
		if problem := JSONSettingsDepthProblem([]byte(rawSettings)); problem != "" {
			*notes = append(*notes, "the recorded settings of "+task+" are unreadable: "+problem)
			continue
		}
		decoded, e := registry.DecodeJSON(rawSettings)
		if e != nil {
			*notes = append(*notes, "the recorded settings of "+task+" are unreadable: "+e.Error())
			continue
		}
		o, object := evidence.Object(decoded)
		if !object && truth(decoded) {
			*notes = append(*notes, "the recorded settings of "+task+" are unreadable: they are not a JSON object")
			continue
		}
		settings[task] = o
		value := func(name string) any {
			v := Get(o, name)
			if v == nil || pyvalue.TypeName(v) == "str" {
				return v
			}
			*notes = append(*notes, "the recorded "+name+" of "+task+" is "+quote.Kind(v)+", not the text a writer records, so it is unread")
			return nil
		}
		if task == child {
			model, effort := value("model"), value("reasoningEffort")
			sandbox := registry.NormalisePolicy(Get(o, "sandbox"))
			if nesting(sandbox) > 32 {
				*notes = append(*notes, "the recorded sandbox of "+task+" nests deeper than 32 levels, which no sandbox policy does, so it is unread")
				sandbox = nil
			}
			var sb any
			if sandbox != nil {
				sb = sandbox
			}
			answer("policy", O("model", model, "effort", effort, "sandbox", sb, "approval", value("approvalPolicy")), "authorized_settings["+child+"]")
		} else {
			answer("callback", O("taskId", parent, "model", value("model"), "effort", value("reasoningEffort")), "relationships.parent_task_id + authorized_settings["+parent+"]")
		}
	}
	for i, task := range []string{child, parent} {
		key := []string{"refusedPolicies", "refusedCallbackPolicies"}[i]
		roles, e := reg.Store.All(ctx, "SELECT DISTINCT role FROM scope_bindings WHERE task_id = ? AND status IN (?,?) AND superseded_by IS NULL", task, "active", "paused")
		if e != nil {
			return e
		}
		if len(roles) == 0 {
			answer(key, []any{}, "rolepolicy: "+task+" holds no bound role, so no role pair applies")
			continue
		}
		if len(roles) > 1 {
			*notes = append(*notes, task+" is bound to more than one role, so its pair is unchecked")
			continue
		}
		held, ok := settings[task]
		if !ok {
			continue
		}
		role := pyjson.Text(roles[0].Get("role"))
		if !r.Policy.Declared {
			*notes = append(*notes, "no role policy resolved for this check ("+r.Policy.Detail+"; it reads the one this store's service declares with service declare --execution-policy, else the variable), so whether the recorded pair of "+task+" is authorised for "+role+" is unchecked")
			continue
		}
		model, effort := Get(held, "model"), Get(held, "reasoningEffort")
		if model != nil && pyvalue.TypeName(model) != "str" || effort != nil && pyvalue.TypeName(effort) != "str" {
			*notes = append(*notes, "the recorded pair of "+task+" is not text, so whether it is authorised for "+role+" is unchecked")
			continue
		}
		refused := []any{}
		if finding := registry.CheckRecord(held, role, r.Policy); finding != nil {
			refused = append(refused, O("model", model, "effort", effort, "code", Get(finding, "code"), "reason", describeRole(finding)))
		}
		answer(key, refused, "rolepolicy "+r.Policy.Digest()+" for "+role)
	}
	return nil
}
func nesting(v any) int {
	o, ok := evidence.Object(v)
	if ok {
		n := 1
		for _, f := range o {
			n = max(n, 1+nesting(f.Value))
		}
		return n
	}
	if l, ok := evidence.List(v); ok {
		n := 1
		for _, x := range l {
			n = max(n, 1+nesting(x))
		}
		return n
	}
	return 0
}
func describeRole(finding Obj) string {
	if truth(Get(finding, "undeclared")) {
		return "this host's execution policy declares no such role, so its authorization cannot be checked"
	}
	if Get(finding, "citedException") != nil && !Has(finding, "recorded") {
		return "its record cites exception " + quote.Value(Get(finding, "citedException")) + ", which this policy does not authorize for that role with this pair and directory"
	}
	if Has(finding, "recorded") {
		return "its recorded authorization is " + quote.Value(Get(finding, "recorded")) + " while the policy for that role is " + quote.Value(Get(finding, "expected"))
	}
	if d := Get(finding, "detail"); d != nil {
		return pyjson.Text(d)
	}
	return "its recorded authorization does not match this policy"
}
func (r Reader) Check(ctx context.Context, s *store.Store, packet any, receiver string, observation, ledger any) (Obj, error) {
	if e := Check(packet); e != nil {
		return nil, e
	}
	reading, e := r.Read(ctx, s, receiver, packet, observation, ledger)
	if e != nil {
		return nil, e
	}
	answer, e := Reception(packet, Get(reading, "record"))
	if e != nil {
		return nil, e
	}
	for _, p := range []struct {
		k string
		v any
	}{{"recordSource", "store"}, {"receiver", receiver}, {"record", Get(reading, "record")}, {"provenance", Get(reading, "provenance")}, {"notes", Get(reading, "notes")}} {
		Set(&answer, p.k, p.v)
	}
	held, e := Ladder(ctx, s, pyjson.Text(Get(Get(reading, "record"), "relationId")), pyjson.Text(Get(Get(packet, "envelope"), "subject")), observation)
	if e != nil {
		return nil, e
	}
	Set(&answer, "handover", Get(held, "handover"))
	Set(&answer, "coordinationSync", Get(held, "coordinationSync"))
	promotions, e := UnsupportedPromotions(Get(held, "handover"))
	if e != nil {
		return nil, e
	}
	Set(&answer, "promotions", promotions)
	claims, e := Claims(Get(packet, "progression"), Get(held, "handover"))
	if e != nil {
		return nil, e
	}
	Set(&answer, "unbackedClaims", Get(claims, "unbacked"))
	Set(&answer, "unmeasurableClaims", Get(claims, "unmeasurable"))
	var repeated any
	if ledger != nil {
		repeated = Repeat(packet, Get(ledger, "answered"))
	}
	return SettleRepeat(answer, repeated), nil
}
func uniqueSorted(values []string) []string { slices.Sort(values); return slices.Compact(values) }
