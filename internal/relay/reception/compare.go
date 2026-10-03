package reception

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func Mismatch(kind, field string, expected, found any, reason string) Obj {
	return O("kind", kind, "field", field, "expected", expected, "found", found, "reason", reason)
}

type comparison struct{ problems, gaps []any }

func (c *comparison) compare(kind, field string, found, expected any, reason string) {
	if !present(expected) {
		c.gap(field, nil, found, "the record the receiver read says nothing about "+field+", so this could not be checked")
		return
	}
	if !present(found) {
		c.problem(kind, field, expected, nil, "the packet states no "+field+", and "+reason)
		return
	}
	if pyvalue.TypeName(found) != pyvalue.TypeName(expected) {
		c.gap(field, expected, found, "the record holds "+quote.Kind(expected)+" for "+field+" and the packet "+quote.Kind(found)+"; values of different shapes are not a reading of each other, so this could not be checked")
		return
	}
	if !equal(found, expected) {
		c.problem(kind, field, expected, found, reason)
	}
}
func (c *comparison) gap(field string, expected, found any, reason string) {
	c.gaps = append(c.gaps, Mismatch("unreadable", field, expected, found, reason))
}
func (c *comparison) problem(kind, field string, expected, found any, reason string) {
	c.problems = append(c.problems, Mismatch(kind, field, expected, found, reason))
}
func firstAssignment(region any) bool {
	return Get(region, "direction") == "parent_to_child" && Get(region, "purpose") == "assignment" && absent(Get(Get(region, "recipient"), "taskId"))
}
func Reception(one, record any) (Obj, error) {
	if e := Check(one); e != nil {
		return nil, e
	}
	if _, ok := evidence.Object(record); !ok {
		return nil, malformed("the receiver's own reading is an object of named values, not %s", quote.Kind(record))
	}
	region := Get(one, "envelope")
	c := comparison{problems: []any{}, gaps: []any{}}
	first := firstAssignment(region)
	direction, purpose := pyjson.Text(Get(region, "direction")), pyjson.Text(Get(region, "purpose"))
	parentChild := direction == "parent_to_child" || direction == "child_to_parent"
	req, _ := RequiredFor(direction, purpose)
	if first {
		c.compare("wrong_relation", "relationId", Get(region, "relationId"), Get(record, "dispatchRequestId"), "a first assignment names the dispatch it was sent under, and this is not the dispatch that opened the receiver's current generation")
	} else {
		c.compare("wrong_relation", "relationId", Get(region, "relationId"), Get(record, "relationId"), "a packet naming another relationship belongs to another assignment")
	}
	pair := roles[direction]
	c.compare("wrong_sender", "sender.taskId", Get(Get(region, "sender"), "taskId"), Get(record, pair[0]+"TaskId"), "the registered pair is what says who may send this, not the message's own account of itself")
	recipient := Get(record, pair[1]+"TaskId")
	if first {
		if !present(recipient) {
			c.gap("recipient.taskId", nil, Get(Get(region, "recipient"), "taskId"), "no registration answers who this first assignment created, so it cannot be taken up yet")
		}
	} else {
		c.compare("wrong_recipient", "recipient.taskId", Get(Get(region, "recipient"), "taskId"), recipient, "a packet addressed to another task is not this task's instruction")
	}
	if slices.Contains(req, "issue") || present(Get(one, "issue")) {
		c.compare("wrong_issue", "issue", Get(one, "issue"), Get(record, "issue"), "the issue binding is what makes this the assignment it claims to be")
	}
	status := Get(record, "relationStatus")
	if !present(status) {
		c.gap("relationStatus", nil, nil, "the record the receiver read does not say whether the relationship is still live, so that is unchecked")
	} else if status != "active" && status != "paused" {
		c.problem("superseded_relation", "relationStatus", "active or paused", status, "the relationship this packet belongs to is "+pyvalue.Str(status)+"; a packet for it is about an assignment that has ended or been replaced")
	}
	if !first && parentChild && (pyvalue.TypeName(Get(record, "dispatchRequestId")) != "str" || !present(Get(record, "dispatchRequestId"))) {
		c.gap("dispatchRequestId", nil, nil, "the receiver could not read which dispatch opened the current generation, so which tenure this packet belongs to is unchecked")
	}
	if !first {
		revision, found := Get(record, "relationRevision"), Get(region, "relationRevision")
		if !Has(record, "relationRevision") || revision != nil && !present(revision) {
			c.gap("relationRevision", nil, found, "the record the receiver read says nothing about the link revision, so whether this relationship has been replaced is unchecked")
		} else if revision == nil {
			if present(found) {
				c.problem("superseded_relation", "relationRevision", nil, found, "the receiver's store holds no link for this relationship, so a packet quoting a revision is about some other linkage")
			}
		} else {
			c.compare("superseded_relation", "relationRevision", found, revision, "a superseded link is preserved and not rewritten, so a message quoting the old revision is about a relationship that has been replaced")
		}
	}
	if slices.Contains(req, "generation") {
		c.compare("stale_generation", "generation", Get(one, "generation"), Get(record, "generation"), "a receipt emitted under a generation the assignment is not in is refused, so a message naming one produces work that cannot be handed back")
	} else if Get(one, "generation") != nil {
		c.compare("stale_generation", "generation", Get(one, "generation"), Get(record, "generation"), "this packet says it belongs to another generation than the current one")
	}
	policy := Get(one, "policy")
	if present(policy) && parentChild {
		n, ok := evidence.PyInt(Get(record, "tenureGeneration"))
		if !ok || n < 1 {
			c.gap("tenureGeneration", nil, nil, "the receiver could not read which registration began the current tenure, so the mode and workflow this policy is held to are unchecked")
		}
		if d := Get(record, "tenureDispatchRequestId"); pyvalue.TypeName(d) != "str" || !present(d) {
			c.gap("tenureDispatchRequestId", nil, nil, "the receiver could not read the dispatch that began the current tenure, so an assignment could not be held for it")
		}
	}
	tenure, _ := evidence.PyInt(Get(record, "tenureGeneration"))
	if !first && Has(record, "relationRevision") && Get(record, "relationRevision") == nil && Get(one, "generation") == nil && tenure != 1 && parentChild {
		c.gap("generation", nil, nil, "this unscoped relationship has had more than one tenure (or the reading cannot say), and a packet stating neither a revision nor a generation cannot be told from one sent in an earlier tenure")
	}
	if slices.Contains(req, "criteriaDigest") {
		c.compare("stale_criteria_digest", "criteriaDigest", Get(one, "criteriaDigest"), Get(record, "criteriaDigest"), "criteria judged against a digest nobody registered are judged against somebody's memory of them")
	} else if present(Get(one, "criteriaDigest")) {
		c.compare("stale_criteria_digest", "criteriaDigest", Get(one, "criteriaDigest"), Get(record, "criteriaDigest"), "this packet was written against criteria that are no longer the registered ones")
	}
	c.artifact(Get(one, "artifact"), record)
	c.callback(Get(one, "callback"), record)
	c.policy(policy, record)
	instructed := []any{}
	stated := [][2]any{}
	if present(policy) {
		stated = append(stated, [2]any{"policy.mode", Get(policy, "mode")})
	}
	if activation := Get(one, "activation"); activation != nil {
		stated = append(stated, [2]any{"activation.mode", Get(activation, "mode")})
	}
	assignment := direction == "parent_to_child" && purpose == "assignment"
	if len(stated) > 0 {
		held := Get(record, "mode")
		if !present(held) {
			if assignment {
				instructed = append(instructed, O("field", "mode", "value", stated[0][1], "source", "packet: the assignment defines the mode where none is held"))
			} else {
				c.gap("mode", nil, stated[0][1], "the receiver holds no reading of the mode its assignment gave, so a mode this packet states is unchecked")
			}
		} else {
			for _, s := range stated {
				c.compare("wrong_mode", s[0].(string), s[1], held, "the assignment this receiver accepted runs under another mode, and no later packet redefines it")
			}
		}
	}
	if present(policy) {
		workflow, held := Get(policy, "workflow"), Get(record, "workflow")
		if !present(held) {
			if assignment {
				instructed = append(instructed, O("field", "workflow", "value", workflow, "source", "packet: the assignment defines the workflow where none is held"))
			} else {
				c.gap("workflow", nil, workflow, "the receiver holds no reading of the workflow its assignment gave, so a workflow this packet states is unchecked")
			}
		} else {
			c.compare("wrong_workflow", "policy.workflow", workflow, held, "the assignment this receiver accepted runs under another workflow, and no later packet replaces it")
		}
		if !refusalsUnreadable(record, "refusedPolicies") {
			pairs, _ := evidence.List(Get(record, "refusedPolicies"))
			for _, p := range pairs {
				if Get(p, "model") == Get(policy, "model") && Get(p, "effort") == Get(policy, "effort") {
					reason := pyjson.Text(Get(p, "reason"))
					if reason == "" {
						reason = "this pair is recorded refused for this role, which is a settings answer rather than a provider failure and is not worked around with a second child"
					}
					c.problem("refused_settings", "policy.model/effort", Get(record, "policy"), policy, reason)
					break
				}
			}
		}
		if refusalsUnreadable(record, "refusedPolicies") {
			c.gap("policy", nil, policy, "the recorded refusals are not a list of model and effort pairs, so whether this pair is authorised for this role is unchecked")
		} else if !Has(record, "refusedPolicies") {
			c.gap("policy", nil, policy, "the receiver read no settings record, so whether this pair is authorised for this role is unchecked")
		}
	}
	disposition := "accepted"
	if len(c.problems) > 0 {
		disposition = "refused"
	} else if len(c.gaps) > 0 {
		disposition = "unavailable"
	}
	return O("version", "relay-packet/1", "disposition", disposition, "messageId", Get(region, "messageId"), "purpose", purpose, "mismatches", c.problems, "gaps", c.gaps, "instructed", instructed), nil
}
func (c *comparison) artifact(artifact, record any) {
	if !truth(artifact) {
		return
	}
	if Get(artifact, "kind") == "pull_request" {
		c.compare("stale_head", "artifact.repository", Get(artifact, "repository"), Get(record, "repository"), "the same number on two projects is two different pull requests")
		c.compare("stale_head", "artifact.number", Get(artifact, "number"), Get(record, "prNumber"), "this assignment is bound to one pull request, and it is not that one")
		c.compare("stale_head", "artifact.headSha", Get(artifact, "headSha"), Get(record, "headSha"), "the candidate moved after this packet was written, so its checks, its review and its readiness are about another commit")
		return
	}
	c.compare("stale_head", "artifact.path", Get(artifact, "path"), Get(record, "artifactPath"), "a digest identifies bytes and not which deliverable they were supposed to be")
	c.compare("stale_head", "artifact.digest", Get(artifact, "digest"), Get(record, "artifactDigest"), "the deliverable's bytes are not the ones this packet names")
}
func refusalsUnreadable(record any, key string) bool {
	if !Has(record, key) {
		return false
	}
	pairs, ok := evidence.List(Get(record, key))
	if !ok {
		return true
	}
	for _, p := range pairs {
		if _, ok := evidence.Object(p); !ok {
			return true
		}
		for _, k := range []string{"model", "effort"} {
			if pyvalue.TypeName(Get(p, k)) != "str" || !present(Get(p, k)) {
				return true
			}
		}
	}
	return false
}
func (c *comparison) callback(stated, record any) {
	if !present(stated) {
		return
	}
	held := Get(record, "callback")
	if pyvalue.TypeName(held) != "dict" || !truth(held) {
		c.gap("callback", nil, stated, "the receiver read no record of the task it answers or the pair that task runs now, so the callback is unchecked")
		return
	}
	c.compare("wrong_callback", "callback.taskId", Get(stated, "taskId"), Get(held, "taskId"), "the answer would go to a task that is not the one this assignment reports to")
	for _, part := range []string{"model", "effort"} {
		c.compare("stale_callback", "callback."+part, Get(stated, part), Get(held, part), "the task being answered is authorised to run another pair now; a callback naming the old one is refused for its settings, not retried as a provider failure or worked around with another child")
	}
	if !Has(record, "refusedCallbackPolicies") || refusalsUnreadable(record, "refusedCallbackPolicies") {
		c.gap("callback", nil, stated, "the receiver has no readable judgement of the answered task's recorded pair against the current role policy, so whether this callback pair is still authorised is unchecked")
		return
	}
	pairs, _ := evidence.List(Get(record, "refusedCallbackPolicies"))
	for _, p := range pairs {
		if Get(p, "model") == Get(stated, "model") && Get(p, "effort") == Get(stated, "effort") {
			reason := pyjson.Text(Get(p, "reason"))
			if reason == "" {
				reason = "the current role policy has moved the answered task's role off this pair, even though its own record still holds it"
			}
			c.problem("stale_callback", "callback.model/effort", nil, O("model", Get(stated, "model"), "effort", Get(stated, "effort")), reason)
			return
		}
	}
}
func sandboxReading(v any) (string, Obj) {
	if s, ok := v.(string); ok {
		name := strings.TrimSpace(s)
		mapped := map[string]string{"danger-full-access": "dangerFullAccess", "read-only": "readOnly", "workspace-write": "workspaceWrite"}
		if k, ok := mapped[name]; ok {
			return k, nil
		}
		return name, nil
	}
	p := registry.NormalisePolicy(v)
	return pyjson.Text(Get(p, "type")), p
}
func (c *comparison) policy(stated, record any) {
	if !present(stated) {
		return
	}
	held := Get(record, "policy")
	if pyvalue.TypeName(held) != "dict" || !truth(held) {
		c.gap("policy", nil, stated, "the receiver read no recorded settings for this task, so the stated pair is unchecked")
		return
	}
	for _, part := range []string{"model", "effort"} {
		c.compare("stale_policy", "policy."+part, Get(stated, part), Get(held, part), "the task was created with another pair; a packet stating this one is about settings the receiver does not hold")
	}
	if s := Get(stated, "sandbox"); present(s) {
		recorded := Get(held, "sandbox")
		theirs, tp := sandboxReading(recorded)
		mine, mp := sandboxReading(s)
		if theirs == "" {
			c.gap("policy.sandbox", nil, s, "the receiver read no sandbox recorded for this task, so the stated one is unchecked")
		} else if mine == "" {
			c.problem("stale_policy", "policy.sandbox", recorded, s, "the stated sandbox is neither a mode nor a policy object, so it names no sandbox the task holds")
		} else {
			same := mine == theirs
			if mp != nil && tp != nil {
				same = equal(mp, tp)
			}
			if !same {
				c.problem("stale_policy", "policy.sandbox", recorded, s, "the task was created under another sandbox; acting on this packet would run under permissions its record never authorised")
			}
		}
	}
	if approval := Get(stated, "approval"); present(approval) {
		c.compare("stale_policy", "policy.approval", approval, Get(held, "approval"), "the task was created under another approval policy; acting on this packet would run under one its record never authorised")
	}
}
