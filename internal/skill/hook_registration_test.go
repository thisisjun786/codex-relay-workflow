package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHookRegistration(t *testing.T) {
	goldenRoot(t)
	crw := recordedCRW(t)
	for _, name := range []string{"declared and trusted", "no registration", "sanitized"} {
		t.Run(name, func(t *testing.T) {
			// Given a schema-bearing binary and independent declaration/trust records.
			dir := t.TempDir()
			binary := filepath.Join(dir, "codex")
			schema := "{\n  \"$schema\": \"http://json-schema.org/draft-07/schema#\",\n  \"title\": \"stop.command.input\",\n  \"required\": [\"session_id\"], \"properties\": {\"session_id\": {\"type\": \"string\"}}\n}\n"
			schema += "{\n  \"$schema\": \"http://json-schema.org/draft-07/schema#\",\n  \"title\": \"pre-tool-use.command.input\",\n  \"properties\": {}\n}\n"
			if err := os.WriteFile(binary, []byte(schema), 0600); err != nil {
				t.Fatal(err)
			}
			codexHome := filepath.Join(dir, "codex-home")
			if name != "no registration" {
				for path, contents := range map[string]string{
					"hooks.json": `{"hooks":{"Stop":[],"SessionStart":[]}}`,
					"plugins/cache/vendor/tool/1/hooks/hooks.json": `{"hooks":{"PreToolUse":[],"Stop":[]}}`,
					"plugins/local/hooks/hooks.json":               `{"hooks":{"PostToolUse":[]}}`,
					"plugins/local/hooks/broken.json":              `{`,
					"plugins/local/hooks/list.json":                `{"hooks":[]}`,
					"config.toml":                                  "[hooks.state.\"file:with:colons:stop:1:1\"]\n[hooks.state.\"source:pre_tool_use:2:1\"]\n[hooks.state.\"source:unknown:1:1\"]\n[hooks.state.\"source:stop:3:1\"]\n",
				} {
					path = filepath.Join(codexHome, path)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			args := []string{"observe", "--binary", binary, "--codex-home", codexHome}
			if name == "sanitized" {
				args = append(args, "--sanitize")
			}
			// When the public command observes the fixture host.
			command := exec.Command(crw, append([]string{"skill", "hook-probe"}, args...)...)
			got := captureSkillProcess(t, command)
			if got.exit != 0 {
				t.Fatalf("observe failed: %+v", got)
			}
			// Then it answers the golden (first taken as hook_probe.py's answer).
			checkSkillAnswer(t, "", "", append([]string{"skill", "hook-probe"}, args...), got)
			// And the observation must actually have exercised active registration.
			if name == "declared and trusted" {
				var report struct {
					Registration struct {
						DeclaredBy         []json.RawMessage
						DeclaredEvents     []string
						HostRecordedEvents []string
					}
				}
				if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
					t.Fatal(err)
				}
				if len(report.Registration.DeclaredBy) != 3 || len(report.Registration.DeclaredEvents) != 4 || len(report.Registration.HostRecordedEvents) != 2 {
					t.Fatalf("observe did not see the fixture registrations: %s", got.stdout)
				}
			}
		})
	}
}
