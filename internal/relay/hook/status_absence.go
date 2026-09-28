package hook

import (
	"fmt"
	"sort"
	"strings"
)

const (
	established  = "established"
	ruledOut     = "ruled_out"
	notRuledOut  = "not_ruled_out"
	notEvaluated = "not_evaluated"
)

type causeResult struct{ standing, detail string }

var causeOrder = []string{"not_registered", "record_path_unidentified", "adapter_cannot_run", "settings_absent", "settings_unusable", "recorded_on_another_path", "journalling_off", "policy_records_only_faults", "nothing_recorded", "records_found"}

func decideAbsence(o map[string]any) map[string]any {
	r := map[string]causeResult{}
	r["not_registered"] = causeNotRegistered(o)
	if r["not_registered"].standing == ruledOut {
		r["record_path_unidentified"] = causeRecordPath(o)
	} else {
		r["record_path_unidentified"] = causeResult{notEvaluated, "not asked, because not_registered would have to be ruled out first for this question to mean anything"}
	}
	r["adapter_cannot_run"] = causeAdapter(o)
	r["settings_absent"] = causeSettingsAbsent(o)
	r["settings_unusable"] = causeSettingsUnusable(o)
	if r["not_registered"].standing == ruledOut {
		r["recorded_on_another_path"] = causeAnotherPath(o)
		r["nothing_recorded"] = causeNothing(o)
	} else {
		d := "not asked, because not_registered would have to be ruled out first for this question to mean anything"
		r["recorded_on_another_path"] = causeResult{notEvaluated, d}
		r["nothing_recorded"] = causeResult{notEvaluated, d}
	}
	r["journalling_off"] = causeJournalOff(o)
	r["policy_records_only_faults"] = causeFaultsOnly(o)
	deps := []string{"not_registered", "record_path_unidentified", "adapter_cannot_run", "settings_absent", "settings_unusable", "journalling_off", "recorded_on_another_path", "policy_records_only_faults", "nothing_recorded"}
	blocked := []string{}
	for _, d := range deps {
		if r[d].standing != ruledOut && r[d].standing != notEvaluated {
			blocked = append(blocked, d)
		}
	}
	if len(blocked) > 0 {
		r["records_found"] = causeResult{notEvaluated, "not asked, because " + strings.Join(blocked, ", ") + " would have to be ruled out first for this question to mean anything"}
	} else {
		r["records_found"] = causeRecordsFound(o)
	}
	candidates, ruled, notEval := []any{}, []any{}, []any{}
	establishedNames, unsettled := []string{}, []string{}
	for _, name := range causeOrder {
		one := r[name]
		entry := map[string]any{"cause": name, "standing": one.standing, "detail": one.detail}
		switch one.standing {
		case established:
			candidates = append(candidates, entry)
			establishedNames = append(establishedNames, name)
		case notRuledOut:
			candidates = append(candidates, entry)
			unsettled = append(unsettled, name)
		case ruledOut:
			ruled = append(ruled, entry)
		case notEvaluated:
			notEval = append(notEval, entry)
		}
	}
	value, evidence := "cause_unreadable", "no rule answered this absence, which is reported as an unsettled cause rather than as any particular one"
	if len(unsettled) > 0 {
		value = "cause_unreadable"
		evidence = "the cause was not settled: " + strings.Join(unsettled, ", ") + " could not be ruled out"
		if len(establishedNames) > 0 {
			evidence += ", beside established " + strings.Join(establishedNames, ", ")
		}
		evidence += ". Every candidate is carried rather than one of them chosen"
	} else if len(establishedNames) == 1 {
		value = establishedNames[0]
		evidence = r[value].detail
	} else if len(establishedNames) > 1 {
		value = "several_causes"
		parts := []string{}
		for _, n := range establishedNames {
			parts = append(parts, n+" ("+r[n].detail+")")
		}
		evidence = "more than one cause is established and each needs its own repair: " + strings.Join(parts, "; ")
	}
	return map[string]any{"value": value, "evidence": evidence, "candidates": candidates, "ruledOut": ruled, "notEvaluated": notEval, "note": "Registered, startable and observed to have recorded are separate claims. What no answer here establishes: a journal write that fails removes what it left and cannot record its own failure, so a journal holding nothing is not proof that the hook never ran. That is why the answer is named for the journal and not for the hook."}
}
func causeNotRegistered(o map[string]any) causeResult {
	records := asInt(o["unregisteredRecords"])
	if !asBool(o["registrationReadable"]) {
		if records > 0 {
			return causeResult{ruledOut, fmt.Sprintf("%d record(s) this hook wrote are under the journal this command settled on, so something invoked this adapter", records)}
		}
		return causeResult{notRuledOut, "the hook file could not be read, so whether this adapter is registered for the event was not established"}
	}
	n := asInt(o["adapterRegistrations"])
	if n > 0 {
		return causeResult{ruledOut, fmt.Sprintf("%d registration(s) in the hook file run this adapter", n)}
	}
	if !asBool(o["registrationReadHere"]) {
		if records > 0 {
			return causeResult{ruledOut, fmt.Sprintf("%d record(s) this hook wrote are under the journal this command settled on, so something invoked this adapter", records)}
		}
		if asBool(o["registrationElsewhere"]) {
			return causeResult{notRuledOut, "these settings record an owner whose registration lives in a package manifest rather than in this hook file, and this command does not read that manifest, so an empty hook file establishes nothing about whether this adapter is registered"}
		}
		return causeResult{notRuledOut, "nothing established who owns this registration or where it lives, so the hook file is not established as the place it would be and an empty one establishes nothing about whether this adapter is registered"}
	}
	if records > 0 {
		return causeResult{established, fmt.Sprintf("the hook file was read and registers this adapter for nothing, so nothing invokes it now. The %d record(s) are what an earlier registration left", records)}
	}
	return causeResult{established, "the hook file was read and registers this adapter for nothing, so nothing on this host invokes it and no further record of one can be written"}
}
func causeRecordPath(o map[string]any) causeResult {
	if asBool(o["relativeSettings"]) {
		return causeResult{established, "a registration spells its settings with a relative path, which the hook resolves against each session's own workspace, so no file reachable from here answers for it"}
	}
	if asInt(o["silentRegistrations"]) > 0 {
		return causeResult{established, "a registration names no settings, so the hook resolves its own at every Stop; the path this command would resolve is not established to be the one the host resolves"}
	}
	return causeResult{ruledOut, "every registration names an absolute settings file"}
}
func causeAdapter(o map[string]any) causeResult {
	p := mapSlice(o["startProbes"])
	if len(p) == 0 {
		if asBool(o["registrationReadHere"]) {
			return causeResult{notEvaluated, "no registration named a command here for this question to be about"}
		}
		return causeResult{notRuledOut, "no probe of a registered command was made"}
	}
	blocked, unjudged := 0, []string{}
	for _, x := range p {
		a, i := fmt.Sprint(x["adapter"]), fmt.Sprint(x["interpreter"])
		bad := a == absent || a == unreadable || a == "not_started" || a == "below_supported_python" || i == absent || i == unreadable || i == "not_started" || i == "below_supported_python"
		if bad {
			blocked++
		} else if a != present || i != present {
			unjudged = append(unjudged, a, i)
		}
	}
	if blocked > 0 {
		return causeResult{established, fmt.Sprintf("%d of %d registrations name an adapter or an interpreter the host cannot start", blocked, len(p))}
	}
	if len(unjudged) > 0 {
		return causeResult{notRuledOut, "whether the host can start a registered command was not established: " + strings.Join(uniqueStrings(unjudged), ", ")}
	}
	return causeResult{ruledOut, "no registered adapter or interpreter is in a state that stops the host starting it"}
}
func entriesForSettings(o map[string]any) []map[string]any {
	e := mapSlice(o["namedJournals"])
	if len(e) == 0 && asBool(o["registrationElsewhere"]) {
		if s, ok := o["settledSettings"].(map[string]any); ok && fmt.Sprint(s["settings"]) != "" {
			return []map[string]any{s}
		}
	}
	return e
}
func causeSettingsAbsent(o map[string]any) causeResult {
	e := entriesForSettings(o)
	if len(e) == 0 {
		return causeResult{notEvaluated, "no registration named a settings file this command could read"}
	}
	for _, x := range e {
		if x["settingsState"] == absent || x["settingsState"] == "config_absent" {
			return causeResult{established, "these settings files are established absent, so whatever reads them is told nothing about where to record and keeps no journal: " + fmt.Sprint(x["settings"])}
		}
	}
	for _, x := range e {
		if x["settingsState"] == accessError || x["settingsState"] == "config_unreachable" {
			return causeResult{notRuledOut, "a settings file could not be reached from here, which does not establish that the hook cannot read it"}
		}
	}
	return causeResult{ruledOut, "every settings file the registrations name exists"}
}
func causeSettingsUnusable(o map[string]any) causeResult {
	e := entriesForSettings(o)
	if len(e) == 0 {
		return causeResult{notEvaluated, "no registration named a settings file this command could read"}
	}
	for _, x := range e {
		state := fmt.Sprint(x["settingsState"])
		if state != "config_absent" && state != absent && state != "config_unreachable" && state != accessError && !asBool(x["usable"]) {
			detail := "these settings files are ones this hook's own reader rejects (" + fmt.Sprint(x["settings"]) + "), so every invocation that reads them releases without recording"
			if asBool(o["registrationElsewhere"]) {
				detail += ". Whether anything reads them was not established here: the registration these settings record lives in a package manifest this command does not open"
			}
			return causeResult{established, detail}
		}
	}
	for _, x := range e {
		if x["settingsState"] == "config_unreachable" || x["settingsState"] == accessError {
			return causeResult{notRuledOut, "a settings file could not be reached from here, so whether its contents are usable was never established"}
		}
	}
	return causeResult{ruledOut, "every settings file the registrations name reads back usable"}
}
func journalSets(o map[string]any) (holding, empty, off, unread []map[string]any) {
	for _, x := range mapSlice(o["namedJournals"]) {
		if !asBool(x["usable"]) || x["startable"] != true {
			continue
		}
		switch x["recordsAnswer"] {
		case "counted":
			if asInt(x["records"]) > 0 {
				holding = append(holding, x)
			} else {
				empty = append(empty, x)
			}
		case "no_records_kept":
			off = append(off, x)
		default:
			unread = append(unread, x)
		}
	}
	return
}
func causeAnotherPath(o map[string]any) causeResult {
	h, e, _, u := journalSets(o)
	if len(h) > 0 && len(e) > 0 {
		return causeResult{established, "this hook has recorded into one named journal while another holds nothing"}
	}
	distinct := map[string]bool{}
	for _, x := range append(append([]map[string]any{}, h...), append(e, u...)...) {
		distinct[fmt.Sprint(x["journalRoot"])] = true
	}
	if len(u) > 0 && len(distinct) > 1 {
		return causeResult{notRuledOut, "named journals could not all be listed, so whether they disagree about holding records was not settled"}
	}
	return causeResult{ruledOut, "no two named journals disagree about holding records"}
}
func causeJournalOff(o map[string]any) causeResult {
	e := entriesForSettings(o)
	if len(e) == 0 {
		return causeResult{notEvaluated, "no registration named a settings file this command could read, so there is no journal policy here for this question to be about"}
	}
	for _, x := range e {
		if asBool(x["usable"]) && x["recordsAnswer"] == "no_records_kept" {
			return causeResult{established, "these registrations keep no journal, so they record nothing about their own invocations by configuration"}
		}
	}
	return causeResult{ruledOut, "every registration with usable settings keeps a journal"}
}
func causeFaultsOnly(o map[string]any) causeResult {
	e := entriesForSettings(o)
	if len(e) == 0 {
		return causeResult{notEvaluated, "no registration named a settings file this command could read, so there is no journal policy here for this question to be about"}
	}
	for _, x := range e {
		if asBool(x["usable"]) && asBool(x["faultsOnly"]) && x["recordsAnswer"] == "counted" && asInt(x["records"]) == 0 {
			return causeResult{notRuledOut, "these settings record only invocations that faulted, so an empty journal is equally what a hook that fired and never faulted leaves behind"}
		}
	}
	return causeResult{ruledOut, "no named settings record only faults over an empty journal"}
}
func causeNothing(o map[string]any) causeResult {
	h, e, _, u := journalSets(o)
	emptyRoots := map[string]bool{}
	for _, x := range e {
		emptyRoots[fmt.Sprint(x["journalRoot"])] = true
	}
	for _, x := range mapSlice(o["namedJournals"]) {
		if asBool(x["usable"]) && x["startable"] == nil && x["recordsAnswer"] != "no_records_kept" && !emptyRoots[fmt.Sprint(x["journalRoot"])] {
			return causeResult{notRuledOut, "a registration whose startability was never established names a journal, so whether its journal counts toward this question was not settled either"}
		}
	}
	if len(h) > 0 {
		return causeResult{ruledOut, "a named journal holds records this hook wrote"}
	}
	if len(u) > 0 {
		return causeResult{notRuledOut, "a named journal could not be listed"}
	}
	if len(e) > 0 {
		allFaults := true
		for _, x := range e {
			allFaults = allFaults && asBool(x["faultsOnly"])
		}
		if allFaults {
			return causeResult{notRuledOut, "every journal that was read keeps only faults, so an empty one does not establish that nothing was recorded"}
		}
		return causeResult{established, "every journal these registrations name was read and holds no record this hook wrote"}
	}
	return causeResult{ruledOut, "no journal was read for this question"}
}
func causeRecordsFound(o map[string]any) causeResult {
	h, e, off, u := journalSets(o)
	if len(h) > 0 && len(e) == 0 && len(off) == 0 && len(u) == 0 {
		return causeResult{established, "every journal these registrations name holds records this hook wrote, so there is no absence to explain"}
	}
	if len(h)+len(e)+len(off)+len(u) == 0 && asInt(o["unregisteredRecords"]) > 0 {
		return causeResult{established, "the journal this command settled on holds records this hook wrote and no registration here names another, so there is no absence to explain"}
	}
	return causeResult{ruledOut, "a journal these registrations name holds no record"}
}
func asBool(v any) bool { b, _ := v.(bool); return b }
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
func mapSlice(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := []map[string]any{}
		for _, v := range x {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
func uniqueStrings(v []string) []string {
	m := map[string]bool{}
	for _, x := range v {
		m[x] = true
	}
	out := []string{}
	for x := range m {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
