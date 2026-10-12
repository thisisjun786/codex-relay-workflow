package scenario

import "github.com/thisisjun786/codex-relay-workflow/internal/flow/script"

type scriptProgram = script.Program

func loadScript(text string) (*scriptProgram, error) {
	return script.Load(script.Source{Name: "t.js", Text: []byte(text)})
}
