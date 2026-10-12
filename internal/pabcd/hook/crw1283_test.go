package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

func crw1283Script(t *testing.T, cwd, name, body string) string {
	t.Helper()
	filename := filepath.Join(cwd, name)
	if err := os.WriteFile(filename, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return filename
}

// TestGithubPostGuardAllowsAWrapperThatNamesNoPost (plan C, Sion 10-13, decided answer 2): the program wrapper ionice -c3 nice in
// front of a command that names no post is read and allowed; a script wrapper that forwards "$@" (heavy_slot.sh) is not read and is
// refused, with a reason that names the path the user can follow, which no guard blocks.
func TestGithubPostGuardAllowsAWrapperThatNamesNoPost(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, command := range []string{"ionice -c3 nice go version", "ionice -c3 nice -n 5 go test ./...", "nice ionice -c3 go version"} {
		if site, denied := githubPostJudgeText(command, cwd); denied {
			t.Errorf("%s: refused: %+v", command, site)
		}
	}
	// git push run directly, or behind the program wrappers, keeps the policy it had at c007bd50: the GitHub guard does not judge it.
	// Inside a script file (bash push.sh) it is refused, and behind an assigned PATH the assignment is refused.
	for _, command := range []string{"git push origin dev", "ionice -c3 nice git push origin dev"} {
		if site, denied := githubPostJudgeText(command, cwd); denied {
			t.Errorf("%s: git push run directly is refused, unlike at c007bd50: %+v", command, site)
		}
	}
	// A gh post behind the program wrappers is refused.
	for _, command := range []string{"ionice -c3 nice gh pr comment 1 --body x", "nice -n 5 gh api -X POST repos/o/r/issues"} {
		if _, denied := githubPostJudgeText(command, cwd); !denied {
			t.Errorf("post escaped the program wrapper: %s", command)
		}
	}
	wrapper := crw1283Script(t, cwd, "heavy_slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	for _, command := range []string{wrapper + " go version", "bash " + wrapper + " go version", wrapper + " gh pr comment 1 --body x",
		wrapper + " git push origin dev"} {
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, command)))
		if !strings.Contains(reason, "run the forwarded command directly") {
			t.Errorf("the \"$@\" script wrapper was not refused with the followable path: %s: %q", command, reason)
		}
	}
	// The path the refusal names: the forwarded command run directly, behind the program wrappers if the user wants them.
	direct := "ionice -c3 nice go version"
	if got := HandleMemoryWriteGate(gateBash(t, cwd, direct), env); got != "" {
		t.Errorf("the memory gate refused the path the refusal names: %s", got)
	}
	rig := newDelRig(t)
	if got := rig.verdict(direct); got.Deny {
		t.Errorf("the worktree guard refused the path the refusal names: %+v", got)
	}
}

// TestMemoryWriteGateReadsAReadableScriptFile (decided answer 3): the memory gate reads a readable script file run as bash f.sh or sh
// f.sh; a script that writes memory is refused; a script whose destination the gate cannot place (a "$@" forward, plan C) is refused
// with a reason that names a path the user can follow, which neither the GitHub nor the worktree guard blocks.
func TestMemoryWriteGateReadsAReadableScriptFile(t *testing.T) {
	cwd, root, env := gateScene(t)
	script := crw1283Script(t, cwd, "check.sh", "#!/bin/bash\nionice -c3 nice go version\n")
	for _, command := range []string{"bash " + script, "sh " + script} {
		if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got != "" {
			t.Fatalf("readable script refused: %s: %s", command, got)
		}
	}
	writer := crw1283Script(t, cwd, "writer.sh", "#!/bin/bash\ntee "+root+"/note.md </dev/null\nsh -c 'echo x >\""+root+"/note.md\"'\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, "bash "+writer), env); got == "" {
		t.Fatal("memory write escaped script")
	}
	forwarder := crw1283Script(t, cwd, "slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	for _, command := range []string{"bash " + forwarder + " go version", "bash " + forwarder + " tee " + root + "/note.md",
		"bash " + forwarder + " sh -c 'echo x >\"" + root + "/note.md\"'"} {
		reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, command), env))
		if !strings.Contains(reason, "Use a direct command with a literal program") {
			t.Fatalf("the refusal of a \"$@\" script names no path: %s: %s", command, reason)
		}
	}
	direct := "ionice -c3 nice go version"
	if site, denied := githubPostJudgeText(direct, cwd); denied {
		t.Errorf("the GitHub guard refused the path the refusal names: %+v", site)
	}
	if got := newDelRig(t).verdict(direct); got.Deny {
		t.Errorf("the worktree guard refused the path the refusal names: %+v", got)
	}
}

// TestCommandReaderPathAssignmentResolvesTheProgram (decided answer 4, split (1), Sion 10-13): CRW-1283 refuses every PATH assignment
// as c007bd50 did, a literal one in front of a harmless program included (a named limit; the literal PATH reading is CRW-1292); a
// gh, a script that posts and a memory writer the assigned PATH would select stay refused, and a value that is not literal too.
func TestCommandReaderPathAssignmentResolvesTheProgram(t *testing.T) {
	cwd, root, env := gateScene(t)
	rig := newDelRig(t)
	crw1283Script(t, cwd, "check", "#!/bin/bash\nprintf ok\n")
	for _, prefix := range []string{"PATH=" + cwd + " ", "export PATH=" + cwd + "; ", "export 'PATH=" + cwd + "'; ", "env PATH=" + cwd + " ",
		"X=1 export PATH=" + cwd + "; ", "PATH=/usr/bin:/bin "} {
		command := prefix + "check"
		if _, err := shellir.Analyze(command, cwd); err == nil || !strings.Contains(err.Error(), "assignment to PATH changes which program runs") {
			t.Errorf("%s: the PATH assignment was read, refused as at c007bd50 until CRW-1292: %v", command, err)
		}
		if _, denied := githubPostJudgeText(command, cwd); !denied {
			t.Errorf("GitHub guard allowed a PATH assignment: %s", command)
		}
		if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got == "" {
			t.Errorf("memory gate allowed a PATH assignment: %s", command)
		}
		if got := rig.verdict(command); !got.Deny {
			t.Errorf("worktree guard allowed a PATH assignment: %s", command)
		}
	}
	crw1283Script(t, cwd, "gh", "#!/bin/bash\nprintf ok\n")
	crw1283Script(t, cwd, "hidden", "#!/bin/bash\ngh pr comment 1 --body x\n")
	crw1283Script(t, cwd, "writer", "#!/bin/bash\necho x >"+root+"/note.md\n")
	for _, command := range []string{"PATH=" + cwd + " gh pr comment 1 --body x", "PATH=" + cwd + " hidden", "PATH=$UNKNOWN gh pr comment 1 --body x"} {
		if _, denied := githubPostJudgeText(command, cwd); !denied {
			t.Errorf("post escaped PATH: %s", command)
		}
	}
	if got := HandleMemoryWriteGate(gateBash(t, cwd, "PATH="+cwd+" writer"), env); got == "" {
		t.Fatal("memory writer escaped PATH")
	}
	// A PATH value that is not literal is refused as well, even in front of an absolute program.
	for _, command := range []string{"PATH=$UNKNOWN /usr/bin/true", "env PATH=$UNKNOWN /usr/bin/true", "PATH=" + cwd + ":$PATH check"} {
		if _, err := shellir.Analyze(command, cwd); err == nil {
			t.Errorf("a PATH value that is not literal was read: %s", command)
		}
	}
}

// TestCRW1283PathDeclarationNameSpelledInParts (evaluation 882ffa84 d2): a declaration word whose value the reader does not know assigns
// PATH when its name is spelled in quoted and unquoted parts ("P"ATH=$D, P"ATH"=$D, "PATH"=$D) or starts with a spelled part of PATH and
// is completed at run time (P$X=$D), as PATH=$D does; every guard refuses it as every other PATH assignment. A name that is not PATH,
// however it is spelled, is read as before.
func TestCRW1283PathDeclarationNameSpelledInParts(t *testing.T) {
	cwd, root, env := gateScene(t)
	rig := newDelRig(t)
	crw1283Script(t, cwd, "hidden", "#!/bin/bash\ngh pr comment 1 --body x\n")
	for _, declaration := range []string{`export "P"ATH=$D`, `export P"ATH"=$D`, `export "PATH"=$D`, `export 'PA'"TH"="$D"`,
		`export "P"ATH+=$D`, `declare -x "P"ATH=$D`, `typeset "PATH"=$D`, `X=1 export "P"ATH=$D`, `export P$X=$D`,
		`export "PA$X"=$D`, `export 'path=/d'`, `export "pa"th=$D`} {
		for _, tail := range []string{"; hidden", "; printf ok", "; tee " + root + "/note.md </dev/null"} {
			command := `D="$CRW1283_UNSET"; ` + declaration + tail
			if _, err := shellir.Analyze(command, cwd); err == nil || !strings.Contains(err.Error(), "changes which program runs") {
				t.Errorf("%s: the PATH assignment was read: %v", command, err)
			}
			if _, denied := githubPostJudgeText(command, cwd); !denied {
				t.Errorf("GitHub guard allowed a PATH assignment: %s", command)
			}
			if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got == "" {
				t.Errorf("memory gate allowed a PATH assignment: %s", command)
			}
			if got := rig.verdict(command); !got.Deny {
				t.Errorf("worktree guard allowed a PATH assignment: %s", command)
			}
		}
		inFunction := `f() { local ` + strings.TrimPrefix(strings.TrimPrefix(declaration, "export "), "declare -x ") + `; printf ok; }; f`
		if !strings.HasPrefix(declaration, "X=1") && !strings.HasPrefix(declaration, "typeset") {
			if _, err := shellir.Analyze(inFunction, cwd); err == nil {
				t.Errorf("%s: the PATH assignment was read", inFunction)
			}
		}
	}
	// Controls: a declaration of another name, spelled in parts or not, is read as before.
	for _, command := range []string{`D="$CRW1283_UNSET"; export "MY"PATH=$D; printf ok`, `D="$CRW1283_UNSET"; export "P"ATHS=$D; printf ok`,
		`D="$CRW1283_UNSET"; export "GO"FLAGS=$D; printf ok`, `export "PATH_EXTRA"=x; printf ok`} {
		if _, err := shellir.Analyze(command, cwd); err != nil {
			t.Errorf("%s: a declaration of another name was refused: %v", command, err)
		}
		if _, denied := githubPostJudgeText(command, cwd); denied {
			t.Errorf("GitHub guard refused a declaration of another name: %s", command)
		}
		if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got != "" {
			t.Errorf("memory gate refused a declaration of another name: %s: %s", command, got)
		}
		if got := rig.verdict(command); got.Deny {
			t.Errorf("worktree guard refused a declaration of another name: %s", command)
		}
	}
}

func TestGuardRefusalNamesAPathAnotherGuardAllows(t *testing.T) {
	cwd, _, env := gateScene(t)
	reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, "echo x >\"$UNKNOWN\""), env))
	if !strings.Contains(reason, "direct command with a literal program and absolute write paths") {
		t.Fatalf("no actionable direct-command route: %s", reason)
	}
	githubReason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash -c \"$UNKNOWN\"")))
	if !strings.Contains(githubReason, "direct command with a literal program and absolute write paths") {
		t.Fatalf("GitHub reader has no actionable direct-command route: %s", githubReason)
	}
	command := "printf '%s\\n' ok >" + filepath.Join(cwd, "out.txt")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got != "" {
		t.Fatalf("recommended route refused by memory gate: %s", got)
	}
	if site, denied := githubPostJudgeText(command, cwd); denied {
		t.Fatalf("recommended route refused by GitHub guard: %+v", site)
	}
}

func TestCRW1283ScriptArgumentsDoNotLeakIntoAnotherShell(t *testing.T) {
	cwd, root, env := gateScene(t)
	post := crw1283Script(t, cwd, "post.sh", "#!/bin/bash\nbash -c '\"$@\"' zero gh pr comment 1 --body x\n")
	if _, denied := githubPostJudgeText("bash "+post+" go version", cwd); !denied {
		t.Fatal("the inner shell's posting arguments were replaced by the outer arguments")
	}
	writer := crw1283Script(t, cwd, "writer.sh", "#!/bin/bash\nbash -c '\"$@\"' zero tee "+root+"/note.md\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, "bash "+writer+" go version"), env); got == "" {
		t.Fatal("the inner shell's memory-writing arguments were replaced by the outer arguments")
	}
}

func TestCRW1283ScriptArgumentsDoNotLeakIntoFunctionBodies(t *testing.T) {
	cwd, _, _ := gateScene(t)
	script := crw1283Script(t, cwd, "function.sh", "#!/bin/bash\nf() { \"$@\"; }\nf gh pr comment 1 --body x\n")
	if _, denied := githubPostJudgeText("bash "+script+" go version", cwd); !denied {
		t.Fatal("a function used the script's arguments instead of its own")
	}
}

// Plan C: a direct forwarder is read by the memory and worktree guards so that its "$@" refuses it, whatever it forwards.
func TestCRW1283DirectForwarderDoesNotHideAMemoryWrite(t *testing.T) {
	cwd, root, env := gateScene(t)
	wrapper := crw1283Script(t, cwd, "slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, wrapper+" go version"), env); got == "" {
		t.Fatal("a direct forwarder was allowed although its \"$@\" is not read")
	}
	if got := HandleMemoryWriteGate(gateBash(t, cwd, wrapper+" tee "+root+"/note.md"), env); got == "" {
		t.Fatal("direct forwarder hid a protected tee destination")
	}
}

func TestCRW1283DirectForwarderDoesNotHideWorktreeRemoval(t *testing.T) {
	rig := newDelRig(t)
	wrapper := crw1283Script(t, rig.checkout, "slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	if got := rig.verdict(wrapper + " go version"); !got.Deny {
		t.Fatal("a direct forwarder was allowed although its \"$@\" is not read")
	}
	if got := rig.verdict(wrapper + " rm -rf " + rig.checkout); !got.Deny {
		t.Fatal("direct forwarder hid a protected worktree removal")
	}
}

func TestCRW1283SourceInheritsArgumentsAndAssignedPath(t *testing.T) {
	cwd, root, env := gateScene(t)
	crw1283Script(t, cwd, "body.sh", "#!/bin/bash\n\"$@\"\n")
	entry := crw1283Script(t, cwd, "source.sh", "#!/bin/bash\nsource "+cwd+"/body.sh\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, "bash "+entry+" tee "+root+"/note.md"), env); got == "" {
		t.Fatal("source lost the invoking script's protected arguments")
	}
	crw1283Script(t, cwd, "lib.sh", "printf ok\n")
	program := crw1283Script(t, cwd, "check", "#!/bin/bash\nsource lib.sh\n")
	if _, denied := githubPostJudgeText("PATH="+cwd+" "+program, cwd); !denied {
		t.Fatal("source searched an inherited PATH without proving the loaded file")
	}
}

func TestCRW1283AssignedPathDoesNotHideADirectScript(t *testing.T) {
	cwd, root, env := gateScene(t)
	writer := crw1283Script(t, cwd, "write.sh", "#!/bin/bash\necho x >"+root+"/note.md\n")
	for _, prefix := range []string{"PATH=/usr/bin:/bin ", "PATH=$UNKNOWN "} {
		if got := HandleMemoryWriteGate(gateBash(t, cwd, prefix+writer), env); got == "" {
			t.Errorf("PATH relaxation concealed a direct script's memory write: %s", prefix)
		}
	}
	rig := newDelRig(t)
	remover := crw1283Script(t, rig.checkout, "remove.sh", "#!/bin/bash\nrm -rf "+rig.checkout+"\n")
	if got := rig.verdict("PATH=/usr/bin:/bin " + remover); !got.Deny {
		t.Error("PATH relaxation concealed a direct script's worktree removal")
	}
}
