package spawn

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// testdata/hook/oracle.json holds what the CXC v0.2.40 spawn hook printed, recorded under Node 24 by
// testdata/hook/record-hook.mjs against a scratch environment (no Node runs here). A step stores the CRW-named input, the sha256 of
// the raw output, the renamed expected output and the seam's expectation: the output alone cannot tell a stop from a finished
// assembly (an exact guard reapplication assembles, then prints nothing). The oracle's output also holds the second half of the hook
// (routing, envelope), so the replay composes the final updatedInput from the assembly by the next issue's rule, which lives here only.

type spawnHookStep struct {
	Seam       string          `json:"seam"`
	Input      json.RawMessage `json:"input"`
	Kind       string          `json:"kind"`
	Expected   json.RawMessage `json:"expected"`
	Role       string          `json:"role"`
	GrantFiles *int            `json:"grantFiles"`
	TmpExists  *bool           `json:"tmpExists"`
	Class      string          `json:"classification"`
	Reason     string          `json:"reason"`
	Note       string          `json:"note"`
}

type spawnHookCase struct {
	Name string `json:"name"`
	Env  struct {
		Skills        string  `json:"skills"`
		Tmp           string  `json:"tmp"`
		Store         *string `json:"store"`
		UnreadableCwd bool    `json:"unreadableCwd"`
	} `json:"env"`
	Steps []spawnHookStep `json:"steps"`
}

type spawnHookFixture struct {
	Skills        map[string]string `json:"skills"`
	AffordanceCap struct {
		MaxUnits int `json:"maxUnits"`
		Cases    []struct {
			Unit     string `json:"unit"`
			Delta    int    `json:"delta"`
			Appended bool   `json:"appended"`
		} `json:"cases"`
	} `json:"affordanceCap"`
	Cases []spawnHookCase `json:"cases"`
}

// spawnHookRig is one case's scratch environment: a workspace, a skills tree of the recorded synthetic skills, a temp root, a CRW
// home with the case's store, and the environment the seam reads. Nothing outside the test's temporary directory is touched.
type spawnHookRig struct {
	dir, ws, skills, tmp string
	env                  host.LookupEnv
	blocks               map[string]string
	nonces               []string
}

func spawnHookNewRig(t *testing.T, skills map[string]string, c spawnHookCase) *spawnHookRig {
	t.Helper()
	r := &spawnHookRig{dir: t.TempDir()}
	r.ws, r.tmp = filepath.Join(r.dir, "ws"), filepath.Join(r.dir, "tmp")
	dir := filepath.Join(r.dir, "skills")
	for folder, body := range skills {
		spawnHookMust(t, os.MkdirAll(filepath.Join(dir, folder), 0o755))
		spawnHookMust(t, os.WriteFile(filepath.Join(dir, folder, "SKILL.md"), []byte(body), 0o644))
	}
	spawnHookMust(t, os.MkdirAll(dir, 0o755))
	spawnHookMust(t, os.MkdirAll(r.ws, 0o755))
	r.skills = spawnHookEval(t, dir)
	configured := dir
	switch c.Env.Skills {
	case "link":
		configured = filepath.Join(r.dir, "skills-link")
		spawnHookMust(t, os.Symlink(dir, configured))
	case "padded":
		configured = " \t" + dir + "\n "
	}
	if c.Env.Tmp == "missing" {
		r.tmp = filepath.Join(r.dir, "absent", "tmp")
	} else {
		spawnHookMust(t, os.Mkdir(r.tmp, 0o700))
	}
	home := filepath.Join(r.dir, "crw")
	spawnHookMust(t, os.MkdirAll(home, 0o755))
	if c.Env.Store != nil {
		spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(*c.Env.Store), 0o644))
	}
	vars := map[string]string{"HOME": filepath.Join(r.dir, "home"), "CODEX_HOME": filepath.Join(r.dir, "codex"), "CRW_HOME": home, "TMPDIR": r.tmp, "CRW_SKILLS_DIR": configured}
	r.env = func(key string) (string, bool) { v, ok := vars[key]; return v, ok }
	r.blocks = map[string]string{"{{LEAF}}": LeafGuardBlock, "{{LEAF_COORD}}": LeafGuardBlockCoordinator, "{{V1}}": V1ScopeBlock,
		"{{V1_COORD}}": V1ScopeBlockCoordinator, "{{AFFORDANCE}}": SkillAffordanceBlock(r.skills)}
	return r
}

// expand fills a recorded value in: the guard and affordance blocks, the scratch paths and the nonces minted so far.
func (r *spawnHookRig) expand(v any) any {
	switch v := v.(type) {
	case string:
		for token, block := range r.blocks {
			v = strings.ReplaceAll(v, token, block)
		}
		v = strings.NewReplacer("{SKILLS}", r.skills, "{WS}", r.ws).Replace(v)
		for i, nonce := range r.nonces {
			v = strings.ReplaceAll(v, fmt.Sprintf("{N%d}", i+1), nonce)
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = r.expand(v[i])
		}
		return out
	case pyjson.Object:
		out := make(pyjson.Object, len(v))
		for i, f := range v {
			out[i] = pyjson.Field{Key: f.Key, Value: r.expand(f.Value)}
		}
		return out
	}
	return v
}

func spawnHookMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func spawnHookEval(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	spawnHookMust(t, err)
	return real
}

func spawnHookLoad(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := pyjson.Loads(string(raw), pyjson.LoadOptions{Python: true, Surrogates: true})
	spawnHookMust(t, err)
	return v
}

func spawnHookCount(dir string) (n int) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// spawnHookCompose is how the next issue finishes the assembly when role routing leaves the message alone (no promptOverride, no
// trust warning): the ciphertext restore (:1052), the first text item replaced or a guard item put in front (:1053-1056) and the
// skill blocks appended to the last text item while the total stays within the cap (:1057-1064).
func spawnHookCompose(a spawnHookAssembly) pyjson.Object {
	updated := a.updatedMessage
	if a.encryptedV2Message {
		updated = a.message
	}
	out := slices.Clone(a.toolInput)
	if !a.validItems {
		return out.Set("message", updated)
	}
	items := make([]any, len(a.mappedItems))
	for i, item := range a.mappedItems {
		items[i] = item
	}
	if a.firstText < 0 {
		items = slices.Insert(items, 0, any(pyjson.Object{{Key: "type", Value: "text"}, {Key: "text", Value: updated}}))
	} else {
		items[a.firstText] = slices.Clone(a.mappedItems[a.firstText]).Set("text", updated)
	}
	if len(a.itemBlocks) > 0 {
		var texts []int
		units := 0
		for i, item := range items {
			if o := item.(pyjson.Object); o.Get("type") == "text" {
				texts = append(texts, i)
				units += spawnInlineUTF16Units(o.Get("text").(string))
			}
		}
		units += (len(texts) - 1) * 2
		suffix := "\n\n" + strings.Join(a.itemBlocks, "\n\n")
		if last := texts[len(texts)-1]; units+spawnInlineUTF16Units(suffix) <= spawnNormalizeMaxLength {
			o := items[last].(pyjson.Object)
			items[last] = slices.Clone(o).Set("text", o.Get("text").(string)+suffix)
		}
	}
	return out.Set("items", items)
}

func spawnHookWithoutRouting(o pyjson.Object) (out pyjson.Object) {
	for _, f := range o {
		if f.Key != "model" && f.Key != "reasoning_effort" {
			out = append(out, f)
		}
	}
	return out
}

func spawnHookReadFixture(t *testing.T) (fixture spawnHookFixture) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hook", "oracle.json"))
	spawnHookMust(t, err)
	spawnHookMust(t, json.Unmarshal(data, &fixture))
	return fixture
}

func TestSpawnHookOracleReplay(t *testing.T) {
	fixture := spawnHookReadFixture(t)
	if len(fixture.Cases) == 0 {
		t.Fatal("no recorded cases")
	}
	steps := 0
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			rig := spawnHookNewRig(t, fixture.Skills, c)
			if c.Env.UnreadableCwd {
				dead := filepath.Join(rig.dir, "dead")
				spawnHookMust(t, os.Mkdir(dead, 0o755))
				t.Chdir(dead)
				spawnHookMust(t, os.Remove(dead))
			}
			for i, step := range c.Steps {
				steps++
				if step.Class != "identical" && (step.Class != "intentionally-changed" || step.Reason == "") {
					t.Fatalf("step %d is unclassified", i+1)
				}
				at := fmt.Sprintf("step %d (%s)", i+1, step.Note)
				input := rig.expand(spawnHookLoad(t, step.Input)).(pyjson.Object)
				asm, deny, stop := spawnHookAssemble(spawnHookView(input), rig.env)
				if m := regexp.MustCompile(`\[CRW-SUBSPAWN-GRANT:([a-f0-9]{64})\]`).FindStringSubmatch(asm.guard); m != nil {
					rig.nonces = append(rig.nonces, m[1])
				}
				switch step.Seam {
				case "stop-empty", "stop-deny":
					var want string
					if step.Seam == "stop-deny" {
						spawnHookMust(t, json.Unmarshal(step.Expected, &want))
					}
					if !stop || deny != want || !reflect.DeepEqual(asm, spawnHookAssembly{}) {
						t.Fatalf("%s: stop=%v deny=%q, want a stop with deny %q and the zero assembly", at, stop, deny, want)
					}
				case "assembled":
					if stop || deny != "" {
						t.Fatalf("%s: stopped (deny %q), want an assembly", at, deny)
					}
					got, want := spawnHookCompose(asm), input.Get("tool_input")
					if step.Kind == "allow" {
						expected := rig.expand(spawnHookLoad(t, step.Expected)).(pyjson.Object)
						want = spawnHookWithoutRouting(expected.Get("updatedInput").(pyjson.Object))
						if cipher := expected.Get("ciphertext") == true; cipher != asm.encryptedV2Message {
							t.Fatalf("%s: encryptedV2Message = %v, want %v", at, asm.encryptedV2Message, cipher)
						}
					} else if asm.encryptedV2Message {
						t.Fatalf("%s: encryptedV2Message on an unchanged message", at)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: composed updatedInput\n%v\nwant\n%v", at, got, want)
					}
					if step.Role != "" && (string(asm.role) != step.Role || asm.resolution.Model == nil || *asm.resolution.Model != "rec/"+step.Role) {
						t.Fatalf("%s: role %q resolution %+v, want %q", at, asm.role, asm.resolution, step.Role)
					}
				default:
					t.Fatalf("%s: unknown seam %q", at, step.Seam)
				}
				if step.GrantFiles != nil && spawnHookCount(rig.tmp) != *step.GrantFiles {
					t.Fatalf("%s: %d grant files, want %d", at, spawnHookCount(rig.tmp), *step.GrantFiles)
				}
				if _, err := os.Stat(rig.tmp); step.TmpExists != nil && (err == nil) != *step.TmpExists {
					t.Fatalf("%s: temp root exists = %v, want %v", at, err == nil, *step.TmpExists)
				}
			}
		})
	}
	t.Logf("replayed %d recorded steps", steps)
}

// TestSpawnHookAffordanceCap: the affordance is appended while the candidate stays within 256 KiB of UTF-16 units, counted per
// unit and not per byte or rune.
func TestSpawnHookAffordanceCap(t *testing.T) {
	fixture := spawnHookReadFixture(t)
	rig := spawnHookNewRig(t, fixture.Skills, spawnHookCase{})
	for _, c := range fixture.AffordanceCap.Cases {
		n := fixture.AffordanceCap.MaxUnits - 2 - spawnInlineUTF16Units(SkillAffordanceBlock(rig.skills)) + c.Delta
		message := strings.Repeat(c.Unit, n)
		if len(c.Unit) > 1 {
			message = strings.Repeat(c.Unit, n/2) + strings.Repeat("a", n%2)
		}
		asm, _, stop := spawnHookAssemble(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "spawn_agent",
			"tool_input": map[string]any{"task_name": "t", "message": message}}, rig.env)
		if got := !stop && strings.Contains(asm.updatedMessage, SkillAffordanceMarker); got != c.Appended || spawnInlineUTF16Units(message) != n {
			t.Errorf("unit %q delta %d: appended = %v, want %v", c.Unit, c.Delta, got, c.Appended)
		}
	}
}

func TestSpawnHookAssemblyFields(t *testing.T) {
	rig := spawnHookNewRig(t, map[string]string{"crw-dev": "---\nname: crw-dev\n---\nbody\n"}, spawnHookCase{})
	wd, err := syscall.Getwd()
	spawnHookMust(t, err)
	spawn := func(extra map[string]any, tool any) map[string]any {
		obj := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "session_id": "s1", "tool_input": tool}
		for k, v := range extra {
			obj[k] = v
		}
		return obj
	}
	// A plain map is the compatibility form of tool_input and of its items: keys are read in sorted order.
	a, deny, stop := spawnHookAssemble(spawn(map[string]any{"cwd": rig.ws}, map[string]any{"message": "hello", "agent_type": "worker"}), rig.env)
	if stop || deny != "" || a.v2Spawn || a.validItems || a.itemInput != nil || a.mappedItems != nil || a.firstText != -1 || a.cwd != rig.ws ||
		a.message != "hello" || a.role != "executor" || a.resolution.Role != a.role || a.trustPrefix != "" || a.guard != V1ScopeBlock ||
		a.updatedMessage != V1ScopeBlock+"\n\nhello" || a.itemBlocks != nil || a.encryptedV2Message || a.toolInput.Get("agent_type") != "worker" {
		t.Fatalf("v1 message: %+v", a)
	}
	a, _, _ = spawnHookAssemble(spawn(nil, map[string]any{"task_name": "t", "message": "$crw-dev"}), rig.env)
	if !a.v2Spawn || a.cwd != wd || a.guard != LeafGuardBlock || !strings.Contains(a.updatedMessage, "<skill name=\"crw-dev\">") || strings.Contains(a.updatedMessage, SkillAffordanceMarker) {
		t.Fatalf("v2 message: %+v", a)
	}
	if _, _, stop = spawnHookAssemble(spawn(nil, nil), rig.env); !stop {
		t.Fatal("a null tool_input is no object")
	}
	// Items keep their order and are not changed in place: the oracle's {...item, text} copies, and so must the port.
	items := []any{pyjson.Object{{Key: "text", Value: " a $crw-dev "}, {Key: "type", Value: "text"}}, pyjson.Object{{Key: "type", Value: "image"}, {Key: "id", Value: "x"}}}
	tool := pyjson.Object{{Key: "agent_type", Value: "explorer"}, {Key: "items", Value: items}, {Key: "extra", Value: true}}
	a, _, stop = spawnHookAssemble(spawn(map[string]any{"cwd": rig.ws}, tool), rig.env)
	if stop || !a.validItems || a.firstText != 0 || len(a.textItems) != 1 || a.mappedItems[0][0].Key != "text" || a.mappedItems[0][1].Key != "type" ||
		a.mappedItems[1].Get("id") != "x" || len(a.itemBlocks) != 1 || items[0].(pyjson.Object).Get("text") != " a $crw-dev " || a.toolInput[2].Key != "extra" {
		t.Fatalf("items: %+v (original %v)", a, items)
	}
	if text := a.mappedItems[0].Get("text").(string); !strings.HasPrefix(text, " a [$crw-dev](skill://") {
		t.Fatalf("first item text %q was not normalized without trimming", text)
	}
}

func TestSpawnHookSkillsDir(t *testing.T) {
	dir := t.TempDir()
	real, plugin := filepath.Join(dir, "real"), filepath.Join(dir, "plugin")
	for _, d := range []string{real, filepath.Join(plugin, "skills")} {
		spawnHookMust(t, os.MkdirAll(d, 0o755))
	}
	spawnHookMust(t, os.Symlink(real, filepath.Join(dir, "link")))
	resolved, pluginSkills := spawnHookEval(t, real), spawnHookEval(t, filepath.Join(plugin, "skills"))
	for name, tc := range map[string]struct{ override, plugin, want string }{
		"trimmed link is resolved":                {" \u00a0" + filepath.Join(dir, "link") + "\n", "", resolved},
		"override wins over the plugin":           {real, plugin, resolved},
		"blank override falls to the plugin":      {" \t", plugin, pluginSkills},
		"missing override falls to the plugin":    {filepath.Join(dir, "none"), plugin, pluginSkills},
		"missing override, plugin without skills": {filepath.Join(dir, "none"), dir, ""},
		"nothing set": {"", "", ""},
	} {
		env := func(key string) (string, bool) {
			return map[string]string{"CRW_SKILLS_DIR": tc.override, "PLUGIN_ROOT": tc.plugin}[key], true
		}
		if got := spawnHookSkillsDir(env); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestSpawnHookTmpDir(t *testing.T) {
	for _, tc := range []struct {
		vars map[string]string
		want string
	}{
		{map[string]string{"TMPDIR": "/a/b/", "TMP": "/t"}, "/a/b"}, {map[string]string{"TMP": "/t", "TEMP": "/e"}, "/t"}, {map[string]string{"TMPDIR": "", "TEMP": "/e//"}, "/e/"},
		{map[string]string{"TMPDIR": "/"}, "/"}, {nil, "/tmp"},
	} {
		if got := spawnHookTmpDir(func(key string) (string, bool) { v, ok := tc.vars[key]; return v, ok }); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.vars, got, tc.want)
		}
	}
}
