package engine

import "strings"

// RepositoryTools are the repository tools the configuration names (CRW-1488, 1209 trial finding 12): today the go the verifier
// runs (verifier.go_root). Every agent call the run starts is given them. An App Server call's thread gets Env in its shell (the
// backend sets them as the thread's shell_environment_policy.set entries, over the host's own policy) and its role prompt names
// them. A session call's subagent shell is the owner session's, which the run cannot set (spawn_agent takes no environment), so
// its packet names them with Prefix, the form a command puts in front of itself.
type RepositoryTools struct {
	// Env are the KEY=value entries set over a call's inherited environment: exactly the ones the verifier's command gets.
	Env []string
	// Prefix is the same entries as a shell command prefix, with the inherited PATH kept as $PATH.
	Prefix string
	// Facts are the lines that name each tool, the setting it comes from and the file that holds the setting.
	Facts []string
}

// facts is the list of the tools' lines.
func (t *RepositoryTools) facts() string {
	var b strings.Builder
	for _, f := range t.Facts {
		b.WriteString("- " + f + "\n")
	}
	return b.String()
}

// PacketSection is the "Repository tools" section of a session call's packet: the tools and the prefix a command that runs one
// takes; "" without tools.
func (t *RepositoryTools) PacketSection() string {
	if t == nil || len(t.Facts) == 0 {
		return ""
	}
	return "## Repository tools\n\n" + t.facts() + "\n" +
		"Your shell does not get these from the run: it is the owner session's. Put `" + t.Prefix + "` in front of every command that runs " +
		"one of them (go build, go test, go vet, gofmt), and write every such command of a plan, a check or a receipt that way.\n\n"
}

// PromptSection is the "Repository tools" section an App Server call's role prompt ends with: the tools and the entries its shell
// has; "" without tools.
func (t *RepositoryTools) PromptSection() string {
	if t == nil || len(t.Facts) == 0 {
		return ""
	}
	return "## Repository tools\n\n" + t.facts() + "\nYour shell has them: " + strings.Join(t.Env, ", ") + " are set in it.\n"
}

// ShellSet is Env as the KEY: value table of a shell_environment_policy.set; nil without tools.
func (t *RepositoryTools) ShellSet() map[string]any {
	if t == nil || len(t.Env) == 0 {
		return nil
	}
	set := map[string]any{}
	for _, kv := range t.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			set[k] = v
		}
	}
	return set
}
