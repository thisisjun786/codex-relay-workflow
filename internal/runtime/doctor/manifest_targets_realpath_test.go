package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestManifestTargetsRealpathFallback pins the two failure paths of the walk the containment judgement
// depends on, which are the ones that send BOTH paths to the oracle's lexical branch: a component that
// cannot be read, and a link chain past the depth a realpath follows. Neither may answer a partial path,
// because targetEscapesRoot reads the pair.
func TestManifestTargetsRealpathFallback(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := manifestTargetsRealpath(filepath.Join(base, "real", "absent")); err == nil {
		t.Fatalf("a missing leaf answered a path instead of an error")
	}
	if _, err := manifestTargetsRealpath(filepath.Join(base, "absent", "deeper")); err == nil {
		t.Fatalf("a missing component answered a path instead of an error")
	}
	// A chain longer than the depth a realpath follows is an error, not a resolved path.
	previous := filepath.Join(base, "loop-0")
	if err := os.Symlink("loop-0", previous); err != nil {
		t.Fatal(err)
	}
	if _, err := manifestTargetsRealpath(previous); err == nil {
		t.Fatalf("a self-referential link answered a path instead of an error")
	}
	// The paired fallback then compares the original strings, so a root reached through an unresolvable
	// component is not reported as escaped when the target is inside it lexically.
	root := filepath.Join(base, "real")
	if targetEscapesRoot(root, filepath.Join(root, "leaf.json")) {
		t.Fatalf("a target inside a resolvable root was reported as escaped")
	}
	if !targetEscapesRoot(root, filepath.Join(base, "elsewhere", "leaf.json")) {
		t.Fatalf("a target outside the root was not reported as escaped")
	}
}

// TestManifestTargetsRealpathSymlinkStillEscapes pins that following a link by restarting at its target
// does not lose the containment property: a link out of the root still answers an escape.
func TestManifestTargetsRealpathSymlinkStillEscapes(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "leaf.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if !targetEscapesRoot(root, filepath.Join(root, "link", "leaf.js")) {
		t.Fatalf("a target reached through a link out of the root was not reported as escaped")
	}
}

// targetKernelRealpath is the oracle's answer for one path, and the judgement the CRW-840 cases are
// compared against. Node's realpathSync resolves the argument with path.resolve first, then walks the
// components in order, and filepath.EvalSymlinks is that same walk in Go: each link is resolved where it
// occurs, an ordinary component that does not exist is ENOENT there, and '..' drops one component only
// after the component before it resolved. filepath.Abs is path.resolve's lexical resolve here.
func targetKernelRealpath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// targetRealpathMatchesKernel runs both walks over path and fails unless they agree on whether the path
// resolves and, when it does, on the resolved location. The port answers the caller's spelling for the
// components before the first link (CRW-652) and leaves the '..' components it walked in place, while the
// kernel answers a normalized path, so the resolved locations are compared after filepath.Clean -- which
// is what targetEscapesRoot itself does with both strings. The comparison is sound for a path whose
// components are plain names, which is why these cases spell no lone surrogate.
func targetRealpathMatchesKernel(t *testing.T, path string) {
	t.Helper()
	want, wantErr := targetKernelRealpath(path)
	got, gotErr := manifestTargetsRealpath(path)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("manifestTargetsRealpath(%q) = %q, %v; the kernel answers %q, %v", path, got, gotErr, want, wantErr)
	}
	if wantErr == nil && filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("manifestTargetsRealpath(%q) = %q; the kernel answers %q", path, got, want)
	}
}

// targetRealpathFixture builds the tree the kernel-order cases walk: root, root/sub, an outside
// directory and a hook.json in root and in outside. The paths handed to the walk are built by hand, never
// with filepath.Join, because Join would clean away the very '..' components under test.
func targetRealpathFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(root, "hook.json"), filepath.Join(outside, "hook.json")} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// TestManifestTargetsRealpathKernelOrder is the CRW-840 case: the walk answers what the kernel answers,
// because a link target is joined to the components still left without being cleaned first. A component
// that does not exist is ENOENT where it occurs, and '..' drops one component only after the component
// before it resolved to an existing directory, which is the kernel's order.
func TestManifestTargetsRealpathKernelOrder(t *testing.T) {
	root, outside := targetRealpathFixture(t)
	sep := string(filepath.Separator)
	t.Run("a_normal_hook_file", func(t *testing.T) {
		targetRealpathMatchesKernel(t, root+sep+"hook.json")
	})
	t.Run("dotdot_after_an_existing_component", func(t *testing.T) {
		targetRealpathMatchesKernel(t, root+sep+"sub"+sep+".."+sep+"hook.json")
	})
	t.Run("dotdot_after_a_missing_component", func(t *testing.T) {
		targetRealpathMatchesKernel(t, root+sep+"missing"+sep+".."+sep+"hook.json")
	})
	t.Run("a_link_chain", func(t *testing.T) {
		if err := os.Symlink("hook.json", filepath.Join(root, "chain-1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("chain-1", filepath.Join(root, "chain-2")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesKernel(t, root+sep+"chain-2")
	})
	t.Run("a_link_loop", func(t *testing.T) {
		if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesKernel(t, root+sep+"loop")
	})
	t.Run("a_link_leaving_the_root", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesKernel(t, root+sep+"out"+sep+"hook.json")
	})
}

// TestManifestTargetsRealpathKernelOrderBelowALink is the evaluation's own shape: the link target names a
// component that does not exist before a '..'. The kernel stops at that component with ENOENT, so the
// port must stop there too. Both link targets below keep a hook.json reachable after the lexical Clean
// the port used to apply, which is exactly why cleaning answered a resolved path where the kernel answers
// ENOENT and targetEscapesRoot skipped its paired lexical fallback.
func TestManifestTargetsRealpathKernelOrderBelowALink(t *testing.T) {
	root, _ := targetRealpathFixture(t)
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"a_missing_component_after_a_leading_dotdot", ".." + sep + "outside" + sep + "missing" + sep + ".." + sep + "hook.json"},
		{"a_missing_component_below_the_root", "." + sep + "missing" + sep + ".." + sep + "hook.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := root + sep + "bad-" + tc.name + ".json"
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			targetRealpathMatchesKernel(t, link)
			if _, err := manifestTargetsRealpath(link); err == nil {
				t.Fatalf("the walk answered a path for a link target with a missing component")
			}
		})
	}
}

// TestManifestTargetsRealpathKernelOrderVerdict is the user-visible answer of the evaluation's case: the
// manifest names a link whose target walks through a component that does not exist, so the file the
// manifest names is missing. dev answered "escapes plugin root" here because the walk cleaned the missing
// component away and resolved to a file outside the root.
func TestManifestTargetsRealpathKernelOrderVerdict(t *testing.T) {
	root, _ := targetRealpathFixture(t)
	// The link target is spelled out, never built with filepath.Join: Join cleans the missing component
	// and the '..' away, which is the very pair this case is about.
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

// TestManifestTargetsRealpathKernelOrderKeepsContainment: the same walk still answers the escape when a
// link really reaches outside the root through an existing component, so removing the Clean did not cost
// the containment property.
func TestManifestTargetsRealpathKernelOrderKeepsContainment(t *testing.T) {
	root, outside := targetRealpathFixture(t)
	sep := string(filepath.Separator)
	if err := os.Symlink(".."+sep+"outside", filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./out/hook.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ./out/hook.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
	if !targetEscapesRoot(root, outside+sep+"hook.json") {
		t.Fatalf("a target outside the root was not reported as escaped")
	}
}
