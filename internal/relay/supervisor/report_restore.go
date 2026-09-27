package supervisor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func validateReportRestore(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	restore, ok := v.(map[string]any)
	if !ok {
		return nil, reportRefusal("malformed_receipt", fmt.Sprintf("a restore section is an object of named fields, not %s", reportType(v)))
	}
	skills := []any{}
	if raw := restore["skills"]; raw != nil {
		var valid bool
		skills, valid = raw.([]any)
		if !valid {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("restore skills is a list of activity names, not %s", reportType(raw)))
		}
	}
	known := []string{"development", "loop", "lost-context", "pull-request", "review-repair"}
	unknown := []string{}
	for _, raw := range skills {
		name, ok := raw.(string)
		if !ok {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("each restore skill is the name of a recorded activity, not %s", reportRepr(raw)))
		}
		found := false
		for _, entry := range known {
			if entry == name {
				found = true
			}
		}
		if !found {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		rendered := make([]string, len(unknown))
		for i, name := range unknown {
			rendered[i] = store.PythonRepr(name)
		}
		return nil, reportRefusal("malformed_receipt", fmt.Sprintf("no recorded skill owner for [%s]; known activities are ['development', 'loop', 'lost-context', 'pull-request', 'review-repair']. Naming an owner nobody has would fail at render time, inside the delivery claim, instead of here", strings.Join(rendered, ", ")))
	}
	checked := map[string]any{}
	if len(skills) > 0 {
		checked["skills"] = skills
	}
	fields := []string{"mode", "scope", "phase", "phaseObservedAt", "plan", "evidence", "remaining"}
	keys := make([]string, 0, len(restore))
	for key := range restore {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "skills" {
			continue
		}
		supported := false
		for _, name := range fields {
			if name == key {
				supported = true
				break
			}
		}
		if !supported {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("%s is not a restore field this build renders; supported fields are ['mode', 'scope', 'phase', 'phaseObservedAt', 'plan', 'evidence', 'remaining'] plus skills. An unrendered field is one the recipient never sees and is never told was dropped", store.PythonRepr(key)))
		}
		if restore[key] == nil {
			continue
		}
		value, ok := restore[key].(string)
		if !ok {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("restore %s is a single line of text, not %s", key, reportType(restore[key])))
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		var err error
		value, err = reportLine(value, "restore "+key, 300)
		if err != nil {
			return nil, err
		}
		checked[key] = value
	}
	if len(checked) > 0 && checked["mode"] == nil {
		present := make([]string, 0, len(checked))
		for key := range checked {
			present = append(present, store.PythonRepr(key))
		}
		sort.Strings(present)
		return nil, reportRefusal("malformed_receipt", "a restore section states the effective workflow in mode: it is the one field no transport carries, so leaving it out drops it rather than deferring it. Present fields were ["+strings.Join(present, ", ")+"]")
	}
	return checked, nil
}
