package hook

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CXC v0.2.40 (3c1459ac), agent-thread-permissions.ts:1-57,89-116,316-405.
// Project config and PABCD are owned by projectcfg and the harness; these
// permission-stage handlers run before that switch and never write state.
const maxMetaLineBytes = 64 * 1024
const maxThreadConfigBytes = 1024 * 1024
const agentThreadAllow = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`
const agentThreadModelAdvice = "This Codex Desktop agent-created thread may show approval prompts even though the user Codex config requests full access. Request escalation explicitly for network or git operations when needed. If the user enabled permissions.agentCreatedThreadAutoAllow, crw answers pending approvals, including one-time network requests, without prompting; it never changes this thread's sandbox."
const agentThreadUserAdvice = "This agent-created thread started in the default approval mode despite your full-access Codex config, so approval prompts may appear. You can switch this thread to Full Access in the composer, or set permissions.agentCreatedThreadAutoAllow to true in ~/.crw/config.json so crw answers these approvals for you, including one-time network requests."

func agentThreadObject(raw string) map[string]any {
	// Keep JSON.parse's depth behavior as well as its distinct surrogate IDs;
	// the harness and file readers already enforce their byte bounds.
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	if err != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

func readFirstRecord(path string) map[string]any {
	if !filepath.IsAbs(path) {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMetaLineBytes+1))
	if err != nil {
		return nil
	}
	if end := bytes.IndexByte(b, '\n'); end >= 0 {
		b = b[:end]
	}
	if len(b) == 0 || len(b) > maxMetaLineBytes {
		return nil
	}
	return agentThreadObject(source.DecodeUTF8(b))
}

func agentCreatedRoot(input map[string]any) bool {
	if input["permission_mode"] != "default" || stringField(input, "session_id") == "" {
		return false
	}
	path, pathOK := input["transcript_path"].(string)
	_, agentID := input["agent_id"]
	_, agentType := input["agent_type"]
	if !pathOK || agentID || agentType {
		return false
	}
	record := readFirstRecord(path)
	payload, _ := record["payload"].(map[string]any)
	return record["type"] == "session_meta" && payload["id"] == input["session_id"] &&
		payload["thread_source"] == "agent_created_thread" && payload["forked_from_id"] == nil
}

func boundedThreadText(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxThreadConfigBytes {
		return "", false
	}
	b, err := os.ReadFile(path)
	return source.DecodeUTF8(b), err == nil
}

func threadRealPath(path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist // realpathSync("") fails rather than resolving cwd.
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func trustedThreadConfig(env host.LookupEnv, key, filename, cwd string) (string, bool) {
	value, _ := env(key)
	override := text.Trim(value)
	if override != "" && !filepath.IsAbs(override) {
		return "", false
	}
	home, err := host.Home(env)
	if err != nil {
		return "", false
	}
	dirName := ".codex"
	dir := filepath.Join(home, dirName)
	if key == "CRW_HOME" {
		dirName = crwdir.DirName
		// This consumer trims the override, while host.CRWHome uses it as given.
		dir, err = host.CRWHome(func(k string) (string, bool) {
			if k == "CRW_HOME" {
				return override, override != ""
			}
			return env(k)
		})
		if err != nil {
			return "", false
		}
	} else if override != "" {
		dir = override
	}
	path := filepath.Join(dir, filename)
	realCwd, err := threadRealPath(cwd)
	if err != nil {
		return "", false
	}
	realConfig, err := threadRealPath(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realCwd, realConfig)
	if err != nil {
		return "", false
	}
	// Security fix: '..named' is a child, not a parent component. The oracle's
	// rel.startsWith('..') let project-controlled configs in such children grant.
	withinCwd := filepath.IsLocal(rel)
	realHome, err := threadRealPath(home)
	if err != nil {
		return "", false
	}
	defaultAtHome := false
	if override == "" && realCwd == realHome && realConfig == filepath.Join(realHome, dirName, filename) {
		info, err := os.Lstat(path)
		defaultAtHome = err == nil && info.Mode().IsRegular()
	}
	if withinCwd && !defaultAtHome {
		return "", false
	}
	return boundedThreadText(realConfig)
}

func globalThreadOptIn(env host.LookupEnv, cwd string) bool {
	content, ok := trustedThreadConfig(env, "CRW_HOME", "config.json", cwd)
	if !ok {
		return false
	}
	config := agentThreadObject(content)
	permissions, _ := config["permissions"].(map[string]any)
	return permissions["agentCreatedThreadAutoAllow"] == true
}

func validTomlAndTopLevel(content string) bool {
	root := newTomlTable()
	current := root
	seen := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n"), "\n")
	header := regexp.MustCompile(`^\[(` + lintDot + `*)\]` + lintSpace + `*(?:#` + lintDot + `*)?$`)
	arrayHeader := regexp.MustCompile(`^\[\[(` + lintDot + `*)\]\]` + lintSpace + `*(?:#` + lintDot + `*)?$`)
	exact := regexp.MustCompile(`^"([^"\r\n]*)"` + lintSpace + `*(?:#` + lintDot + `*)?$`)
	for index := 0; index < len(lines); index++ {
		line := text.Trim(lines[index])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			array := strings.HasPrefix(line, "[[")
			match := header.FindStringSubmatch(line)
			if array {
				match = arrayHeader.FindStringSubmatch(line)
			}
			if match == nil {
				return false
			}
			parsed := tomlKeyPath(match[1], 0)
			if parsed == nil || parsed.end != len(match[1]) {
				return false
			}
			current = tomlEnterTable(root, parsed.parts, array)
			if current == nil {
				return false
			}
			continue
		}
		parsed := tomlKeyPath(line, 0)
		if parsed == nil || parsed.end >= len(line) || line[parsed.end] != '=' || !tomlAssignKey(current, parsed.parts) {
			return false
		}
		key := ""
		if len(parsed.parts) == 1 {
			key = parsed.parts[0]
		}
		if current == root && key == "profile" {
			return false
		}
		value := strings.TrimLeftFunc(line[parsed.end+1:], func(r rune) bool { return text.Trim(string(r)) == "" })
		for !validTomlValue(value) {
			if !(strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") || strings.HasPrefix(value, `"""`) || strings.HasPrefix(value, "'''")) || index+1 >= len(lines) {
				return false
			}
			index++
			value += "\n" + lines[index]
		}
		if current == root && (key == "approval_policy" || key == "sandbox_mode") {
			match := exact.FindStringSubmatch(value)
			if match == nil {
				return false
			}
			seen[key] = match[1]
		}
	}
	return seen["approval_policy"] == "never" && seen["sandbox_mode"] == "danger-full-access"
}

func codexConfigFullAccess(env host.LookupEnv, cwd string) bool {
	content, ok := trustedThreadConfig(env, "CODEX_HOME", "config.toml", cwd)
	return ok && validTomlAndTopLevel(content)
}

func coveredThreadTool(value any) bool {
	name, ok := value.(string)
	if !ok {
		return false
	}
	if name == "Bash" || name == "write_stdin" || name == "apply_patch" {
		return true
	}
	match := regexp.MustCompile(`^mcp__(` + lintDot + `+?)__(` + lintDot + `+)$`).FindStringSubmatch(name)
	return match != nil && strings.ReplaceAll(match[1], "_", "") != "" && strings.ReplaceAll(match[2], "_", "") != ""
}

func parseThreadHook(raw, event string) map[string]any {
	input := agentThreadObject(raw)
	if input["hook_event_name"] != event {
		return nil
	}
	return input
}

// HandleAgentThreadPermissionRequest answers only an opted-in agent-created root
// with full-access user config. It never changes the thread's sandbox or files.
func HandleAgentThreadPermissionRequest(raw string, env host.LookupEnv) string {
	input := parseThreadHook(raw, "PermissionRequest")
	cwd := stringField(input, "cwd")
	if input != nil && cwd != "" && coveredThreadTool(input["tool_name"]) && globalThreadOptIn(env, cwd) && agentCreatedRoot(input) && codexConfigFullAccess(env, cwd) {
		return agentThreadAllow
	}
	return ""
}

// HandleAgentThreadSessionStartAdvisory advises even without global opt-in, as in
// the oracle; only the native root identity and full-access user config govern it.
func HandleAgentThreadSessionStartAdvisory(raw string, env host.LookupEnv) string {
	input := parseThreadHook(raw, "SessionStart")
	cwd := stringField(input, "cwd")
	if input == nil || cwd == "" || !agentCreatedRoot(input) || !codexConfigFullAccess(env, cwd) {
		return ""
	}
	type output struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	}
	b, err := role.Stringify(struct {
		Message string `json:"systemMessage"`
		Output  output `json:"hookSpecificOutput"`
	}{agentThreadUserAdvice, output{"SessionStart", agentThreadModelAdvice}}, "")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}
