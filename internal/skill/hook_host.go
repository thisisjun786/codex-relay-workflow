package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func recordTag(name string) [2]string {
	stem := strings.TrimSuffix(name, path.Ext(name))
	for _, prefix := range []string{"host-observation-", "host-capability-"} {
		if strings.HasPrefix(stem, prefix) {
			host, version, _ := strings.Cut(strings.TrimPrefix(stem, prefix), "-")
			if host != "" && version != "" {
				return [2]string{host, version}
			}
		}
	}
	return [2]string{}
}

func hostObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

func hostStated(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimFunc(s, isSpace) != ""
}

func hostDifference(left, right []string) []string {
	result := []string{}
	for _, value := range left {
		if !slices.Contains(right, value) && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return result
}

func hostList(value any) ([]any, error) {
	if !pyvalue.Truthy(value) {
		return nil, nil
	}
	switch list := value.(type) {
	case []any:
		return list, nil
	case string:
		items := make([]any, 0, len(list))
		for _, char := range list {
			items = append(items, string(char))
		}
		return items, nil
	case map[string]any:
		items := make([]any, 0, len(list))
		for key := range list {
			items = append(items, key)
		}
		return items, nil
	default:
		return nil, pythonNotIterable(value, false)
	}
}

// hostSorted is sorted(value or []). Sorting needs only '<', so a list or an
// object in the list is refused only where '<' refuses it; hashing waits for
// the set() calls in hostFieldProblems.
func hostSorted(value any) ([]any, error) {
	items, err := hostList(value)
	if err != nil {
		return nil, err
	}
	return pySorted(items)
}

// hostHashable is set(values) reaching its first element that cannot be hashed.
func hostHashable(values []any) error {
	for _, value := range values {
		switch value.(type) {
		case []any, map[string]any:
			return &evidence.PythonError{Class: "TypeError", Detail: "unhashable type: '" + pyvalue.TypeName(value) + "'"}
		}
	}
	return nil
}

// hostSetDifference is sorted(set(left) - set(right)) for lists hostHashable passed.
func hostSetDifference(left, right []any) ([]any, error) {
	return pySorted(hostValueDifference(left, right))
}

func hostFieldProblems(record, capability map[string]any, paired string) ([]string, error) {
	stop := hostObject(hostObject(capability["events"])["stop"])
	declaredValues, err := hostSorted(hostObject(stop["input"])["required"])
	if err != nil {
		return nil, err
	}
	input := hostObject(record["stopInput"])
	deliveredValues, err := hostSorted(input["fields"])
	if err != nil {
		return nil, err
	}
	if len(declaredValues) == 0 {
		return []string{paired + " declares no required Stop input"}, nil
	}
	if !pyvalue.ItemEqual(deliveredValues, declaredValues) {
		// sorted(set(declared) - set(delivered)) hashes declared, then delivered.
		if err := hostHashable(declaredValues); err != nil {
			return nil, err
		}
		if err := hostHashable(deliveredValues); err != nil {
			return nil, err
		}
		missing, err := hostSetDifference(declaredValues, deliveredValues)
		if err != nil {
			return nil, err
		}
		unexpected, err := hostSetDifference(deliveredValues, declaredValues)
		if err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("delivered Stop fields disagree with %s (missing %s, unexpected %s)", paired, pyvalue.Repr(missing), pyvalue.Repr(unexpected))}, nil
	}
	types, ok := input["types"].(map[string]any)
	if !ok {
		return []string{"records no Stop field types"}, nil
	}
	fields := sortedKeys(types)
	fieldValues := make([]any, len(fields))
	for i, field := range fields {
		fieldValues[i] = field
	}
	if !pyvalue.ItemEqual(fieldValues, deliveredValues) {
		// sorted(set(delivered) - set(types)) hashes delivered; the type names are strings.
		if err := hostHashable(deliveredValues); err != nil {
			return nil, err
		}
		absent, err := hostSetDifference(deliveredValues, fieldValues)
		if err != nil {
			return nil, err
		}
		extra, err := hostSetDifference(fieldValues, deliveredValues)
		if err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("the recorded Stop field types do not cover the fields it delivered (missing %s, unexpected %s)", pyvalue.Repr(absent), pyvalue.Repr(extra))}, nil
	}
	unnamed := []string{}
	for _, name := range fields {
		kind, ok := types[name].(string)
		if !ok || !slices.Contains([]string{"NoneType", "bool", "dict", "float", "int", "list", "str"}, kind) {
			unnamed = append(unnamed, name)
		}
	}
	if len(unnamed) > 0 {
		return []string{"the recorded Stop field types name something that is not a JSON type for " + strings.Join(unnamed, ", ")}, nil
	}
	return nil, nil
}

func hostValueDifference(left, right []any) []any {
	result := []any{}
	for _, value := range left {
		if !hostValueContains(right, value) && !hostValueContains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func hostValueContains(values []any, want any) bool {
	for _, value := range values {
		if pyvalue.ItemEqual(value, want) {
			return true
		}
	}
	return false
}

func replayHostObservations(hostFS fs.FS, dir string, contractFS fs.FS, contractPath string) (int, []string, error) {
	questions, err := replayPacketQuestions(contractFS, contractPath)
	if err != nil {
		return 0, nil, err
	}
	// Python preserves the packet's table order when reporting missing rows.
	contract, err := fs.ReadFile(contractFS, contractPath)
	if err != nil {
		return 0, nil, err
	}
	ids := []string{}
	for _, line := range strings.Split(string(contract), "\n") {
		cells := strings.Split(strings.TrimSpace(line), "|")
		if len(cells) > 2 {
			id := strings.TrimSpace(cells[1])
			if _, exists := questions[id]; exists && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	paths, _ := fs.Glob(hostFS, path.Join(dir, "host-observation-*.json"))
	checked := 0
	problems := []string{}
	for _, filename := range paths {
		name := path.Base(filename)
		value, err := pythonReadJSON(hostFS, filename, "/"+filename)
		if err != nil {
			var osError *probeOSError
			if errors.As(err, &osError) {
				return checked, nil, err
			}
			problems = append(problems, name+": unreadable ("+pythonValueDetail(err)+")")
			continue
		}
		record, recordOK := orderedPlain(value).(map[string]any)
		if !recordOK {
			return checked, nil, pythonAttribute(value, "get")
		}
		checked++
		pairedName := pyvalue.Str(record["capabilityRecord"])
		if pairedName != "" {
			pairedName = path.Base(pairedName)
		}
		pairedPath := path.Join(dir, pairedName)
		own := recordTag(name)
		version, versionString := record["version"].(string)
		info, statErr := fs.Stat(hostFS, pairedPath)
		switch {
		case own == [2]string{}:
			problems = append(problems, name+": its name carries no host and version tag")
		case recordTag(pairedName) != own:
			problems = append(problems, name+": names "+pairedName+", which is not the capability record for the same host and version")
		case !versionString || !slices.Contains(strings.FieldsFunc(version, isSpace), own[1]):
			problems = append(problems, fmt.Sprintf("%s: the version it records, %s, does not state %s, the version its own name carries", name, pyvalue.Repr(record["version"]), own[1]))
		case statErr != nil || !info.Mode().IsRegular():
			problems = append(problems, name+": names a capability record that is not beside it")
		default:
			value, err := pythonReadJSON(hostFS, pairedPath, "/"+pairedPath)
			if err != nil {
				return checked, nil, err
			}
			capability, capabilityOK := orderedPlain(value).(map[string]any)
			if !capabilityOK {
				return checked, nil, pythonAttribute(value, "get")
			}
			fieldProblems, fieldErr := hostFieldProblems(record, capability, pairedName)
			if fieldErr != nil {
				return checked, nil, fieldErr
			}
			for _, problem := range fieldProblems {
				problems = append(problems, name+": "+problem)
			}
		}
		rows := hostObject(record["observations"])
		unanswered := []string{}
		for _, id := range ids {
			if _, ok := rows[id]; !ok {
				unanswered = append(unanswered, id)
			}
		}
		if len(unanswered) > 0 {
			problems = append(problems, name+": the packet asks "+strings.Join(unanswered, ", ")+" and this record carries no such row")
		}
		unasked := hostDifference(sortedKeys(rows), ids)
		if len(unasked) > 0 {
			problems = append(problems, name+": rows the packet does not ask about: "+strings.Join(unasked, ", "))
		}
		for _, id := range sortedKeys(rows) {
			row := hostObject(rows[id])
			status, _ := row["status"].(string)
			claimed := questions[id]
			problem := ""
			switch {
			case !hostStated(row["question"]):
				problem = "states no question"
			case status != "resolved" && status != "unresolved":
				problem = "carries no readable status"
			case (claimed == "resolved" || claimed == "unresolved") && status != claimed:
				problem = "records " + status + " where the packet's own table says " + claimed
			case status == "resolved" && !hostStated(row["observed"]):
				problem = "is resolved and states nothing observed"
			case status == "resolved" && !hostStated(row["evidence"]):
				problem = "is resolved and states no evidence"
			case status == "unresolved" && !hostStated(row["whyUnresolved"]):
				problem = "is unresolved and says nothing about why"
			}
			if problem != "" {
				problems = append(problems, name+": "+id+" "+problem)
			}
		}
	}
	duplicated, unreadable := []string{}, []string{}
	for _, id := range ids {
		switch questions[id] {
		case "duplicated":
			duplicated = append(duplicated, id)
		case "resolved", "unresolved":
		default:
			unreadable = append(unreadable, id)
		}
	}
	if len(duplicated) > 0 {
		problems = append(problems, "the packet's table states "+strings.Join(duplicated, ", ")+" more than once, so one row id would carry two statuses")
	}
	if len(unreadable) > 0 {
		problems = append(problems, "the packet's table states no readable status for "+strings.Join(unreadable, ", "))
	}
	if len(ids) == 0 {
		problems = append(problems, "the contract's host-verification packet asks nothing; its table is unreadable or gone")
	} else if checked == 0 {
		problems = append(problems, "the contract asks "+strings.Join(ids, ", ")+" and no observation record answers any of them")
	}
	return checked, problems, nil
}
