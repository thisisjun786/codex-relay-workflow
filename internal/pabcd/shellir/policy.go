package shellir

import "strings"

func isWrapper(name string) bool {
	switch name {
	case "env", "command", "builtin", "exec", "nohup", "nice", "ionice", "timeout", "time",
		"stdbuf", "setsid", "sudo", "doas", "xargs", "busybox", "find", "parallel",
		"watch", "flock", "chroot", "script", "strace", "ltrace", "entr", "at", "batch":
		return true
	}
	return false
}

// isShell is whether a program name is a POSIX or Korn family shell: a program that runs shell text from -c, a script file,
// a here-document, a here-string or the standard input. busybox reaches the same family through its applets (sh, ash, hush);
// the wrapper table hands the applet to this check.
func isShell(name string) bool {
	switch name {
	case "bash", "sh", "dash", "zsh", "ksh", "ash", "mksh", "hush", "pdksh", "oksh", "posh", "yash", "rbash":
		return true
	}
	return false
}

// isOnceCarrier names the carrier of a shell's -c string: the shell runs that text once, in place.
func isOnceCarrier(carrier string) bool {
	name, ok := strings.CutSuffix(carrier, " -c")
	return ok && isShell(name)
}

// IsShell is isShell for the consumers that read a shebang line: a script whose interpreter is one of these names is shell text.
func IsShell(name string) bool { return isShell(name) }

// isCodeEnvName lists the environment names that make a program run code the
// text does not show. The list is closed; tests pin it. SHELL picks the shell that
// flock -c, script -c, watch and entr -s run their string with. RIPGREP_CONFIG_PATH names
// a configuration whose --pre runs a program; GIT_EXTERNAL_DIFF names the program git diff
// runs; GIT_CONFIG_PARAMETERS, GIT_CONFIG_COUNT, GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM give
// git configuration keys (diff.external, core.pager) the text does not show.
func isCodeEnvName(name string) bool {
	switch name {
	case "BASH_ENV", "ENV", "ZDOTDIR", "GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_SSH_COMMAND",
		"PAGER", "GIT_PAGER", "LD_PRELOAD", "LD_LIBRARY_PATH", "PYTHONSTARTUP", "PYTHONPATH",
		"NODE_OPTIONS", "RUBYOPT", "PERL5OPT", "npm_config_script_shell", "SHELL",
		"RIPGREP_CONFIG_PATH", "GIT_EXTERNAL_DIFF", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM":
		return true
	}
	return false
}

// modelledName reports a name a function must not shadow: the layer's own
// programs and the programs the consumers judge.
func modelledName(name string) bool {
	if isWrapper(name) || isShell(name) || isInterpreter(name) {
		return true
	}
	switch name {
	case "eval", "source", ".", "trap", "cd", "chdir", "pushd", "popd", "su", "git", "npm", "gh",
		"set", "unset", "hash", "tee", "cp", "mv", "install", "dd", "sort", "rm", "ln",
		"unlink", "rmdir", "curl", "wget", "setopt", "unsetopt", "alias", "unalias",
		"repeat", "foreach", "read", "printf", "echo", "test", "[":
		return true
	}
	return false
}

// zshOnlyUse names a use whose meaning differs between zsh and bash. The text
// is refused rather than read under one of the two meanings.
func zshOnlyUse(name string, args []Word) string {
	switch name {
	case "repeat", "foreach", "setopt", "unsetopt", "alias", "unalias", "emulate", "zmodload",
		"autoload", "compdef", "bindkey", "zstyle":
		return name + " has a zsh meaning the reader does not model"
	case "hash":
		// hash -r and hash -l take options only; a word that is no option, or one the reader cannot read, is an operand.
		for _, a := range args {
			if !a.Known || !strings.HasPrefix(a.Value, "-") {
				return "hash with operands"
			}
		}
	case "set":
		for i, a := range args {
			if !a.Known {
				return "set with an operand that is not known"
			}
			if (a.Value == "-o" || a.Value == "+o") && i+1 < len(args) {
				return "set -o with an operand"
			}
		}
	}
	return ""
}

// checkGit accepts only the git invocations whose meaning the text shows: the
// global options it models, configuration keys that name no code, and a
// subcommand from a closed list.
func checkGit(args []Word) error {
	i := 0
	for i < len(args) {
		v, err := knownValue(args[i], "git option")
		if err != nil {
			return err
		}
		switch {
		case v == "-C" || v == "--git-dir" || v == "--work-tree" || v == "--namespace" || v == "--super-prefix":
			i += 2
		case v == "-c":
			if i+1 >= len(args) {
				return unreadablef("git -c without a key")
			}
			kv, err := knownValue(args[i+1], "git -c key")
			if err != nil {
				return err
			}
			if gitConfigKeyForbidden(kv) {
				return unreadablef("git -c %s names code the text does not show", kv)
			}
			i += 2
		case strings.HasPrefix(v, "--exec-path"):
			return unreadablef("git --exec-path runs code the text does not show")
		case strings.HasPrefix(v, "--config-env"):
			return unreadablef("git --config-env reads configuration from the environment")
		case strings.HasPrefix(v, "--git-dir=") || strings.HasPrefix(v, "--work-tree=") ||
			strings.HasPrefix(v, "--namespace=") || strings.HasPrefix(v, "--super-prefix="):
			i++
		case v == "--no-pager" || v == "-p" || v == "-P" || v == "--paginate" || v == "--bare" ||
			v == "--no-replace-objects" || v == "--literal-pathspecs" || v == "--glob-pathspecs" ||
			v == "--noglob-pathspecs" || v == "--icase-pathspecs" || v == "--no-optional-locks" ||
			v == "--no-lazy-fetch" || v == "--version" || v == "--help":
			i++
		case strings.HasPrefix(v, "-"):
			return unreadablef("git option %s is not modelled", v)
		default:
			if !gitSubcommandAllowed(v) {
				return unreadablef("git %s may be an alias or an external git-%s", v, v)
			}
			return nil
		}
	}
	return nil
}

func gitSubcommandAllowed(name string) bool {
	switch name {
	case "add", "am", "apply", "archive", "bisect", "blame", "branch", "bundle", "cat-file",
		"checkout", "cherry", "cherry-pick", "clean", "clone", "commit", "config", "count-objects",
		"describe", "diff", "fetch", "for-each-ref", "format-patch", "fsck", "gc", "grep",
		"hash-object", "help", "init", "log", "ls-files", "ls-remote", "ls-tree", "merge",
		"merge-base", "mv", "name-rev", "notes", "pull", "push", "range-diff", "read-tree",
		"rebase", "reflog", "remote", "reset", "restore", "rev-list", "rev-parse", "revert",
		"rm", "shortlog", "show", "show-ref", "sparse-checkout", "stash", "status", "submodule",
		"switch", "symbolic-ref", "tag", "update-index", "update-ref", "var", "verify-commit",
		"verify-tag", "version", "whatchanged", "worktree", "write-tree":
		return true
	}
	return false
}

// gitConfigKeyForbidden reports a configuration key that names code: an alias,
// a pager, an editor, a helper program or an include.
func gitConfigKeyForbidden(kv string) bool {
	key := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		key = kv[:i]
	}
	key = strings.ToLower(key)
	// A driver, textconv, clean, smudge, process or command key of any name runs the program it gives (diff.<driver>.command,
	// merge.<driver>.driver, filter.<driver>.clean and the like).
	for _, suffix := range []string{".command", ".textconv", ".driver", ".cmd", ".clean", ".smudge", ".process"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	switch key {
	case "core.editor", "core.pager", "core.sshcommand", "core.fsmonitor", "core.hookspath",
		"core.askpass", "core.gitproxy", "diff.external", "sequence.editor", "gpg.program":
		return true
	}
	for _, p := range []string{"alias.", "pager.", "credential.", "difftool.", "mergetool.",
		"filter.", "gpg.", "include.", "includeif."} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// checkNpm refuses the npm option that runs a chosen shell.
func checkNpm(args []Word) error {
	for _, a := range args {
		if !a.Known {
			return unreadablef("npm argument is not known (%s)", a.Reason)
		}
		if a.Value == "--script-shell" || strings.HasPrefix(a.Value, "--script-shell=") {
			return unreadablef("npm --script-shell runs a shell the text does not show")
		}
	}
	return nil
}
