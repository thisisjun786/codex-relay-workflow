package engine

import "strings"

// RepositoryTools are the repository tools the configuration names (CRW-1488, 1209 trial finding 12): today the go the verifier
// runs (verifier.go_root). Every agent call the run starts is given them. Every packet names them: a session call's packet (its
// subagent shell is the owner session's, which the run cannot set: spawn_agent takes no environment) and the text of every App
// Server turn. An App Server thread also gets Env in its shell, as the leaf keys ShellOverrides names, on the thread/start or
// thread/fork that creates it, per call and with no state kept anywhere. A thread the host loads again after a restart is rebuilt
// from the host's configuration and that thread/resume's config only: the backend sends the same leaf keys on it, from the turn's
// TurnSpec.Tools (CRW-1559), and the turn text still names the tools.
type RepositoryTools struct {
	// Env are the KEY=value entries set over a call's inherited environment: exactly the ones the verifier's command gets.
	Env []string
	// Prefix is the same entries as a shell command prefix, with the inherited PATH kept as $PATH.
	Prefix string
	// Facts are the lines that name each tool, the setting it comes from and the file that holds the setting.
	Facts []string
}

// PacketSection is the "Repository tools" section of a call's packet: the tools and the prefix the commands take; "" without
// tools. The prefix holds wherever the call runs: a session subagent's shell is the owner session's, and an App Server thread
// loaded again after a host restart is rebuilt from the host's configuration plus what its resume carries. With the run's TMPDIR
// among the tools (CRW-1570) the prefix goes in front of every command the subagent runs, since every command that makes a temporary
// file takes TMPDIR; with the go alone, in front of every command that runs it.
func (t *RepositoryTools) PacketSection() string {
	if t == nil || len(t.Facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Repository tools\n\n")
	for _, f := range t.Facts {
		b.WriteString("- " + f + "\n")
	}
	if t.sets(runTempEnv) {
		b.WriteString("\nYour shell may not have them: put `" + t.Prefix + "` in front of every command you run (TMPDIR concerns every command " +
			"that makes a temporary file, not only go build, go test, go vet and gofmt), and write every command of a plan, a check or a " +
			"receipt that way.\n\n")
		return b.String()
	}
	b.WriteString("\nYour shell may not have them: put `" + t.Prefix + "` in front of every command that runs one of them (go build, go test, " +
		"go vet, gofmt), and write every such command of a plan, a check or a receipt that way.\n\n")
	return b.String()
}

// runTempEnv is the variable of the run's temporary location among the tools' Env (CRW-1570); it only names the entry, and the
// location itself is tmploc's.
const runTempEnv = "TMPDIR"

// sets says whether Env has an entry for the variable name.
func (t *RepositoryTools) sets(name string) bool {
	for _, kv := range t.Env {
		if k, _, _ := strings.Cut(kv, "="); k == name {
			return true
		}
	}
	return false
}

// ShellOverrides are Env as the config overrides of a thread/start, thread/fork or thread/resume: one leaf key
// shell_environment_policy.set.<NAME> for each entry. Codex 0.154 applies a request's override keys one by one, each by its dotted
// path, over the server's own overrides (app-server config_manager.rs load_with_cli_overrides, config overrides.rs
// apply_toml_override), so a leaf key changes that variable only and the policy the server set (inherit, exclude, include_only,
// its other set entries) stays; a shell_environment_policy table would replace it. An entry whose name is empty or holds a dot,
// which a dotted key cannot name, is left out. Nil without tools.
func (t *RepositoryTools) ShellOverrides() map[string]any {
	if t == nil || len(t.Env) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, kv := range t.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" && !strings.Contains(k, ".") {
			out["shell_environment_policy.set."+k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toolsSuffix is what an App Server turn's text ends with: the packet section after the task, so the turn names the tools whatever
// its thread's shell holds (CRW-1488). The journaled input is the script's; "" without tools.
func toolsSuffix(t *RepositoryTools) string {
	section := t.PacketSection()
	if section == "" {
		return ""
	}
	return "\n\n" + strings.TrimRight(section, "\n")
}
