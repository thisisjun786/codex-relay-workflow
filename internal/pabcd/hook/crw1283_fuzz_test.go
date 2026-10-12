package hook

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// CRW-1283 differential fuzz. A grammar composes the forms the relaxations touch ("$@" forwarding wrapper files, program wrappers,
// PATH assignments, readable script files, program files under names taken from the existing tests, and compositions of two
// layers) around payloads taken only from crw1283_test.go, crw1283_review_test.go and crw1283_probe_test.go: each guard's
// protected command and the benign controls. The corpus is deterministic (full enumeration; the depth-2 sample is drawn with PCG
// seed 1283) and pinned by fzFamilyCounts. No test runs a command. A form is class target when its payload is a guard's target
// (git push only behind a wrapper, script or PATH assignment, where the GitHub guard reads it) or an assigned PATH makes a fixture
// that holds the target the program it runs; benign otherwise; ownlimit for the pre-existing unread-script limit fzOwnLimit pins.
//
// Head property (always runs): every form carrying a guard's target is refused by that guard. Base differential, from the
// repository (OUT is any directory, TMP a directory under the TMPDIR of the verification):
//
//	CRW1283_FUZZ_TABLE=$OUT/head.tsv go test -count=1 -run TestCRW1283GuardFuzz ./internal/pabcd/hook/
//	git worktree add --detach $TMP/base c007bd50 && cp internal/pabcd/hook/crw1283_fuzz_test.go $TMP/base/internal/pabcd/hook/ &&
//	  (cd $TMP/base && CRW1283_FUZZ_TABLE=$OUT/base.tsv go test -count=1 -run TestCRW1283GuardFuzz ./internal/pabcd/hook/)
//	git worktree remove --force $TMP/base
//	join -t "$(printf '\t')" <(sort $OUT/base.tsv) <(sort $OUT/head.tsv) | awk -F'\t' '$3=="refuse" && $7=="allow" {print $2}' | sort | uniq -c
//
// With CRW1283_FUZZ_TABLE set the test writes one line per form (guard:id, class, decision, description, command) and skips the
// head property, so the identical file records the decisions of any revision whose hook APIs it compiles against. A form that
// the base refuses and the head allows is a P0 when its class is target and an intended relaxation when it is benign.

// fzOwnLimit is the forms of class ownlimit the guard allows: a script run by its path is not read by the memory and worktree
// guards from the command itself unless its body forwards "$@" outside quotes (c007bd50 and the head alike; the CRW-1283 handoff
// names it an open item), and the worktree guard does not read one at any depth, nor a script without a shebang line. The count is
// pinned so it cannot grow; a form of this class that the head refuses is not counted.
var fzOwnLimit = map[string]int{"memory": 36, "github": 0, "worktree": 36}

var fzFamilyCounts = map[string]int{"plain": 6, "wrapper": 360, "mutate": 630, "progwrap": 84, "assignpath": 288, "path": 288, "script": 36, "progfile": 816, "depth2": 1800,
	"binhdr": 360, "shebang": 384}

// fzDepth2Pairs is the depth-2 sample size; CRW1283_FUZZ_PAIRS=all runs every pair (an exhaustive search; the pinned counts and the
// pinned limit are then not asserted).
const fzDepth2Pairs = 300

type fzPayload struct {
	id, cmd, prog string
	target, deep  bool // deep: a target only behind a wrapper file, a script or a PATH assignment (the GitHub guard reads git push there only)
}

type fzCmd struct {
	cmd, desc, path string // path is the PATH value a layer assigned ("" if none)
	amb             string // the PATH the shell starts with, when the form needs another than /usr/bin:/bin
	deep            bool   // a wrapper file, a script or a PATH assignment is around the payload
	direct          bool   // a script is run by its path somewhere in the chain
}

type fzLayer struct {
	name  string
	argv  bool // takes the command as its arguments, so only argv layers or the payload may follow it
	apply func(g *fzGuard, c fzCmd) fzCmd
}

type fzGuard struct {
	name, dir, root string
	refused         func(string) bool
	payloads        []fzPayload
	seq             int
	t               *testing.T
}

type fzForm struct{ key, class, desc, cmd, amb string }

// fzFixture is the payload that runs each program name of the fixture directories; the names are those the existing CRW-1283
// tests use (builtins, keywords, common programs). The target program names are fixtures too, without a payload of their own.
var fzFixture = map[string]string{"go": "go version", "true": "true", "echo": "echo ok", "printf": "printf x", "test": "test -n x",
	"time": "time true", "let": "let x=1", "declare": "declare x=1", "mapfile": "mapfile x </dev/null", "[[": "[[ -n x ]]",
	"pushd": "pushd /tmp", "python3": "python3 -c 'print(1)'", "perl": "perl -e 1", "node": "node -e 1", "awk": "awk 'BEGIN{}'",
	"cd": "cd /tmp", "command": "command true", "gh": "", "git": "", "tee": "", "rm": "", "bash": ""}

var fzNames = func() (names []string) {
	for name := range fzFixture {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}()

var fzBodies = []string{"\"$@\"\n", "#!/bin/bash\nexec \"$@\"\n", "#!/bin/bash\nionice -c3 nice \"$@\"\n",
	"#!/usr/bin/env bash\nset -euo pipefail\nexec ionice -c3 nice -n 5 \"$@\"\n", "#!/bin/sh\nenv TZ=UTC GOFLAGS=-p=4 \"$@\"\n",
	"#!/bin/bash\ntimeout 600 \"$@\"\n", "#!/bin/bash\nexport TMPDIR=/tmp\nexec ionice -c3 nice \"$@\"\n", "#!/bin/bash\n\"$@\"\n",
	"#!/bin/bash\n{W2} \"$@\"\n", "#!/bin/bash\nexec bash {W2} \"$@\"\n",
	// the loop shapes of heavy_slot.sh (the final verification of 418273ef, finding 2): variables assigned in the loop that forwards "$@"
	"#!/bin/bash\nwhile :; do x=1; nice \"$@\"; rc=$?; exit $rc; done\n", "#!/bin/bash\nfor i in 1 2; do start=$(date +%s); ionice -c3 nice \"$@\"; break; done\n"}

// fzMutants are the scripts the security reviews found read as forwarders: a script that changes its positional parameters, or runs
// "$@" later or in another shell, does not forward the arguments it was given. {C} is the inner command; each body and its
// arguments are those of TestReview1283PositionalMutation and TestReview1283ProbeForms, with the inner command where they put the
// protected one. The inner command runs in each of them, so a guard that reads "$@" as the arguments allows a target.
var fzMutants = [][2]string{{"set -- {C}\n\"$@\"\n", "true"}, {"set -e -- {C}\n\"$@\"\n", "true"}, {"shift\n\"$@\"\n", "true {C}"},
	{"shift 2\n\"$@\"\n", "true true {C}"}, {"eval shift\n\"$@\"\n", "true {C}"}, {"trap shift DEBUG\n\"$@\"\n", "true {C}"},
	{"f() { set -- {C}; }\nf\n\"$@\"\n", "true"}, {"if test -n \"$1\"; then shift; fi\n\"$@\"\n", "true {C}"},
	{"trap '\"$@\"' EXIT\nshift\n", "true {C}"}, {"trap '\"$@\"' EXIT\nset -- {C}\n", "true"},
	{"bash -s {C} <<'EOF'\n\"$@\"\nEOF\n", "true"}, {"bash /dev/fd/3 {C} 3<<'EOF'\n\"$@\"\nEOF\n", "true"},
	{"sh -s {C} <<'EOF'\n\"$@\"\nEOF\n", "true"}, {". ./shift.sh\n\"$@\"\n", "true {C}"},
	// "$@" run before a positional change in a loop: the next iteration runs the changed arguments (the final verification of 418273ef,
	// finding 1); TestReview1283LoopPositionalChange has the bodies. {C} is the command the changed arguments reach.
	{"while :; do nice \"$@\"; 1=env; done\n", "true {C}"}, {"until false; do ionice -c3 \"$@\"; 2+=x; done\n", "true {C}"},
	{"while :; do timeout 5 \"$@\"; argv=(env {C}); done\n", "true"}, {"while :; do env \"$@\"; set -- {C}; done\n", "true"},
	{"while :; do nice \"$@\"; shift; done\n", "true {C}"}, {"while :; do nice \"$@\"; for 1 in env; do :; done; done\n", "true {C}"},
	{"while :; do x=1; nice \"$@\"; if :; then 1=env; fi; done\n", "true {C}"}}

// fzShebangs are scripts whose #! line launches the interpreter fzShebangProgs names through env (the evaluation of deb45bd1, d2): the
// assigned PATH selects the interpreter, whose body in the evil fixture directory is the target. fzBinValues are the PATH values of
// the binary-headed forms (d1): a fixture directory named like a system one (usr/local/bin), another bin directory, and /usr/bin.
var fzShebangs = []string{"#!/usr/bin/env bash\n:\n", "#!/usr/bin/env python3\n:\n", "#!/usr/bin/env -S bash -e\n:\n", "#!/usr/bin/env node"}
var fzShebangProgs = []string{"bash", "python3", "bash", "node"}
var fzBinValues = []string{"{B}", "{B}:/usr/bin:/bin", "/usr/bin:/bin:{B}", "{L}:/usr/bin:/bin", "/usr/bin:/bin"}
var fzBinProgs = []string{"check", "linked", "static-sh"}

var fzRunModes = []string{"bash %s ", "sh %s ", "%s ", ". %s ", "source %s "}
var fzScriptModes = []string{"bash %s", "sh %s", ". %s", "source %s", "%s", "%s"}
var fzProgWraps = []string{"ionice -c3 ", "nice ", "nice -n 5 ", "env ", "env -i ", "env X=1 ", "timeout 60 ", "timeout -k 5 60 ", "nohup ",
	"exec ", "command ", "ionice -c3 nice -n 5 ", "env -u FOO TZ=UTC ", "GOFLAGS=-p=4 TZ=UTC "}
var fzPathValues = []string{"{D}", "{D}:/usr/bin:/bin", "/usr/bin:/bin:{D}", "/usr/bin:/bin", ":/usr/bin", "relbin:/usr/bin", "$UNKNOWN:/usr/bin",
	"{D}:$PATH", "$PATH:{D}", "{E}:/usr/bin:/bin", "/usr/bin:/bin:{E}", "{E}"}
var fzPathKinds = []string{"PATH=%s ", "export PATH=%s; ", "PATH=%s; export PATH; ", "env PATH=%s "}

func fzLayers(family string) (layers []fzLayer) {
	switch family {
	case "wrapper":
		for i := range fzBodies {
			for j, mode := range fzRunModes {
				layers = append(layers, fzLayer{fmt.Sprintf("wrapper%d.%d", i, j), true, func(g *fzGuard, c fzCmd) fzCmd {
					c.cmd, c.deep = fmt.Sprintf(mode, filepath.Join(g.dir, fmt.Sprintf("w%d.sh", i)))+c.cmd, true
					return c
				}})
			}
		}
	case "progwrap":
		for i, prefix := range fzProgWraps {
			layers = append(layers, fzLayer{fmt.Sprintf("progwrap%d", i), strings.HasPrefix(prefix, "env") || !strings.Contains(prefix, "="),
				func(g *fzGuard, c fzCmd) fzCmd { c.cmd = prefix + c.cmd; return c }})
		}
	case "path":
		for i, kind := range fzPathKinds {
			for j, value := range fzPathValues {
				layers = append(layers, fzLayer{fmt.Sprintf("path%d.%d", i, j), i == 3, func(g *fzGuard, c fzCmd) fzCmd {
					v := strings.NewReplacer("{D}", filepath.Join(g.dir, "ben"), "{E}", filepath.Join(g.dir, "evil")).Replace(value)
					c.cmd, c.deep = fmt.Sprintf(kind, v)+c.cmd, true
					if c.path == "" {
						c.path = v
					}
					return c
				}})
			}
		}
	case "mutate":
		for i, m := range fzMutants {
			for j, mode := range fzScriptModes[:5:5] {
				layers = append(layers, fzLayer{fmt.Sprintf("mutate%d.%d", i, j), false, func(g *fzGuard, c fzCmd) fzCmd {
					g.seq++
					g.write("shift.sh", "shift\n")
					file := g.write(fmt.Sprintf("m%05d.sh", g.seq), "#!/bin/bash\n"+strings.ReplaceAll(m[0], "{C}", c.cmd))
					c.cmd, c.deep, c.direct = fmt.Sprintf(mode, file)+" "+strings.ReplaceAll(m[1], "{C}", c.cmd), true, c.direct || j == 4
					return c
				}})
			}
		}
	case "script":
		for i, mode := range fzScriptModes {
			layers = append(layers, fzLayer{fmt.Sprintf("script%d", i), false, func(g *fzGuard, c fzCmd) fzCmd {
				g.seq++
				shebang := "#!/bin/bash\n"
				if i == 5 {
					shebang = ""
				}
				file := g.write(fmt.Sprintf("s%05d.sh", g.seq), shebang+c.cmd+"\n")
				c.cmd, c.deep, c.direct = fmt.Sprintf(mode, file), true, c.direct || i >= 4
				return c
			}})
		}
	}
	return layers
}

func (g *fzGuard) write(name, body string) string {
	file := filepath.Join(g.dir, name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
		g.t.Fatal(err)
	}
	return file
}

// setup writes the wrapper files and the two fixture directories: ben (benign bodies) and evil (the guard's target as the body).
func (g *fzGuard) setup() {
	w2 := filepath.Join(g.dir, "w2.sh")
	for i, body := range fzBodies {
		g.write(fmt.Sprintf("w%d.sh", i), strings.ReplaceAll(body, "{W2}", w2))
	}
	for _, name := range fzNames {
		g.write("ben/"+name, "#!/bin/sh\nprintf ok\n")
		g.write("evil/"+name, "#!/bin/sh\n"+g.payloads[0].cmd+"\n")
	}
	shell, err := os.ReadFile("/bin/sh")
	if err != nil {
		g.t.Fatal(err)
	}
	for k, body := range fzShebangs {
		for _, d := range []string{"", "ben/", "evil/"} {
			g.write(fmt.Sprintf("%ssb%d", d, k), body)
		}
	}
	for _, d := range []string{"usr/local/bin", "bin"} {
		g.write(d+"/check", string(shell))
		if err := os.Symlink("/bin/sh", filepath.Join(g.dir, d, "linked")); err != nil {
			g.t.Fatal(err)
		}
	}
}

// fzResolve is where the assigned PATH value finds a program: evil (a fixture running the target), benign, system, none or unknown
// (a variable expansion comes first). cd and command are found before any PATH search by every shell.
func fzResolve(prog, value string) string {
	if prog == "cd" || prog == "command" {
		return "none"
	}
	_, fixture := fzFixture[prog]
	for _, entry := range strings.Split(value, ":") {
		switch {
		case strings.HasSuffix(entry, "/evil") && fixture:
			return "evil"
		case strings.HasSuffix(entry, "/ben") && fixture:
			return "benign"
		case entry == "$UNKNOWN":
			return "unknown"
		case entry == "$PATH":
			return fzResolve(prog, "/usr/bin:/bin")
		case entry == "/usr/bin" || entry == "/bin":
			if _, err := os.Stat(filepath.Join(entry, prog)); err == nil {
				return "system"
			}
		}
	}
	return "none"
}

func (g *fzGuard) form(family string, n int, p fzPayload, c fzCmd) fzForm {
	class := "benign"
	switch r := fzResolve(p.prog, c.path); {
	case p.target && (!p.deep || c.deep) || r == "evil":
		class = "target"
	case r == "unknown" && c.path != "":
		class = "unknown"
	}
	if class == "target" && g.name != "github" && c.direct {
		class = "ownlimit"
	}
	return fzForm{fmt.Sprintf("%s:%s-%04d", g.name, family, n), class, c.desc + " <- " + p.id, c.cmd, c.amb}
}

func (g *fzGuard) forms() (all []fzForm, counts map[string]int) {
	counts = map[string]int{}
	add := func(family string, p fzPayload, c fzCmd) {
		counts[family]++
		all = append(all, g.form(family, counts[family], p, c))
	}
	for _, p := range g.payloads {
		add("plain", p, fzCmd{cmd: p.cmd, desc: "plain"})
	}
	for _, family := range []string{"wrapper", "progwrap", "path", "script", "mutate"} {
		for _, layer := range fzLayers(family) {
			for _, p := range g.payloads {
				c := layer.apply(g, fzCmd{cmd: p.cmd})
				c.desc = layer.name
				add(family, p, c)
			}
		}
	}
	for _, layer := range fzLayers("path") {
		for _, name := range fzNames {
			if cmd := fzFixture[name]; cmd != "" {
				c := layer.apply(g, fzCmd{cmd: cmd})
				c.desc = layer.name
				add("progfile", fzPayload{id: "file-" + name, cmd: cmd, prog: name}, c)
			}
		}
	}
	// Found by the exhaustive depth-2 search (CRW1283_FUZZ_PAIRS=all): an assignment in front of the statement that assigns PATH.
	assign := fzLayers("progwrap")[13]
	for _, inner := range fzLayers("path") {
		for _, p := range g.payloads {
			c := assign.apply(g, inner.apply(g, fzCmd{cmd: p.cmd}))
			c.desc = assign.name + "<-" + inner.name
			add("assignpath", p, c)
		}
	}
	// The evaluation of deb45bd1: d2, a script whose env #! interpreter the assigned PATH selects (run by name and by path); d1, a
	// binary-headed program (a copy, a link, busybox as static-sh under a starting PATH that lacks it) in a PATH directory.
	for _, layer := range fzLayers("path") {
		for k, prog := range fzShebangProgs {
			for _, dir := range []string{"", g.dir + "/"} {
				name := fmt.Sprintf("sb%d", k)
				c := layer.apply(g, fzCmd{cmd: dir + name})
				c.desc = layer.name
				add("shebang", fzPayload{id: name, cmd: dir + name, prog: prog}, c)
			}
		}
	}
	for ki, kind := range fzPathKinds {
		for i, value := range fzBinValues {
			v := strings.NewReplacer("{B}", filepath.Join(g.dir, "usr/local/bin"), "{L}", filepath.Join(g.dir, "bin")).Replace(value)
			for _, prog := range fzBinProgs {
				for _, p := range g.payloads {
					c := fzCmd{cmd: fmt.Sprintf(kind, v) + prog + " -c '" + strings.ReplaceAll(p.cmd, "'", `'\''`) + "'", path: v, deep: true}
					c.desc = fmt.Sprintf("binhdr%d.%d.%s", ki, i, prog)
					if prog == "static-sh" {
						c.amb = filepath.Join(g.dir, "amb")
					}
					add("binhdr", p, c)
				}
			}
		}
	}
	var pairs [][2]fzLayer
	for _, outer := range []string{"wrapper", "progwrap", "path", "script", "mutate"} {
		for _, o := range fzLayers(outer) {
			for _, inner := range []string{"wrapper", "progwrap", "path", "script", "mutate"} {
				for _, i := range fzLayers(inner) {
					if !o.argv || i.argv {
						pairs = append(pairs, [2]fzLayer{o, i})
					}
				}
			}
		}
	}
	rng := rand.New(rand.NewPCG(1283, 1283))
	rng.Shuffle(len(pairs), func(a, b int) { pairs[a], pairs[b] = pairs[b], pairs[a] })
	if os.Getenv("CRW1283_FUZZ_PAIRS") != "all" {
		pairs = pairs[:fzDepth2Pairs]
	}
	for _, pair := range pairs {
		for _, p := range g.payloads {
			c := pair[0].apply(g, pair[1].apply(g, fzCmd{cmd: p.cmd}))
			c.desc = pair[0].name + "<-" + pair[1].name
			add("depth2", p, c)
		}
	}
	return all, counts
}

func fzGuards(t *testing.T) []*fzGuard {
	benign := []fzPayload{{"go-version", "go version", "go", false, false}, {"go-test", "go test ./...", "go", false, false}, {"true", "true", "true", false, false},
		{"echo", "echo ok", "echo", false, false}}
	cwd, root, env := gateScene(t)
	gh, _, _ := gateScene(t)
	rig := newDelRig(t)
	targets := []*fzGuard{
		{name: "memory", dir: cwd, root: root, refused: func(c string) bool { return HandleMemoryWriteGate(gateBash(t, cwd, c), env) != "" },
			payloads: []fzPayload{{"tee", "tee " + root + "/note.md", "tee", true, false}, {"sh-redirect", "sh -c 'echo x >\"" + root + "/note.md\"'", "sh", true, false}}},
		{name: "github", dir: gh, refused: func(c string) bool { _, denied := githubPostJudgeText(c, gh); return denied },
			payloads: []fzPayload{{"gh-comment", "gh pr comment 1 --body x", "gh", true, false}, {"git-push", "git push origin dev", "git", true, true}}},
		{name: "worktree", dir: rig.checkout, refused: func(c string) bool { return rig.verdict(c).Deny },
			payloads: []fzPayload{{"rm-rf", "rm -rf " + rig.checkout, "rm", true, false}, {"git-worktree-remove", "git worktree remove --force " + rig.checkout, "git", true, false}}},
	}
	for _, g := range targets {
		g.t, g.payloads = t, append(g.payloads, benign...)
		g.setup()
	}
	return targets
}

// TestCRW1283GuardFuzz: the corpus keeps its pinned size, every form carrying a guard's target is refused, and the intended
// relaxations (a heavy_slot-shaped wrapper running go test, a PATH assignment with go, a readable script without a target) still allow.
func TestCRW1283GuardFuzz(t *testing.T) {
	t.Setenv("HOME", cleanTempDir(t))
	t.Setenv("PATH", "/usr/bin:/bin")
	table, pinned := os.Getenv("CRW1283_FUZZ_TABLE"), os.Getenv("CRW1283_FUZZ_PAIRS") != "all"
	if table != "" {
		if err := os.WriteFile(table, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range fzGuards(t) {
		all, counts := g.forms()
		t.Logf("%s corpus: %v", g.name, counts)
		for family, want := range fzFamilyCounts {
			if pinned && counts[family] != want {
				t.Errorf("%s family %s has %d forms, pinned %d", g.name, family, counts[family], want)
			}
		}
		stats := map[string]int{}
		var rows []string
		canon := strings.NewReplacer(g.dir, "{DIR}")
		if g.root != "" {
			canon = strings.NewReplacer(g.dir, "{DIR}", g.root, "{ROOT}")
		}
		for _, f := range all {
			if err := os.Setenv("PATH", map[bool]string{true: f.amb, false: "/usr/bin:/bin"}[f.amb != ""]); err != nil {
				t.Fatal(err)
			}
			refused := g.refused(f.cmd)
			decision := map[bool]string{true: "refuse", false: "allow"}[refused]
			stats[f.class+"/"+decision]++
			rows = append(rows, strings.Join([]string{f.key, f.class, decision, f.desc, canon.Replace(f.cmd)}, "\t"))
			if table == "" && f.class == "target" && !refused {
				t.Errorf("%s guard ALLOWED a target form %s (%s): %s", g.name, f.key, f.desc, f.cmd)
			}
		}
		t.Logf("%s decisions: %v", g.name, stats)
		if table == "" && pinned && stats["ownlimit/allow"] != fzOwnLimit[g.name] {
			t.Errorf("%s guard allows %d forms of the pre-existing unread-script limit, pinned %d", g.name, stats["ownlimit/allow"], fzOwnLimit[g.name])
		}
		if table != "" {
			out, err := os.OpenFile(table, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(out, strings.Join(rows, "\n"))
			out.Close()
			continue
		}
		t.Run("controls-"+g.name, func(t *testing.T) { g.t = t; g.controls() })
	}
}

// controls: the intended relaxations still allow: a heavy_slot-shaped wrapper running go test, a PATH assignment with go (the
// toolchain the starting PATH runs), a PATH of system directories, and a readable script without a target.
func (g *fzGuard) controls() {
	tool := filepath.Join(g.dir, "toolchain")
	program, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		g.t.Skipf("no /usr/bin/true: %v", err)
	}
	if err := os.MkdirAll(tool, 0o755); err != nil || os.WriteFile(filepath.Join(tool, "go"), program, 0o755) != nil {
		g.t.Fatal(err)
	}
	g.t.Setenv("PATH", tool+":/usr/bin:/bin")
	slot := filepath.Join(g.dir, "w2.sh")
	for _, control := range []string{"bash " + slot + " go test ./...", slot + " go version", "PATH=" + tool + ":/usr/bin:/bin go version",
		"export PATH=" + tool + ":/usr/bin:/bin; go version", "PATH=/usr/bin:/bin printf x", "bash " + g.write("control.sh", "go version\n"),
		"PATH=" + tool + ":/usr/bin:/bin " + filepath.Join(g.dir, "sb0")} {
		if g.refused(control) {
			g.t.Errorf("%s guard refused the intended relaxation: %s", g.name, control)
		}
	}
}
