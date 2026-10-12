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

func TestGithubPostGuardAllowsAWrapperThatNamesNoPost(t *testing.T) {
	cwd, _, _ := gateScene(t)
	wrapper := crw1283Script(t, cwd, "heavy_slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	for _, command := range []string{wrapper + " go version", "bash " + wrapper + " go version"} {
		if site, denied := githubPostJudgeText(command, cwd); denied {
			t.Errorf("%s: refused: %+v", command, site)
		}
	}
	for _, command := range []string{wrapper + " gh pr comment 1 --body x", wrapper + " git push origin dev"} {
		if _, denied := githubPostJudgeText(command, cwd); !denied {
			t.Errorf("post escaped wrapper: %s", command)
		}
	}
}

func TestMemoryWriteGateReadsAReadableScriptFile(t *testing.T) {
	cwd, root, env := gateScene(t)
	script := crw1283Script(t, cwd, "check.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, "bash "+script+" go version"), env); got != "" {
		t.Fatalf("readable script refused: %s", got)
	}
	for _, command := range []string{"bash " + script + " tee " + root + "/note.md", "bash " + script + " sh -c 'echo x >\"" + root + "/note.md\"'"} {
		if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got == "" {
			t.Fatalf("memory write escaped script: %s", command)
		}
	}
}

func TestCommandReaderPathAssignmentResolvesTheProgram(t *testing.T) {
	cwd, root, env := gateScene(t)
	program := crw1283Script(t, cwd, "check", "#!/bin/bash\nprintf ok\n")
	for _, prefix := range []string{"PATH=" + cwd + " ", "export PATH=" + cwd + "; ", "export 'PATH=" + cwd + "'; ", "env PATH=" + cwd + " "} {
		command := prefix + "check"
		res, err := shellir.Analyze(command, cwd)
		if err != nil {
			t.Errorf("%s: %v", command, err)
			continue
		}
		found := false
		for _, exec := range res.Execs {
			found = found || exec.Program.Value == program
		}
		if !found {
			t.Errorf("%s: did not resolve %s: %+v", command, program, res.Execs)
		}
		if _, denied := githubPostJudgeText(command, cwd); denied {
			t.Errorf("benign PATH program refused: %s", command)
		}
		if got := HandleMemoryWriteGate(gateBash(t, cwd, command), env); got != "" {
			t.Errorf("benign PATH program refused by memory gate: %s", got)
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
	for _, command := range []string{"PATH=$UNKNOWN /usr/bin/true", "env PATH=$UNKNOWN /usr/bin/true"} {
		if _, err := shellir.Analyze(command, cwd); err != nil {
			t.Errorf("absolute program does not use unknown PATH: %s: %v", command, err)
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

func TestCRW1283DirectForwarderDoesNotHideAMemoryWrite(t *testing.T) {
	cwd, root, env := gateScene(t)
	wrapper := crw1283Script(t, cwd, "slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	if got := HandleMemoryWriteGate(gateBash(t, cwd, wrapper+" go version"), env); got != "" {
		t.Fatalf("harmless direct forwarder refused: %s", got)
	}
	if got := HandleMemoryWriteGate(gateBash(t, cwd, wrapper+" tee "+root+"/note.md"), env); got == "" {
		t.Fatal("direct forwarder hid a protected tee destination")
	}
}

func TestCRW1283DirectForwarderDoesNotHideWorktreeRemoval(t *testing.T) {
	rig := newDelRig(t)
	wrapper := crw1283Script(t, rig.checkout, "slot.sh", "#!/bin/bash\nionice -c3 nice \"$@\"\n")
	if got := rig.verdict(wrapper + " go version"); got.Deny {
		t.Fatalf("harmless direct forwarder refused: %+v", got)
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
