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
