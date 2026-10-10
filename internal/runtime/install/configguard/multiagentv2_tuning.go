package configguard

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// The tuning of multi_agent_v2 is the settings of its [features.multi_agent_v2] table other than enabled. The Codex CLI turns
// the flag by writing multi_agent_v2 = <bool> under [features], which replaces the table, so crw puts the table back after
// the runner (CRW-1141). Both the detection and the restore read the document through internal/tomledit, so a quoted or
// spaced header, a comment, an enabled line inside a string and a multi-line value are handled as the decoder reads them;
// a form crw cannot restore exactly is refused before the runner runs.

var multiAgentV2Path = []string{"features", "multi_agent_v2"}

func multiAgentV2Table(doc map[string]any) (map[string]any, bool) {
	features, _ := doc["features"].(map[string]any)
	table, ok := features["multi_agent_v2"].(map[string]any)
	return table, ok
}

func hasPathPrefix(path, prefix []string) bool {
	return len(path) >= len(prefix) && slices.Equal(path[:len(prefix)], prefix)
}

// multiAgentV2Tuning answers the lines of the multi_agent_v2 table that are tuning (everything but its enabled key), as
// written, and whether there is any. A document whose tuning is not in one [features.multi_agent_v2] table (an inline table,
// dotted keys, sub-tables, a second header) is refused.
func multiAgentV2Tuning(pre string) (string, bool, error) {
	doc, err := tomledit.Decode(pre)
	if err != nil {
		return "", false, err
	}
	table, ok := multiAgentV2Table(doc)
	if !ok {
		return "", false, nil
	}
	tuned := false
	for key := range table {
		tuned = tuned || key != "enabled"
	}
	if !tuned {
		return "", false, nil
	}
	refuse := func(why string) (string, bool, error) {
		return "", false, fmt.Errorf("features.multi_agent_v2 carries tuning that is %s, and the Codex CLI would drop it when it turns the flag; write it as one [features.multi_agent_v2] table and run the command again. Nothing was changed", why)
	}
	stmts, err := tomledit.Statements(pre)
	if err != nil {
		return refuse("in a form crw cannot locate (" + err.Error() + ")")
	}
	header := -1
	for i, st := range stmts {
		switch {
		case !st.Table && !st.ArrayTable || !hasPathPrefix(st.Path, multiAgentV2Path):
		case st.Table && len(st.Path) == len(multiAgentV2Path) && header < 0:
			header = i
		default:
			return refuse("in a second table, a sub-table or an array of tables")
		}
	}
	if header < 0 {
		return refuse("an inline table or dotted keys")
	}
	end := len(pre)
	for _, st := range stmts[header+1:] {
		if st.Table || st.ArrayTable {
			end = st.Start
			break
		}
	}
	var kept strings.Builder
	at := stmts[header].End
	for _, st := range stmts[header+1:] {
		if st.Start >= end {
			break
		}
		if !hasPathPrefix(st.Path, multiAgentV2Path) {
			return refuse("mixed with other keys")
		}
		if len(st.Path) == len(multiAgentV2Path)+1 && st.Path[len(st.Path)-1] == "enabled" {
			kept.WriteString(pre[at:st.Start])
			at = st.End
		}
	}
	kept.WriteString(pre[at:end])
	tuning := strings.TrimRight(kept.String(), " \t\r\n")
	for {
		line, rest, found := strings.Cut(tuning, "\n")
		if !found || strings.TrimSpace(line) != "" {
			break
		}
		tuning = rest
	}
	return tuning, true, nil
}

// multiAgentV2Preserve puts the multi_agent_v2 table back after a runner replaced it with the scalar flag: it removes the
// scalar and appends [features.multi_agent_v2] with enabled = want and the tuning as written. It answers false when there is
// nothing to restore (no tuning before, or the runner kept a table). The candidate is checked to hold the same document as
// post except for the table, and the table to hold the flag and the tuning it had.
func multiAgentV2Preserve(pre, post string, want bool) (string, bool, error) {
	tuning, has, err := multiAgentV2Tuning(pre)
	if err != nil || !has {
		return "", false, err
	}
	postDoc, err := tomledit.Decode(post)
	if err != nil {
		return "", false, err
	}
	features, _ := postDoc["features"].(map[string]any)
	if value, ok := features["multi_agent_v2"].(bool); !ok || value != want {
		return "", false, nil
	}
	stmts, err := tomledit.Statements(post)
	if err != nil {
		return "", false, fmt.Errorf("the runner replaced the multi_agent_v2 table, and the file is in a form crw cannot locate the flag in (%w)", err)
	}
	at := -1
	for i, st := range stmts {
		if !st.Table && !st.ArrayTable && slices.Equal(st.Path, multiAgentV2Path) {
			if at >= 0 || st.Multiline {
				return "", false, errors.New("the runner replaced the multi_agent_v2 table, and the flag it wrote is not on a line of its own")
			}
			at = i
		}
	}
	if at < 0 {
		return "", false, errors.New("the runner replaced the multi_agent_v2 table, and the flag it wrote cannot be located (it is inside an inline table)")
	}
	eol := "\n"
	if strings.Contains(pre, "\r\n") {
		eol = "\r\n"
	}
	rest := post[:stmts[at].Start] + post[stmts[at].End:]
	value := strconvBool(want)
	out := strings.TrimRight(rest, "\r\n")
	if out != "" {
		out += eol + eol
	}
	out += "[features.multi_agent_v2]" + eol + "enabled = " + value + eol + tuning + eol
	if err := multiAgentV2CheckRepair(pre, post, out, want); err != nil {
		return "", false, err
	}
	return out, true, nil
}

func multiAgentV2CheckRepair(pre, post, out string, want bool) error {
	before, _ := tomledit.Decode(pre)
	oldTable, _ := multiAgentV2Table(before)
	postDoc, _ := tomledit.Decode(post)
	cand, err := tomledit.Decode(out)
	if err != nil {
		return fmt.Errorf("the multi_agent_v2 tuning repair would leave config.toml invalid: %w", err)
	}
	// The tuning is compared without enabled, which the repair always writes with the requested value: a table that had no
	// enabled key before is restored with one, and its tuning keys must be the same keys with the same values.
	table, ok := multiAgentV2Table(cand)
	if !ok || table["enabled"] != want || multiAgentV2TuningKeys(table) != multiAgentV2TuningKeys(oldTable) {
		return errors.New("the multi_agent_v2 tuning repair would not restore the table as it was")
	}
	for key, value := range oldTable {
		if key != "enabled" && !tomledit.Equal(value, table[key]) {
			return errors.New("the multi_agent_v2 tuning repair would not restore the table as it was")
		}
	}
	if !tomledit.Equal(withoutMultiAgentV2(postDoc), withoutMultiAgentV2(cand)) {
		return errors.New("the multi_agent_v2 tuning repair would change more than the multi_agent_v2 table")
	}
	return nil
}

// multiAgentV2TuningKeys counts the keys of a multi_agent_v2 table other than enabled.
func multiAgentV2TuningKeys(table map[string]any) int {
	n := len(table)
	if _, ok := table["enabled"]; ok {
		n--
	}
	return n
}

// withoutMultiAgentV2 is a copy of a decoded document without features.multi_agent_v2.
func withoutMultiAgentV2(doc map[string]any) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	if features, ok := doc["features"].(map[string]any); ok {
		copied := make(map[string]any, len(features))
		for k, v := range features {
			if k != "multi_agent_v2" {
				copied[k] = v
			}
		}
		out["features"] = copied
	}
	return out
}
