package hook

import (
	"strings"
	"testing"
)

// auditRow is one rule of the closed tables of the reader (wrappers, shell carriers, zsh and program-identity invalidators,
// code-bearing environment names, git -c keys): the command that exercises it and the verdict each gate must give.
type auditRow struct {
	id, cmd                  string
	memory, github, worktree string
}

// auditRows lists one row per rule and a control for the rules that have one.
func auditRows() []auditRow {
	return []auditRow{
		{"W-env-S", "env -S 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-env-u", "env -u HOME rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-env-i", "env -i rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-env-assign", "env A=1 rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-env-plain-control", "env A=1 echo hi", "none", "", "allow"},
		{"W-command", "command rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-command-v-control", "command -v rm", "none", "", "allow"},
		{"W-builtin", "builtin rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-exec-a", "exec -a x rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-exec-c", "exec -c rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-exec-l", "exec -l rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-nohup", "nohup rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-nice", "nice -n 5 rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-nice-unmodelled", "nice --bogus rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-ionice", "ionice -c 3 rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-timeout", "timeout 5 rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-timeout-unmodelled", "timeout --foreground 5 rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-time", "time rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-stdbuf", "stdbuf -oL rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-setsid", "setsid rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-sudo", "sudo rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-doas", "doas rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-xargs", "xargs rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-busybox", "busybox rm -rf {CHECKOUT}", "", "", "deny"},
		{"W-su-c", "su -c 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-find-exec", "find {CHECKOUT} -exec rm -rf {} +", "", "", "deny"},
		{"W-find-execdir", "find {CHECKOUT} -execdir rm -rf {} +", "", "", "deny"},
		{"W-find-delete", "find {CHECKOUT} -delete", "", "", "deny"},
		{"W-parallel", "parallel rm -rf ::: {CHECKOUT}", "", "", "deny"},
		{"W-bash-c", "bash -c 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-sh-c", "sh -c 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-dash-c", "dash -c 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-zsh-c", "zsh -c 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-pipe-shell", "printf 'rm -rf {CHECKOUT}' | bash", "", "", "deny"},
		{"W-here-string-shell", "bash <<< 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-heredoc-shell", "bash <<EOF\nrm -rf {CHECKOUT}\nEOF", "", "", "deny"},
		{"W-procsub-shell", "bash < <(echo 'rm -rf {CHECKOUT}')", "", "", "deny"},
		{"W-eval", "eval 'rm -rf {CHECKOUT}'", "", "", "deny"},
		{"W-source-procsub", "source <(echo 'rm -rf {CHECKOUT}')", "", "", "deny"},
		{"W-trap", "trap 'rm -rf {CHECKOUT}' EXIT; true", "", "", "deny"},
		{"Z-zsh-word", "=ls -la", "attempt", "", "deny"},
		{"Z-repeat", "repeat 2 echo x", "attempt", "", "deny"},
		{"Z-foreach", "foreach x (a b); echo $x; end", "attempt", "", "deny"},
		{"Z-setopt", "setopt xtrace", "attempt", "", "deny"},
		{"Z-set-o-operand", "set -o noclobber", "attempt", "", "deny"},
		{"Z-set-o-control", "set -o", "none", "", "allow"},
		{"Z-alias", "alias rm='rm -rf'", "attempt", "", "deny"},
		{"Z-hash-operand", "hash ls", "attempt", "", "deny"},
		{"Z-hash-control", "hash -r", "none", "", "allow"},
		{"Z-path-assign", "PATH=/tmp/x:$PATH ls", "attempt", "", "deny"},
		{"Z-path-prefix", "PATH=/tmp/x ls", "attempt", "", "deny"},
		{"Z-path-lower", "path=(/tmp) ls", "attempt", "", "deny"},
		{"E-BASH_ENV", "BASH_ENV=/tmp/x bash -c 'true'", "attempt", "", "deny"},
		{"E-ENV", "ENV=/tmp/x sh -c 'true'", "attempt", "", "deny"},
		{"E-ZDOTDIR", "ZDOTDIR=/tmp/x zsh -c 'true'", "attempt", "", "deny"},
		{"E-GIT_EDITOR", "GIT_EDITOR=vi git log", "", "", "deny"},
		{"E-GIT_SEQUENCE_EDITOR", "GIT_SEQUENCE_EDITOR=vi git rebase -i HEAD~1", "", "", "deny"},
		{"E-GIT_SSH_COMMAND", "GIT_SSH_COMMAND=x git fetch", "", "", "deny"},
		{"E-PAGER", "PAGER=cat man ls", "attempt", "", "deny"},
		{"E-GIT_PAGER", "GIT_PAGER=cat git log", "", "", "deny"},
		{"E-LD_PRELOAD", "LD_PRELOAD=/tmp/x.so ls", "attempt", "", "deny"},
		{"E-LD_LIBRARY_PATH", "LD_LIBRARY_PATH=/tmp ls", "attempt", "", "deny"},
		{"E-PYTHONSTARTUP", "PYTHONSTARTUP=/tmp/x python3 -c 'print(1)'", "attempt", "", "deny"},
		{"E-PYTHONPATH", "PYTHONPATH=/tmp python3 -c 'print(1)'", "attempt", "", "deny"},
		{"E-NODE_OPTIONS", "NODE_OPTIONS=--require=/tmp/x node -e '1'", "attempt", "", "deny"},
		{"E-RUBYOPT", "RUBYOPT=-r/tmp/x ruby -e '1'", "attempt", "", "deny"},
		{"E-PERL5OPT", "PERL5OPT=-M/tmp/x perl -e '1'", "attempt", "", "deny"},
		{"G-alias", "git -c alias.x='!rm -rf {CHECKOUT}' x", "", "", "deny"},
		{"G-core.pager", "git -c core.pager='sh -c \"rm -rf {CHECKOUT}\"' log", "", "", "deny"},
		{"G-core.editor", "git -c core.editor=vi commit", "", "", "deny"},
		{"G-core.sshCommand", "git -c core.sshCommand=x fetch", "", "", "deny"},
		{"G-core.fsmonitor", "git -c core.fsmonitor=x status", "", "", "deny"},
		{"G-core.hooksPath", "git -c core.hooksPath=/tmp status", "", "", "deny"},
		{"G-credential.helper", "git -c credential.helper=x fetch", "", "", "deny"},
		{"G-diff.external", "git -c diff.external=x diff", "", "", "deny"},
		{"G-pager.log", "git -c pager.log=x log", "", "", "deny"},
		{"G-sequence.editor", "git -c sequence.editor=x rebase -i HEAD~1", "", "", "deny"},
		{"G-gpg.program", "git -c gpg.program=x commit -S", "", "", "deny"},
		{"G-exec-path", "git --exec-path=/tmp status", "", "", "deny"},
		{"G-user.name-control", "git -c user.name=x status", "", "", "allow"},
		{"G-status-control", "git status", "", "", "allow"},
		{"N-script-shell", "npm --script-shell=/bin/sh run x", "", "", "deny"},
		{"N-run-residual-control", "npm run build", "", "", "allow"},
		{"M-make-residual-control", "make test", "", "", "allow"},
		{"M-go-test-residual-control", "go test ./...", "", "", "allow"},
	}
}

// TestAuditRows checks every rule row against the gates it names. {MEMORY} and {CHECKOUT} are the rig's own directories.
func TestAuditRows(t *testing.T) {
	r := newDelRig(t)
	cwd, root, env := gateScene(t)
	fill := strings.NewReplacer("{MEMORY}", root, "{CHECKOUT}", r.checkout, "{SLOT}", r.slotRoot)
	for _, row := range auditRows() {
		cmd := fill.Replace(row.cmd)
		if row.memory != "" {
			if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env).Surface != ""; got != (row.memory == "attempt") {
				t.Errorf("%s memory gate: attempt=%v, want %s: %q", row.id, got, row.memory, cmd)
			}
		}
		if row.worktree != "" {
			if denied := r.verdict(cmd).Deny; denied != (row.worktree == "deny") {
				t.Errorf("%s worktree guard: denied=%v, want %s: %q", row.id, denied, row.worktree, cmd)
			}
		}
	}
}
