package hook

import "testing"

// TestWorktreeDelInlineProgramRows: in a managed worktree an interpreter's inline program that removes a file, starts
// another program or reaches a name only at run time is refused; an ordinary program is allowed.
func TestWorktreeDelInlineProgramRows(t *testing.T) {
	r := newDelRig(t)
	denied := []string{
		`python3 -c 'from pathlib import Path; Path("x").rmdir()'`,
		`python3 -c 'from pathlib import Path; Path("x").unlink()'`,
		`python3 -c 'import os; os.removedirs("d")'`,
		`python3 -c 'import os; os.remove("x")'`,
		`python3 -c 'import shutil; getattr(shutil,"rm"+"tree")("d")'`,
		`python3 -c 'import subprocess; subprocess.run(["rm","-rf","x"])'`,
		`python3 -c 'import os; os.system("rm -rf x")'`,
		`node -e 'require("child_process").execSync("rm -rf x")'`,
		`node -e 'require("fs").promises.rm("x",{recursive:true})'`,
		`node -e 'require("fs").rm("x",{recursive:true},()=>{})'`,
		`node -e 'require("node:fs").promises.rm("x")'`,
		`node -e 'require("fs").rmSync("x")'`,
		`node -e 'require("fs")["rm"+"Sync"]("x")'`,
		`perl -e 'unlink "x"'`,
		`perl -e 'use File::Path; remove_tree("d")'`,
		`perl -e 'system("rm -rf x")'`,
		`ruby -e 'require "fileutils"; FileUtils.rm_rf("x")'`,
		`ruby -e 'Dir.rmdir("x")'`,
		`ruby -e 'system("rm -rf x")'`,
		`python3 -c 'import builtins; vars(builtins)'`,
		`python3 -c 'import importlib; importlib.import_module("os")'`,
	}
	for _, cmd := range denied {
		if !r.verdict(cmd).Deny {
			t.Errorf("%s: allowed in a managed worktree, want refused", cmd)
		}
	}
	allowed := []string{
		`python3 -c 'print(1+1)'`,
		`python3 -c 'import json, re, math; print(json.dumps(re.findall("a", "aa")), math.sqrt(4))'`,
		`python3 -c 'print(open("README.md").read())'`,
		`node -e 'console.log(JSON.stringify({a: [1, 2]}))'`,
		`perl -e 'print "hi\n"'`,
		`ruby -e 'puts [1,2].sum'`,
	}
	for _, cmd := range allowed {
		if v := r.verdict(cmd); v.Deny {
			t.Errorf("%s: refused (%s), want allowed", cmd, v.Reason)
		}
	}
}

// TestWorktreeDelProgramRefusalWords pins the token rule on the words that decide it, without a command around them.
func TestWorktreeDelProgramRefusalWords(t *testing.T) {
	refused := []string{"fs.rm(p)", "x.Rmdir()", "os.popen(c)", "p.rm_rf", "f(x)[expr]", "d[\"a\"+\"b\"]"}
	allowed := []string{"d[0:2]", "d[\"status\"]", "[1, 2]", "`ls`"}
	for _, src := range refused {
		if worktreeDelProgramRefusal("python", src) == "" {
			t.Errorf("%q: allowed, want refused", src)
		}
	}
	for _, src := range allowed {
		if why := worktreeDelProgramRefusal("node", src); why != "" {
			t.Errorf("%q: refused (%s), want allowed", src, why)
		}
	}
	if worktreeDelProgramRefusal("ruby", "`ls`") == "" {
		t.Errorf("ruby backtick: allowed, want refused")
	}
}
