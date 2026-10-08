package hook

import "testing"

// TestWorktreeDelInlineProgramRows: in a managed worktree an interpreter's inline program that removes a file, starts
// another program, loads code from a variable or reaches a name only at run time is refused; ordinary programs are allowed.
func TestWorktreeDelInlineProgramRows(t *testing.T) {
	r := newDelRig(t)
	denied := []string{
		`ruby -e 'require "fileutils"; FileUtils.rm_r("x")'`,
		`ruby -e 'require "fileutils"; FileUtils.rm_rf("x")'`,
		`ruby -e 'require "fileutils"; FileUtils.rm("x")'`,
		`ruby -e 'require "fileutils"; FileUtils.remove_entry("x")'`,
		`ruby -e 'require "fileutils"; FileUtils.remove_dir("x")'`,
		`ruby -e 'Dir.rmdir("x")'`,
		`ruby -e 'File.unlink("x")'`,
		`ruby -e 'Pathname.new("x").rmtree'`,
		`ruby -e 'system("rm -rf x")'`,
		`ruby -e 'send(:system, "rm -rf x")'`,
		`python3 -c 'from pathlib import Path; Path("x").rmdir()'`,
		`python3 -c 'from pathlib import Path; Path("x").unlink()'`,
		`python3 -c 'import os; os.removedirs("d")'`,
		`python3 -c 'import os; os.remove("x")'`,
		`python3 -c 'import shutil; shutil.rmtree("d")'`,
		`python3 -c 'import shutil; getattr(shutil,"rm"+"tree")("d")'`,
		`python3 -c 'import subprocess; subprocess.run(["rm","-rf","x"])'`,
		`python3 -c 'import os; os.system("rm -rf x")'`,
		`python3 -c 'import runpy; runpy.run_path("x.py")'`,
		`python3 -c 'import pickle,base64; pickle.loads(base64.b64decode("gAJ9"))'`,
		`python3 -c 'import ctypes; ctypes.CDLL("libc.so.6").system(b"x")'`,
		`node -e 'require("child_process").execSync("rm -rf x")'`,
		`node -e 'require("fs").promises.rm("x",{recursive:true})'`,
		`node -e 'require("fs").rm("x",{recursive:true},()=>{})'`,
		`node -e 'require("node:fs").promises.rm("x")'`,
		`node -e 'require("fs").rmSync("x")'`,
		`node -e 'require("fs")["rm"+"Sync"]("x")'`,
		`node -e 'Deno.remove("x")'`,
		`node -e 'new Function("return 1")()'`,
		`node -e 'require("vm").runInThisContext("1")'`,
		`perl -e 'unlink "x"'`,
		`perl -e 'use File::Path; remove_tree("d")'`,
		`perl -e 'system("rm -rf x")'`,
		`perl -e 'require $f'`,
		`perl -e 'do $file'`,
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
		`python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["status"])'`,
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
	refused := []struct{ lang, src string }{
		{"python", "fs.rm(p)"}, {"node", "fs.rm(p)"}, {"python", "x.Rmdir()"}, {"python", "os.popen(c)"},
		{"python", "p.rm_rf"}, {"ruby", "FileUtils.rm_r(p)"}, {"python", "f(x)[expr]"}, {"python", "d[\"a\"+\"b\"]"},
		{"ruby", "send(:x)"}, {"node", "new Function(s)"}, {"perl", "require $f"}, {"ruby", "`ls`"},
	}
	allowed := []struct{ lang, src string }{
		{"python", "d[0:2]"}, {"python", "d[\"status\"]"}, {"python", "[1, 2]"}, {"node", "d[\"status\"]"},
		{"python", "json.load(f)"}, {"ruby", "load_config"}, {"perl", "print 1"},
	}
	for _, c := range refused {
		if worktreeDelProgramRefusal(c.lang, c.src) == "" {
			t.Errorf("%s %q: allowed, want refused", c.lang, c.src)
		}
	}
	for _, c := range allowed {
		if why := worktreeDelProgramRefusal(c.lang, c.src); why != "" {
			t.Errorf("%s %q: refused (%s), want allowed", c.lang, c.src, why)
		}
	}
}
