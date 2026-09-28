package routing

// Obligations keeps queued history but drops open work contradicted by a new placement.
func Obligations(decision, existing Object, cause any, labels []any) []any {
	labelled := (decision["disposition"] == "new_issue" || decision["disposition"] == "follow_up") && decision["owner"] == nil
	owed := []any{}
	for _, x := range list(object(existing["target"])["obligations"]) {
		o := object(x)
		contradicted := false
		switch o["kind"] {
		case "add_label":
			contradicted = !labelled
		case "reopen":
			contradicted = decision["disposition"] != "reopen"
		default:
			contradicted = o["toIssue"] != nil && !contains(decision["relate"], o["toIssue"])
		}
		if o["state"] != "open" || !contradicted {
			owed = append(owed, o)
		}
	}
	wanted := []any{}
	makeOwed := func(kind string, issue, fault, label any) Object {
		return Object{"kind": kind, "toIssue": issue, "toFault": fault, "label": label, "state": "open"}
	}
	if decision["disposition"] == "reopen" {
		wanted = append(wanted, makeOwed("reopen", nil, nil, nil))
	}
	if labelled {
		for _, label := range labels {
			wanted = append(wanted, makeOwed("add_label", nil, nil, label))
		}
	}
	for _, issue := range list(decision["relate"]) {
		wanted = append(wanted, makeOwed("add_relation", issue, nil, nil))
	}
	if cause != nil {
		wanted = append(wanted, makeOwed("add_relation", nil, cause, nil))
	}
	for _, x := range wanted {
		o := object(x)
		found := false
		for _, y := range owed {
			p := object(y)
			if o["kind"] == p["kind"] && o["toIssue"] == p["toIssue"] && o["toFault"] == p["toFault"] && o["label"] == p["label"] {
				found = true
				break
			}
		}
		if !found {
			owed = append(owed, o)
		}
	}
	return owed
}

// Attention returns exactly one decision, with a missing link preceding a cause claim.
func Attention(snapshot Object) any {
	if snapshot["stage"] == "pending_classification" {
		return "awaiting_classification"
	}
	if snapshot["stage"] == "held" {
		return "held_" + text(snapshot["hold"])
	}
	active := snapshot["state"] == "observed" || snapshot["state"] == "open" || snapshot["state"] == "fix_pending"
	if snapshot["disposition"] == "project_proposal" {
		if snapshot["hold"] != nil {
			return "held_" + text(snapshot["hold"])
		}
		if snapshot["stage"] == "filed" && snapshot["project"] == nil && active {
			return "project_proposed"
		}
		return nil
	}
	if snapshot["linkState"] == "unlinked" {
		return "link_incomplete"
	}
	if len(object(snapshot["unverifiedCause"])) > 0 {
		return "cause_unverified"
	}
	if snapshot["disposition"] == "completion_mismatch" && active {
		return "completion_mismatch_open"
	}
	return nil
}
