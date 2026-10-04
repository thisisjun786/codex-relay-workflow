package doctor

type CodexInvocation struct {
	File    string          `json:"file"`
	Args    []string        `json:"args"`
	Options map[string]bool `json:"options"`
}

func ResolveCodexInvocation(command string, args, env []string) CodexInvocation {
	return CodexInvocation{}
}
