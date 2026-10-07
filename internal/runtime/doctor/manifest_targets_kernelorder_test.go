package doctor

// This file is the CRW-937 test for the manifest-target walk's component order. Before the fix
// manifestTargetsFollow joined a link target to the components still to walk and ran filepath.Clean
// over the result, so a '..' inside the link target was dropped lexically before anything was
// walked. With root/sublink -> ../outside/deep and root/bad.json -> sublink/../hook.json the kernel
// (filepath.EvalSymlinks) answers <base>/outside/hook.json while the walk answered root/hook.json, so
// an outside hook JSON was judged inside the plugin root and read.
//
// Every expectation below is either the kernel's own answer or a verdict this port has always given;
// the oracle (Node fs.realpathSync) resolves the link target lexically with path.resolve, so the
// escape rows are a deliberate divergence from it, recorded in docs/port-cxc/known-defects/CRW-937.md.
//
// Fixtures are built with filepath.Join only for the parts that hold no '..'; a link target that must
// keep a '..' is spelled out, because Join would clean it away.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// kernelOrderFixture builds root/sub/deep, root/outside-free layout the escape rows share: a root, a
// root/sub/deep directory, and an outside directory with a deep child holding hook.json. It returns
// root and outside.
func kernelOrderFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "sub", "deep"), filepath.Join(outside, "deep")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "hook.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

// kernelOrderBadLink adds root/sublink -> ../outside/deep and root/bad.json -> sublink/../hook.json,
// the issue's fixture: the walk must climb out of the directory the link really reached.
func kernelOrderBadLink(t *testing.T, root string) {
	t.Helper()
	if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.json", filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
}

// TestManifestTargetsKernelOrderHookFileEscapes is the issue's own case: the manifest names a hook
// document that is a link whose target climbs out of the root. The kernel resolves it outside, so the
// verdict is an escape; before the fix the walk answered root/hook.json and reported nothing.
func TestManifestTargetsKernelOrderHookFileEscapes(t *testing.T) {
	root, outside := kernelOrderFixture(t)
	kernelOrderBadLink(t, root)
	if err := os.WriteFile(filepath.Join(root, "hook.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./bad.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ./bad.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
	// The kernel's own answer for the same path, so the verdict above is not self-referential.
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "bad.json"))
	if err != nil || resolved != filepath.Join(outside, "hook.json") {
		t.Fatalf("EvalSymlinks(bad.json) = %q, %v; want %q", resolved, err, filepath.Join(outside, "hook.json"))
	}
}

// TestManifestTargetsKernelOrderMCPServersFileEscapes is the same shape on the mcpServers path: it
// shares targetEscapesRoot, so it must reach the same verdict.
func TestManifestTargetsKernelOrderMCPServersFileEscapes(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	kernelOrderBadLink(t, root)
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./bad.json"}`)
	want := []TargetIssue{{TargetMCP, "manifest mcpServers file escapes plugin root: ./bad.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderHookCommandTargetEscapes is the same shape reached from a hook
// command's ${PLUGIN_ROOT} target, which goes through targetCheck.
func TestManifestTargetsKernelOrderHookCommandTargetEscapes(t *testing.T) {
	root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/cmd-bad.js\""`, `null`, `[]`)
	if err := os.MkdirAll(filepath.Join(root, "..", "outside", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "..", "outside", "hook.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.js", filepath.Join(root, "cmd-bad.js")); err != nil {
		t.Fatal(err)
	}
	want := []TargetIssue{{TargetHook, "target escapes plugin root: cmd-bad.js"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderMCPArgumentEscapes is the same shape as an mcp `.js` argument, which
// checkTarget judges with the same resolution.
func TestManifestTargetsKernelOrderMCPArgumentEscapes(t *testing.T) {
	root := targetTestRoot(t, `null`, `null`, `["./cmd-bad.js"]`)
	if err := os.MkdirAll(filepath.Join(root, "..", "outside", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "..", "outside", "hook.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.js", filepath.Join(root, "cmd-bad.js")); err != nil {
		t.Fatal(err)
	}
	want := []TargetIssue{{TargetMCP, "target escapes plugin root: ./cmd-bad.js"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderStaysInsideTheRoot is the control the issue names: a link whose
// target climbs and then returns inside the root is still accepted, because the climb starts from
// where the link really led (root/sub), not from a lexical prefix.
func TestManifestTargetsKernelOrderStaysInsideTheRoot(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	if err := os.Symlink("sub/deep", filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.json", filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "hook.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./bad.json"]}`)
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, []TargetIssue{}) {
		t.Fatalf("issues: %+v %v; want none", got, err)
	}
	// The kernel agrees the target is inside the root.
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "bad.json"))
	if err != nil || resolved != filepath.Join(root, "sub", "hook.json") {
		t.Fatalf("EvalSymlinks(bad.json) = %q, %v; want %q", resolved, err, filepath.Join(root, "sub", "hook.json"))
	}
}

// TestManifestTargetsKernelOrderMissingComponentKeepsPairedFallback is CRW-652's case, kept: a link
// whose target names a component that does not exist is an error before the climb, so both paths go
// lexical and the verdict is "missing", never "escapes".
func TestManifestTargetsKernelOrderMissingComponentKeepsPairedFallback(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	// Spelled out: filepath.Join would clean the missing component and the '..' away.
	target := ".." + string(filepath.Separator) + "outside" + string(filepath.Separator) + "missing" +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "hook.json"
	if err := os.Symlink(target, filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./bad.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file missing: ./bad.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderPlainTargetUnchanged pins that a path with no link and no '..' is
// judged exactly as before: present, missing and empty all keep their answers.
func TestManifestTargetsKernelOrderPlainTargetUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want []TargetIssue
	}{
		{"present", "data", []TargetIssue{}},
		{"empty", "", []TargetIssue{{TargetHook, "target is empty: plain.js"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/plain.js\""`, `null`, `[]`)
			targetTestWrite(t, root, "plain.js", tc.data)
			if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("issues: %+v %v; want %+v", got, err, tc.want)
			}
		})
	}
	// A missing plain target keeps its own answer too.
	root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/gone.js\""`, `null`, `[]`)
	want := []TargetIssue{{TargetHook, "hook references missing dist: gone.js"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderAgreesWithEvalSymlinks requires the walk's answer to equal the
// kernel's for every reachable shape the issue names, compared as targetEscapesRoot compares them
// (after filepath.Clean). The unreachable shapes are covered by the paired-fallback row above.
func TestManifestTargetsKernelOrderAgreesWithEvalSymlinks(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.json", filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/deep", filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside/../hook.json", filepath.Join(root, "good.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "hook.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hook.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	for _, path := range []string{
		filepath.Join(root, "hook.json"),
		filepath.Join(root, "bad.json"),
		filepath.Join(root, "good.json"),
		filepath.Join(root, "sublink"),
		root + sep + "sublink" + sep + ".." + sep + "hook.json",
	} {
		t.Run(strings.TrimPrefix(path, root+sep), func(t *testing.T) {
			// The kernel is asked the path exactly as the caller spelled it, '..' and all: that is
			// the file a hook command naming this target would open, so it is the comparison that
			// matters.
			want, wantErr := filepath.EvalSymlinks(path)
			got, gotErr := manifestTargetsRealpath(path)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("manifestTargetsRealpath(%q) = %q, %v; the kernel answers %q, %v", path, got, gotErr, want, wantErr)
			}
			if wantErr == nil && filepath.Clean(got) != filepath.Clean(want) {
				t.Fatalf("manifestTargetsRealpath(%q) = %q; the kernel answers %q", path, got, want)
			}
		})
	}
}

// TestManifestTargetsKernelOrderDriftMCPCaller drives the same fixture through harnessDriftMCPCheck,
// the drift:mcp check, so the shared resolution is visible at its other caller as well.
func TestManifestTargetsKernelOrderDriftMCPCaller(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	kernelOrderBadLink(t, root)
	manifestPath := filepath.Join(root, ".codex-plugin", "plugin.json")
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./bad.json"}`)
	manifest, err := harnessDriftReadJSON(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	check := harnessDriftMCPCheck(root, manifest)
	if check.Severity != HarnessFail || !strings.Contains(check.Evidence, "resolves outside the plugin root") {
		t.Fatalf("drift:mcp check: %+v; want a FAIL naming the escape", check)
	}
}

// TestManifestTargetsKernelOrderLongTargetDoesNotFailOpen pins that the walk's answer does not depend
// on the length of the text it accumulates. A link target made of many './' components names no '..'
// at all, yet it grows the path the walk builds until os.Lstat answers ENAMETOOLONG; the walk would
// then return an error, targetEscapesRoot would take its paired lexical fallback, and the outside
// document would be judged inside the root. The kernel resolves it outside, so the verdict is an
// escape.
func TestManifestTargetsKernelOrderLongTargetDoesNotFailOpen(t *testing.T) {
	base := t.TempDir()
	// A long root, so the accumulated path crosses the kernel's limit while the link itself is
	// still short enough to create.
	root := filepath.Join(base, strings.Repeat("d", 100), strings.Repeat("e", 100), "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "hook.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strings.Repeat("./", 1950)+"out/hook.json", filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "bad.json"))
	if err != nil || resolved != filepath.Join(outside, "hook.json") {
		t.Fatalf("EvalSymlinks(bad.json) = %q, %v; want %q", resolved, err, filepath.Join(outside, "hook.json"))
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./bad.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ./bad.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsKernelOrderCallerPathEscapes is the pre-merge evaluation's d1: a manifest target
// whose OWN spelling names a link followed by '..'. targetResolve joined the whole spelling with
// filepath.Join, whose lexical Clean drops the '..' after the link before the walk sees it, so the
// judgement resolved root/hook.sh while a hook command naming the same string opens outside/hook.sh.
func TestManifestTargetsKernelOrderCallerPathEscapes(t *testing.T) {
	root, _ := kernelOrderFixture(t)
	kernelOrderBadLink(t, root)
	// The file the caller's spelling really reaches, and a decoy of the same name inside the root.
	if err := os.WriteFile(filepath.Join(root, "..", "outside", "hook.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hook.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("manifest_hooks_entry", func(t *testing.T) {
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./sublink/../hook.sh"]}`)
		want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ./sublink/../hook.sh"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
	t.Run("manifest_mcp_servers_file", func(t *testing.T) {
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./sublink/../hook.sh"}`)
		want := []TargetIssue{{TargetMCP, "manifest mcpServers file escapes plugin root: ./sublink/../hook.sh"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
	t.Run("hook_command_target", func(t *testing.T) {
		root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/sublink/../hook.sh\""`, `null`, `[]`)
		if err := os.MkdirAll(filepath.Join(root, "..", "outside", "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "..", "outside", "hook.sh"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "hook.sh"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
			t.Fatal(err)
		}
		want := []TargetIssue{{TargetHook, "target escapes plugin root: sublink/../hook.sh"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
}
