package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type hookReplayReach struct {
	reached map[string]bool
}

func newHookReplayReach() *hookReplayReach { return &hookReplayReach{reached: map[string]bool{}} }
func (r *hookReplayReach) Reach(function string, ordinal int) {
	if r == nil {
		return
	}
	r.reached[function+":"+fmt.Sprint(ordinal)] = true
}

type hookReplaySite struct {
	key, function, source string
	line                  int
}

func pythonReturnSites(fsys fs.FS) ([]hookReplaySite, error) {
	raw, err := fs.ReadFile(fsys, "crw/skills/crw-run/scripts/hook_probe.py")
	if err != nil {
		return nil, err
	}
	return parsePythonReturns(string(raw)), nil
}

func parsePythonReturns(source string) []hookReplaySite {
	targets := map[string]bool{"observe_state": true, "decide": true, "derive_assignment_state": true, "identity_contested": true, "classify_declaration": true, "_correlated": true, "_correlation_problem": true, "_covered": true, "_ambiguity_resolved": true, "resolve_assignment": true, "selected_marker": true, "_claimant": true}
	lines := strings.Split(source, "\n")
	var sites []hookReplaySite
	function, bodyIndent, nestedIndent, ordinal := "", -1, -1, 0
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(trimmed, "def ") {
			name := strings.SplitN(strings.TrimPrefix(trimmed, "def "), "(", 2)[0]
			if targets[name] {
				function, bodyIndent, nestedIndent, ordinal = name, indent, -1, 0
			} else if function != "" {
				if indent <= bodyIndent {
					function = ""
				} else {
					nestedIndent = indent
				}
			}
			continue
		}
		if function == "" || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent <= bodyIndent {
			function = ""
			continue
		}
		if nestedIndent >= 0 {
			if indent > nestedIndent {
				continue
			}
			nestedIndent = -1
		}
		if strings.HasPrefix(trimmed, "return ") {
			ordinal++
			sites = append(sites, hookReplaySite{key: function + ":" + fmt.Sprint(ordinal), function: function, line: index + 1, source: trimmed})
		}
	}
	return sites
}

func explicitSkillFS(path string) (fs.FS, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return os.DirFS("."), path
	}
	return os.DirFS(filepath.VolumeName(abs) + string(filepath.Separator)), filepath.ToSlash(strings.TrimPrefix(abs, filepath.VolumeName(abs)+string(filepath.Separator)))
}

func replayTraceIDs(fsys fs.FS, path string) ([]string, error) {
	raw, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| T") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		id := strings.TrimSpace(parts[1])
		if len(id) > 1 && id[0] == 'T' && strings.Trim(id[1:], "0123456789") == "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func replayMissingTraces(paths, ids []string) []string {
	names := make([]string, len(paths))
	for i, path := range paths {
		names[i] = strings.ToLower(filepath.Base(path))
	}
	var missing []string
	for _, id := range ids {
		prefix := strings.ToLower(id) + "-"
		alt := strings.ToLower(id) + "b-"
		found := false
		for _, name := range names {
			found = found || strings.HasPrefix(name, prefix) || strings.HasPrefix(name, alt)
		}
		if !found {
			missing = append(missing, id)
		}
	}
	return missing
}

func replayPacketQuestions(fsys fs.FS, path string) (map[string]string, error) {
	raw, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| H") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		id := strings.TrimSpace(cells[1])
		if len(id) < 2 || id[0] != 'H' || strings.Trim(id[1:], "0123456789") != "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(cells[len(cells)-2]))
		if _, exists := found[id]; exists {
			found[id] = "duplicated"
		} else {
			found[id] = status
		}
	}
	return found, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
