package hook

import (
	"strings"
	"testing"
)

// The two findings of the final Opus verification of 418273ef (opusverify-final-418273ef-f1): a positional assignment word
// (zsh: 1=x, 2+=x) inside a loop must end the arguments the reader follows, and a loop whose body only assigns variables must not.
// No test runs the command.

// crw1283HeavySlot is .omo/mgmt/tools/heavy_slot.sh as the repository holds it (10-12): the wrapper of the CRW-1283 reproduction,
// whose "$@" forward sits in a while loop that assigns variables (start, rc).
const crw1283HeavySlot = `#!/usr/bin/env bash
# Host entry gate for heavy checks (go test / go build / go vet / make check). Management 10-07 17:5x (Jun: current
# parallelism is the maximum; optimise tests). Reversible: raise /var/tmp/crw-heavy-slots/limit or stop using it.
# - one run per caller (TMPDIR, else cwd) at a time: a second run from the same worktree waits for the first;
# - at most N runs on the host (N in the limit file, default 10); a new run waits for a free slot, nothing is killed;
# - Go's build cache goes to /scratch (idle NVMe) unless the caller set it. GOTMPDIR is left as the caller has it
#   (unset means Go uses TMPDIR): a test that masks temporary paths by TMPDIR failed when GOTMPDIR differed
#   (10-07 19:0x, internal/relay/store TestSchemaConformance_python_properties, found by CRW-943). The test TMPDIR
#   is left alone too (socket path length);
# - a lane's command (caller without CRW_HEAVY_PRIORITY) runs at nice +12 and best-effort I/O priority 7.
# - CRW_HEAVY_PRIORITY=integrator (set by /state/crw/integration/heavy-gate.sh): the integrator's CI steps may also use
#   R reserved slots (file reserve, default 4) that lanes never take, and add no nice (the queue runner already runs
#   at +5), so the merge path (the
#   bottleneck) neither waits for lane runs nor loses the CPU to them under load (10-10: load 80, 50-minute integrations
#   failing on timing flakes).
# usage: heavy_slot.sh <command> [args...]
set -u
DIR=/var/tmp/crw-heavy-slots
mkdir -p "$DIR"
N=$(cat "$DIR/limit" 2>/dev/null || echo 10)
R=$(cat "$DIR/reserve" 2>/dev/null || echo 4)
if [ "${CRW_HEAVY_PRIORITY:-}" = integrator ]; then TOP=$((N + R)); NI=0; else TOP=$N; NI=12; fi
owner="${TMPDIR:-$PWD}"
okey=$(printf '%s' "$owner" | sha1sum | cut -c1-12)
exec 8>"$DIR/owner-$okey.lock"
flock 8
export GOCACHE="${GOCACHE:-/scratch/crw/shared/gocache}"
case " ${GOFLAGS:-} " in *" -p="*) ;; *) export GOFLAGS="${GOFLAGS:+$GOFLAGS }-p=4" ;; esac
while :; do
  for i in $(seq 1 "$TOP"); do
    exec 9>"$DIR/slot-$i.lock"
    if flock -n 9; then
      start=$(date +%s)
      echo "$(date -u +%FT%TZ) start slot=$i owner=$owner pid=$$ cmd=$*" >> "$DIR/log"
      # The locks stay with this shell only: a command's children (e.g. "go list | grep -q .") must not inherit fds 8/9, or an
      # orphaned child keeps the owner lock after this shell dies and every later run of the same owner waits forever (10-10).
      nice -n "$NI" ionice -c2 -n7 "$@" 8>&- 9>&-; rc=$?
      echo "$(date -u +%FT%TZ) end slot=$i owner=$owner rc=$rc secs=$(( $(date +%s) - start ))" >> "$DIR/log"
      exit $rc
    fi
    exec 9>&-
  done
  sleep 5
done
`

// TestReview1283LoopPositionalChange: a loop runs "$@" and then changes the positional parameters (a zsh positional assignment word,
// an argv array assignment, set, shift, a positional loop variable): a later iteration runs other arguments than the first, so the
// reader must not read "$@" as the arguments the script was given. Each form was refused before the relaxations (c007bd50, which
// refused every "$@"), and a guard whose target the changed arguments reach refuses it now (Sion decision 5).
func TestReview1283LoopPositionalChange(t *testing.T) {
	cases := []struct {
		name, body, args string // %s is the protected command
	}{
		{"posassign_while", "while :; do nice \"$@\"; 1=env; done\n", "true %s"},
		{"posassign_until", "until false; do ionice -c3 \"$@\"; 1=env; done\n", "true %s"},
		{"posassign_timeout", "while :; do timeout 5 \"$@\"; 1=env; done\n", "true %s"},
		{"posassign_env", "while :; do env \"$@\"; 1=env; done\n", "true %s"},
		{"posassign_append", "while :; do nice \"$@\"; 2+=x; done\n", "true %s"},
		{"posassign_before_forward", "while :; do 1=env; nice \"$@\"; done\n", "true %s"},
		{"posassign_in_if", "while :; do nice \"$@\"; if :; then 1=env; fi; done\n", "true %s"},
		{"posassign_condition", "while nice \"$@\" && 1=env; do :; done\n", "true %s"},
		{"posassign_in_function", "f() { 1=env; }\nwhile :; do nice \"$@\"; f; done\n", "true %s"},
		{"posassign_after_variable", "while :; do x=1; nice \"$@\"; 1=env; done\n", "true %s"},
		{"posassign_subscript", "while :; do nice \"$@\"; argv[1]=env; done\n", "true %s"},
		{"argv_array", "while :; do nice \"$@\"; argv=(env %s); done\n", "true"},
		{"argv_array_after_variable", "while :; do x=1; nice \"$@\"; argv=(env %s); done\n", "true"},
		{"set_dashdash", "while :; do nice \"$@\"; set -- %s; done\n", "true"},
		{"shift", "while :; do nice \"$@\"; shift; done\n", "true %s"},
		{"eval_shift", "while :; do nice \"$@\"; eval shift; done\n", "true %s"},
		{"command_shift", "while :; do nice \"$@\"; command shift; done\n", "true %s"},
		{"for_positional", "while :; do nice \"$@\"; for 1 in env; do :; done; done\n", "true %s"},
		{"read_argv", "while :; do nice \"$@\"; read -A argv <<<'env %s'; done\n", "true"},
		{"typeset_argv", "while :; do nice \"$@\"; typeset argv=(env %s); done\n", "true"},
		{"nameref_argv", "while :; do nice \"$@\"; declare -n r=argv; r=(env %s); done\n", "true"},
		{"arith_positional", "while :; do nice \"$@\"; (( 1 = 3 )); done\n", "true %s"},
		{"default_assign_positional", "while :; do nice \"$@\"; : ${1:=env}; done\n", "true %s"},
		{"dynamic_name", "while :; do nice \"$@\"; n=1; read \"$n\" <<<env; done\n", "true %s"},
		{"dynamic_command", "while :; do nice \"$@\"; $CMD; done\n", "true %s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				body := strings.ReplaceAll(c.body, "%s", target.command)
				args := strings.ReplaceAll(c.args, "%s", target.command)
				script := crw1283Script(t, target.dir, "loopmut.sh", "#!/bin/bash\n"+body)
				commands := []string{"bash " + script + " " + args, "zsh " + script + " " + args}
				if target.name == "github" {
					// the memory and worktree guards do not read a script run by its path unless it forwards "$@" (c007bd50 and the
					// head alike: the open item CRW-1287, pinned by the differential fuzz)
					commands = append(commands, script+" "+args)
				}
				for _, command := range commands {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED %q after a positional change in a loop (body %q)", target.name, command, body)
					}
				}
			}
		})
	}
}

// TestReview1283LoopWrapperForwardsArguments: plan C (Sion 10-13) withdrew the reading of a forwarded "$@": a loop wrapper (the shape
// of heavy_slot.sh, the CRW-1283 reproduction) is refused with a reason that names the path the user can follow, and a protected
// command forwarded the same way stays refused.
func TestReview1283LoopWrapperForwardsArguments(t *testing.T) {
	bodies := map[string]string{
		"assign_in_while": "while :; do x=1; nice \"$@\"; exit 0; done\n",
		"assign_after":    "while :; do nice \"$@\"; rc=$?; exit $rc; done\n",
		"assign_prefix":   "until false; do X=1 nice \"$@\"; rc=$?; break; done\n",
		"for_loop":        "for i in 1 2; do nice \"$@\"; done\n",
		"declare_in_loop": "while :; do declare -i n=1; local_x=2; ionice -c3 nice \"$@\"; exit; done\n",
		"arith_in_loop":   "while :; do n=$((n + 1)); timeout 600 \"$@\"; exit; done\n",
		"nested_loops":    "for i in 1 2; do while :; do start=$(date +%s); nice \"$@\"; rc=$?; break; done; done\n",
		"heavy_slot":      crw1283HeavySlot,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			gh, _, _ := gateScene(t)
			script := crw1283Script(t, gh, "slot.sh", "#!/bin/bash\n"+body)
			for _, command := range []string{
				script + " go version",
				"bash " + script + " go version",
				"GOFLAGS=-p=4 TZ=UTC TMPDIR=/scratch/lt/it-guard-rd " + script + " go version",
				"GOFLAGS=-p=4 TZ=UTC TMPDIR=/scratch/lt/it-guard-rd bash " + script + " go version",
			} {
				// Plan C (Sion 10-13): the forwarded "$@" is not followed; the issue's reproduction is refused as a named limit with
				// the path the user can follow (trusted wrappers: CRW-1290).
				if site, denied := githubPostJudgeText(command, gh); !denied || !strings.Contains(site.reason, "run the forwarded command directly") {
					t.Errorf("github guard did not refuse a loop wrapper with the followable path: %s: %+v", command, site)
				}
			}
			for _, command := range []string{script + " gh pr comment 1 --body x", "bash " + script + " git push origin dev"} {
				if _, denied := githubPostJudgeText(command, gh); !denied {
					t.Errorf("github guard ALLOWED a post forwarded by a loop wrapper: %s", command)
				}
			}
		})
	}
}

// TestReview1283LoopWrapperMemoryAndWorktree: the memory and worktree guards read a loop wrapper like the GitHub guard does and, since
// plan C, refuse it whatever it forwards, the protected command included.
func TestReview1283LoopWrapperMemoryAndWorktree(t *testing.T) {
	bodies := map[string]string{
		"assign_in_while": "while :; do x=1; nice \"$@\"; exit 0; done\n",
		"assign_after":    "while :; do nice \"$@\"; rc=$?; exit $rc; done\n",
		"for_loop":        "for i in 1 2; do ionice -c3 nice \"$@\"; done\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			for _, target := range crw1283Targets(t) {
				script := crw1283Script(t, target.dir, "loopslot.sh", "#!/bin/bash\n"+body)
				if target.name == "github" {
					continue
				}
				for _, command := range []string{"bash " + script + " go version", script + " go version"} {
					if !target.refused(command) {
						t.Errorf("%s guard allowed a \"$@\" loop wrapper (plan C): %s", target.name, command)
					}
				}
				for _, command := range []string{"bash " + script + " " + target.command, script + " " + target.command} {
					if !target.refused(command) {
						t.Errorf("%s guard ALLOWED the protected command forwarded by a loop wrapper: %s", target.name, command)
					}
				}
			}
		})
	}
}
