package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// reviewRoundArgsRoot is the placeholder the recorder uses for the root of the world it built.
const reviewRoundArgsRoot = "$" + "{ROOT}"

// reviewRoundArgsOracle is testdata/review_round_args/oracle.json: what the Node build of CXC v0.2.40 (3c1459ac) answered,
// recorded by record-oracle.mjs.
type reviewRoundArgsOracle struct {
	World struct {
		Dirs     []string
		Files    map[string]string
		Symlinks map[string]string
		Modes    map[string]uint32
	}
	Parse []struct {
		ID   string
		Argv []string
		Cwd  string
		Want json.RawMessage
	}
	Help string
	Hash []struct {
		ID    string
		Files []goalplan.PlanFileHash
		Want  string
	}
	Collect []struct {
		ID, Cwd, Unit string
		Paths         []string
		Want          struct {
			Files         []goalplan.PlanFileHash
			Error, Throws string
		}
	}
	Recomputed []struct {
		ID, Cwd string
		Paths   []string
		Want    []goalplan.PlanFileHash
	}
	Toml []struct {
		ID     string
		Config *string
		Where  string
		Want   bool
	}
	Packet []struct {
		ID, LaunchID, RoundID string
		FileCount             int
		Config                *string
		Want                  string
	}
}

func reviewRoundArgsLoad(t *testing.T) *reviewRoundArgsOracle {
	t.Helper()
	data, err := os.ReadFile("testdata/review_round_args/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	o := &reviewRoundArgsOracle{}
	if err := json.Unmarshal(data, o); err != nil {
		t.Fatal(err)
	}
	if len(o.Parse) == 0 || len(o.Hash) == 0 || len(o.Collect) == 0 || len(o.Recomputed) == 0 || len(o.Toml) == 0 || len(o.Packet) == 0 {
		t.Fatal("the oracle holds an empty section")
	}
	return o
}

func reviewRoundArgsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func reviewRoundArgsWrite(t *testing.T, path, content string) {
	t.Helper()
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
	reviewRoundArgsMust(t, os.WriteFile(path, []byte(content), 0o644))
}

func reviewRoundArgsHex(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func reviewRoundArgsEnv(m map[string]string) host.LookupEnv {
	return func(key string) (string, bool) { v, ok := m[key]; return v, ok }
}

func reviewRoundArgsReadable(path string) bool {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
	}
	return err == nil
}

// reviewRoundArgsWorld builds the recorded layout (a workspace, a directory beside it and links between them) under a temporary
// root and returns the root.
func reviewRoundArgsWorld(t *testing.T, o *reviewRoundArgsOracle) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recorded layout holds links and file names Windows does not allow")
	}
	root := t.TempDir()
	for _, dir := range o.World.Dirs {
		reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	for path, content := range o.World.Files {
		reviewRoundArgsWrite(t, filepath.Join(root, path), content)
	}
	for path, target := range o.World.Symlinks {
		reviewRoundArgsMust(t, os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755))
		reviewRoundArgsMust(t, os.Symlink(strings.ReplaceAll(target, reviewRoundArgsRoot, root), filepath.Join(root, path)))
	}
	for path, mode := range o.World.Modes {
		reviewRoundArgsMust(t, os.Chmod(filepath.Join(root, path), fs.FileMode(mode)))
	}
	return root
}

func TestReviewRoundArgsParserOracle(t *testing.T) {
	for _, c := range reviewRoundArgsLoad(t).Parse {
		t.Run(c.ID, func(t *testing.T) {
			before := slices.Clone(c.Argv)
			p := ParseReviewRoundCliArgs(c.Argv, c.Cwd)
			if (p.Args != nil) == (p.Error != "") {
				t.Fatalf("expected exactly one parser outcome: %+v", p)
			}
			var observed any = p.Args
			if p.Error != "" {
				observed = map[string]string{"error": p.Error}
			}
			gotJSON, err := json.Marshal(observed)
			reviewRoundArgsMust(t, err)
			// Whole JSON shapes are compared, so a field the oracle leaves undefined must be absent here too.
			var got, want any
			reviewRoundArgsMust(t, json.Unmarshal(gotJSON, &got))
			reviewRoundArgsMust(t, json.Unmarshal(c.Want, &want))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("argv %q: got %s, want %s", c.Argv, gotJSON, c.Want)
			}
			if !reflect.DeepEqual(before, c.Argv) {
				t.Error("parser changed argv")
			}
		})
	}
}

func TestReviewRoundArgsHelpAndHashOracle(t *testing.T) {
	o := reviewRoundArgsLoad(t)
	// The CLI name table renames the verb; nothing else in the help changes.
	if got, want := RenderReviewRoundHelp(), strings.ReplaceAll(o.Help, "cxc review-round", "crw pabcd review-round"); got != want {
		t.Errorf("help text:\n%s\nwant:\n%s", got, want)
	}
	for _, c := range o.Hash {
		if got := PlanFilesHash(c.Files); got != c.Want {
			t.Errorf("%s: aggregate hash %s, want %s", c.ID, got, c.Want)
		}
	}
}

// The security item: the oracle hashed a plan file whose real path is outside the working directory, because only the spelling of
// its path was checked. Three recorded layouts read as a refusal now; every other recorded answer is reproduced exactly.
func TestReviewRoundArgsCollectOracle(t *testing.T) {
	o := reviewRoundArgsLoad(t)
	root := reviewRoundArgsWorld(t, o)
	expand := func(s string) string { return strings.ReplaceAll(s, reviewRoundArgsRoot, root) }
	changed := map[string]bool{"unit_symlinked_outside": true, "subdirectory_symlinked_outside": true, "unit_outside_cwd": true}
	for _, c := range o.Collect {
		t.Run(c.ID, func(t *testing.T) {
			paths := make([]string, len(c.Paths))
			for i, p := range c.Paths {
				paths[i] = expand(p)
			}
			files, refusal, err := reviewRoundArgsCollectPlanFiles(expand(c.Cwd), expand(c.Unit), paths)
			if c.Want.Throws != "" {
				if reviewRoundArgsReadable(filepath.Join(expand(c.Cwd), paths[0])) {
					t.Skip("the file is readable (running as root?)")
				}
				if err == nil {
					t.Fatalf("the oracle throws %s here; got files %v refusal %q", c.Want.Throws, files, refusal)
				}
				return
			}
			wantFiles, wantRefusal := c.Want.Files, expand(c.Want.Error)
			if changed[c.ID] {
				if len(wantFiles) == 0 {
					t.Fatal("a case tagged as changed must have been answered with files by the oracle")
				}
				wantFiles, wantRefusal = nil, "plan path "+paths[0]+" is not a readable regular file"
				delete(changed, c.ID)
			}
			if err != nil || refusal != wantRefusal || !slices.Equal(files, wantFiles) {
				t.Errorf("got files %v refusal %q err %v; want files %v refusal %q", files, refusal, err, wantFiles, wantRefusal)
			}
		})
	}
	if len(changed) != 0 {
		t.Errorf("changed cases never recorded: %v", changed)
	}
}

// Recomputed reads "missing" for an entry whose real path is outside the working directory (the security item); the other recorded
// answers, including an entry linked to a file inside the workspace and a working directory that is itself a link, are exact.
func TestReviewRoundArgsRecomputedOracle(t *testing.T) {
	o := reviewRoundArgsLoad(t)
	root := reviewRoundArgsWorld(t, o)
	expand := func(s string) string { return strings.ReplaceAll(s, reviewRoundArgsRoot, root) }
	changed := map[string][]int{
		"absolute_outside": {0}, "parent_climb_outside": {0}, "symlink_to_outside": {0}, "symlinked_unit_outside": {0},
		"symlinked_subdirectory_outside": {0}, "cwd_is_a_symlink": {2},
	}
	for _, c := range o.Recomputed {
		t.Run(c.ID, func(t *testing.T) {
			if len(c.Want) != len(c.Paths) {
				t.Fatalf("recorded %d answers for %d entries", len(c.Want), len(c.Paths))
			}
			var in, want []goalplan.PlanFileHash
			for i, p := range c.Paths {
				in = append(in, goalplan.PlanFileHash{Path: expand(p), Sha256: "stale"})
				w := goalplan.PlanFileHash{Path: expand(c.Want[i].Path), Sha256: c.Want[i].Sha256}
				if slices.Contains(changed[c.ID], i) {
					if w.Sha256 == "missing" {
						t.Fatal("a case tagged as changed must have been answered with a hash by the oracle")
					}
					w.Sha256 = "missing"
				}
				want = append(want, w)
			}
			delete(changed, c.ID)
			if c.ID == "unreadable_file" && reviewRoundArgsReadable(filepath.Join(expand(c.Cwd), in[0].Path)) {
				t.Skip("the file is readable (running as root?)")
			}
			if got := Recomputed(expand(c.Cwd), in); !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
	if len(changed) != 0 {
		t.Errorf("changed cases never recorded: %v", changed)
	}
}

func TestReviewRoundArgsV2ProbeAndPacketOracle(t *testing.T) {
	o := reviewRoundArgsLoad(t)
	env := func(t *testing.T, config *string, where string) host.LookupEnv {
		dir := t.TempDir()
		vars, configDir := map[string]string{"CODEX_HOME": dir, "HOME": t.TempDir()}, dir
		if where == "home" {
			vars, configDir = map[string]string{"CODEX_HOME": "", "HOME": dir}, filepath.Join(dir, ".codex")
		}
		if config != nil {
			reviewRoundArgsWrite(t, filepath.Join(configDir, "config.toml"), *config)
		}
		return reviewRoundArgsEnv(vars)
	}
	for _, c := range o.Toml {
		t.Run(c.ID, func(t *testing.T) {
			if got, err := reviewRoundArgsV2SpawnSurface(env(t, c.Config, c.Where)); err != nil || got != c.Want {
				t.Errorf("v2 surface %v %v, want %v", got, err, c.Want)
			}
		})
	}
	for _, c := range o.Packet {
		t.Run("packet_"+c.ID, func(t *testing.T) {
			round := goalplan.ReviewRoundState{RoundID: c.RoundID, Lane: goalplan.ReviewLane{LaunchID: c.LaunchID}}
			got, err := reviewRoundArgsRenderOpenPacket(round, c.FileCount, env(t, c.Config, "codex-home"))
			// Rule R9 renames the role header the reviewer's reply is matched against.
			if want := strings.ReplaceAll(c.Want, "CXC-ROLE:", "CRW-ROLE:"); err != nil || got != want {
				t.Errorf("packet:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// help-verbs.test.ts:83-100: help, --help and -h are not an unknown verb, and the help names the verbs' usage.
func TestReviewRoundArgsHelpVerbs(t *testing.T) {
	for _, token := range []string{"help", "--help", "-h"} {
		p := ParseReviewRoundCliArgs([]string{token}, "/ws")
		if p.Args == nil || p.Args.Verb != ReviewRoundVerbHelp || p.Error != "" {
			t.Fatalf("%s must not be an unknown verb: %+v", token, p)
		}
	}
	help := RenderReviewRoundHelp()
	for _, want := range []string{"Usage:", "open --session", "abort --session", "crw pabcd review-round open"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(help, "cxc") {
		t.Error("help still carries a CXC name")
	}
}

// review-binding.test.ts:96-100 and :127-134, review-deadlock.test.ts:41-47: the argv an open and an abort are given, and what a
// foreign or a missing --plan-path reads.
func TestReviewRoundArgsBindingPlanPaths(t *testing.T) {
	cwd, unit := t.TempDir(), "devlog/_plan/260815_probe"
	doc := unit + "/000_plan.md"
	reviewRoundArgsWrite(t, filepath.Join(cwd, doc), "# probe\n")
	reviewRoundArgsWrite(t, filepath.Join(cwd, "package.json"), "{}\n")

	open := ParseReviewRoundCliArgs([]string{"open", "--session", "rb", "--cwd", cwd, "--plan-path", doc}, cwd)
	if open.Args == nil || open.Args.Verb != ReviewRoundVerbOpen || *open.Args.Session != "rb" || open.Args.Cwd != cwd || !slices.Equal(open.Args.PlanPaths, []string{doc}) {
		t.Fatalf("open argv: %+v", open)
	}
	abort := ParseReviewRoundCliArgs([]string{"abort", "--session", "rb", "--cwd", cwd, "--reason", "reviewer died"}, cwd)
	if abort.Args == nil || abort.Args.Verb != ReviewRoundVerbAbort || *abort.Args.Reason != "reviewer died" || len(abort.Args.PlanPaths) != 0 {
		t.Fatalf("abort argv: %+v", abort)
	}

	files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, open.Args.PlanPaths)
	if want := []goalplan.PlanFileHash{{Path: doc, Sha256: reviewRoundArgsHex("# probe\n")}}; err != nil || refusal != "" || !slices.Equal(files, want) {
		t.Fatalf("one document: %v %q %v, want %v", files, refusal, err, want)
	}
	for _, c := range []struct {
		paths []string
		text  string
	}{{[]string{"package.json"}, "outside the bound plan unit"}, {nil, "--plan-path is required"}} {
		if _, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, c.paths); err != nil || !strings.Contains(refusal, c.text) {
			t.Errorf("%q: refusal %q err %v, want %q", c.paths, refusal, err, c.text)
		}
	}
}

// crlf-inputs.test.ts:79-142: a CRLF config.toml reads as its LF twin, and the packet tells the reviewer how it is launched.
func TestReviewRoundArgsCRLFConfig(t *testing.T) {
	lf := "[features.multi_agent_v2]\nenabled = true\n"
	var packets [2]string
	for i, config := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		home := t.TempDir()
		reviewRoundArgsWrite(t, filepath.Join(home, "config.toml"), config)
		env := reviewRoundArgsEnv(map[string]string{"CODEX_HOME": home, "HOME": t.TempDir()})
		if v2, err := reviewRoundArgsV2SpawnSurface(env); err != nil || !v2 {
			t.Errorf("config %d: the v2 surface is not read (%v)", i, err)
		}
		round := goalplan.ReviewRoundState{RoundID: "r1", Lane: goalplan.ReviewLane{LaunchID: "r1-20261004101010"}}
		packet, err := reviewRoundArgsRenderOpenPacket(round, 1, env)
		reviewRoundArgsMust(t, err)
		packets[i] = packet
	}
	launch := regexp.MustCompile(`(?m)^  LAUNCH: \w+-\d{14}$`)
	if !launch.MatchString(packets[1]) || packets[0] != packets[1] {
		t.Errorf("CRLF must give the same dispatch text:\n%s\n%s", packets[0], packets[1])
	}
	for _, want := range []string{`agent_type "reviewer"`, "CRW-ROLE: reviewer before TASK:"} {
		if !strings.Contains(packets[1], want) {
			t.Errorf("packet lacks %q", want)
		}
	}
}

// The security item: a link inside the workspace that points outside it is not followed, so the outside file is never hashed, by
// Recomputed or by the collection of plan files.
func TestReviewRoundArgsLinkOutOfWorkspaceIsNotHashed(t *testing.T) {
	root := t.TempDir()
	ws, unit := filepath.Join(root, "ws"), "devlog/_plan/u"
	secret := filepath.Join(root, "out", "secret.md")
	reviewRoundArgsWrite(t, secret, "TOP SECRET\n")
	reviewRoundArgsWrite(t, filepath.Join(root, "ws-evil", "secret.md"), "TOP SECRET\n")
	reviewRoundArgsWrite(t, filepath.Join(ws, unit, "000_plan.md"), "# plan\n")
	reviewRoundArgsMust(t, os.Symlink(secret, filepath.Join(ws, "leak.md")))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "out"), filepath.Join(ws, unit, "000_leak")))
	reviewRoundArgsMust(t, os.Symlink("../ws-evil/secret.md", filepath.Join(ws, "evil.md")))

	// ws-evil is a sibling whose name starts with the workspace's: a prefix test on the path text would take it for inside.
	entries := []string{"leak.md", unit + "/000_leak/secret.md", secret, "../out/secret.md", "evil.md", "../ws-evil/secret.md", unit + "/000_plan.md"}
	var in []goalplan.PlanFileHash
	for _, p := range entries {
		in = append(in, goalplan.PlanFileHash{Path: p, Sha256: "stale"})
	}
	got := Recomputed(ws, in)
	if len(got) != len(entries) {
		t.Fatalf("%d entries read for %d", len(got), len(entries))
	}
	for i, f := range got[:6] {
		if f.Sha256 != "missing" {
			t.Errorf("%s: read %s through a link or a path out of the workspace", entries[i], f.Sha256)
		}
	}
	if got[6].Sha256 != reviewRoundArgsHex("# plan\n") {
		t.Errorf("the plan inside the workspace reads %s", got[6].Sha256)
	}
	files, refusal, err := reviewRoundArgsCollectPlanFiles(ws, unit, []string{unit + "/000_leak/secret.md"})
	if want := "plan path " + unit + "/000_leak/secret.md is not a readable regular file"; err != nil || len(files) != 0 || refusal != want {
		t.Errorf("collect through a linked directory: %v %q %v, want refusal %q", files, refusal, err, want)
	}
}

// A relative working directory is made absolute against the kernel's directory.
func TestReviewRoundArgsRelativeCwd(t *testing.T) {
	root, unit := t.TempDir(), "devlog/_plan/u"
	reviewRoundArgsWrite(t, filepath.Join(root, "ws", unit, "000_plan.md"), "# plan\n")
	reviewRoundArgsWrite(t, filepath.Join(root, "out", "secret.md"), "TOP SECRET\n")
	t.Chdir(root)

	files, refusal, err := reviewRoundArgsCollectPlanFiles("ws", unit, []string{unit + "/000_plan.md"})
	if want := []goalplan.PlanFileHash{{Path: unit + "/000_plan.md", Sha256: reviewRoundArgsHex("# plan\n")}}; err != nil || refusal != "" || !slices.Equal(files, want) {
		t.Errorf("relative cwd: %v %q %v, want %v", files, refusal, err, want)
	}
	in := []goalplan.PlanFileHash{{Path: unit + "/000_plan.md"}, {Path: "../out/secret.md"}}
	if got := Recomputed("ws", in); len(got) != 2 || got[0].Sha256 != reviewRoundArgsHex("# plan\n") || got[1].Sha256 != "missing" {
		t.Errorf("relative cwd: %v", got)
	}
}

// An argument is decoded as Node decodes argv: each maximal invalid subpart becomes one U+FFFD (two bad bytes make two, a truncated
// three-byte sequence makes one), so it selects the file whose name holds that many replacement characters.
func TestReviewRoundArgsPathDecodedLikeArgv(t *testing.T) {
	cwd, unit := t.TempDir(), "u"
	one, two := unit+"/000_\uFFFD.md", unit+"/000_\uFFFD\uFFFD.md"
	reviewRoundArgsWrite(t, filepath.Join(cwd, one), "one\n")
	reviewRoundArgsWrite(t, filepath.Join(cwd, two), "two\n")
	for _, c := range []struct{ arg, want string }{{"000_\xff\xff.md", two}, {"000_\xe2\x82.md", one}, {"000_\xff.md", one}} {
		files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, []string{unit + "/" + c.arg})
		if err != nil || refusal != "" || len(files) != 1 || files[0].Path != c.want {
			t.Errorf("%q: %v %q %v, want %s", c.arg, files, refusal, err, c.want)
		}
	}
}

// Paths sort as JavaScript sorts strings, by UTF-16 code unit: a supplementary character (a surrogate pair, 0xD800 and up) comes
// before U+E000, which is the reverse of byte order.
func TestReviewRoundArgsPathsSortByUTF16(t *testing.T) {
	cwd, unit := t.TempDir(), "u"
	bmp, astral := unit+"/000_\uE000.md", unit+"/000_\U00010000.md"
	for _, p := range []string{bmp, astral} {
		reviewRoundArgsWrite(t, filepath.Join(cwd, p), "x\n")
	}
	for _, order := range [][]string{{bmp, astral}, {astral, bmp}} {
		files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, order)
		if err != nil || refusal != "" || len(files) != 2 || files[0].Path != astral || files[1].Path != bmp {
			t.Errorf("order %q: %v %q %v", order, files, refusal, err)
		}
	}
}

// CODEX_HOME names the config when it is set and not empty, and HOME/.codex only otherwise; the two files disagree here, and the
// variables are a map, so no run reads the real home.
func TestReviewRoundArgsCodexHomeBeatsHome(t *testing.T) {
	codexHome, home := t.TempDir(), t.TempDir()
	reviewRoundArgsWrite(t, filepath.Join(codexHome, "config.toml"), "[features]\nmulti_agent_v2 = false\n")
	reviewRoundArgsWrite(t, filepath.Join(home, ".codex", "config.toml"), "[features]\nmulti_agent_v2 = true\n")
	for _, c := range []struct {
		vars map[string]string
		want bool
	}{{map[string]string{"CODEX_HOME": codexHome, "HOME": home}, false}, {map[string]string{"CODEX_HOME": "", "HOME": home}, true}, {map[string]string{"HOME": home}, true}} {
		if got, err := reviewRoundArgsV2SpawnSurface(reviewRoundArgsEnv(c.vars)); err != nil || got != c.want {
			t.Errorf("%v: %v %v, want %v", c.vars, got, err, c.want)
		}
	}
}

// A relative path is made absolute against the kernel's working directory, not against $PWD: here $PWD names the directory through a
// link, and "../lnk/f.md" must climb out of the real directory (to a path that does not exist) instead of out of the link.
func TestReviewRoundArgsRelativePathUsesKernelDirectory(t *testing.T) {
	root := t.TempDir()
	reviewRoundArgsWrite(t, filepath.Join(root, "ws", "deep", "f.md"), "x\n")
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws", "deep"), filepath.Join(root, "lnk")))
	t.Chdir(filepath.Join(root, "lnk"))
	if got := Recomputed(".", []goalplan.PlanFileHash{{Path: "f.md"}, {Path: "../lnk/f.md"}}); len(got) != 2 || got[0].Sha256 != reviewRoundArgsHex("x\n") || got[1].Sha256 != "missing" {
		t.Errorf("got %v", got)
	}
}

// A home that cannot be made absolute is an error, where the oracle's resolve(homedir(), ".codex") throws outside its try: HOME is
// empty and the process's own directory is gone.
func TestReviewRoundArgsHomeThatCannotBeResolvedIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a removed working directory is not an error there")
	}
	gone := t.TempDir()
	t.Chdir(gone)
	reviewRoundArgsMust(t, os.Remove(gone))
	env := reviewRoundArgsEnv(map[string]string{"CODEX_HOME": "", "HOME": ""})
	if got, err := reviewRoundArgsV2SpawnSurface(env); err == nil || got {
		t.Errorf("probe: %v %v, want an error", got, err)
	}
	if packet, err := reviewRoundArgsRenderOpenPacket(goalplan.ReviewRoundState{}, 0, env); err == nil || packet != "" {
		t.Errorf("packet: %q %v, want an error", packet, err)
	}
}
