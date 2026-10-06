package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// targetOracleRealpath is an independent model of the oracle for one path: Node's fs.realpathSync, which
// escapesRoot calls and which the port has to match. It is written from that function's algorithm rather
// than from the port's walk:
//
//  1. resolve the argument lexically (path.resolve).
//  2. walk the components left to right. An ordinary component is lstated. A symlink component is
//     stat'ed first -- following the link, so a target that cannot be reached is that error -- and then
//     its target is resolved LEXICALLY against the directory reached so far
//     (path.resolve(previous, linkTarget)).
//  3. restart the walk at path.resolve(resolvedLink, remaining).
//
// The lexical step is what separates the oracle from the kernel's physical walk: the kernel walks a '..'
// inside a link target physically, so it leaves the directory the link reached, while Node's path.resolve
// drops that '..' against the lexical prefix.
func targetOracleRealpath(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	links := 0
	for {
		volume := filepath.VolumeName(p)
		parts := strings.Split(strings.TrimPrefix(p[len(volume):], sep), sep)
		dir := volume
		followed := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := dir + sep + part
			info, err := os.Lstat(next)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				dir = next
				continue
			}
			if _, err := os.Stat(next); err != nil {
				return "", err
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(dir, target)
			}
			if rest := strings.Join(parts[i+1:], sep); rest != "" {
				resolved = filepath.Join(resolved, rest)
			}
			p = filepath.Clean(resolved)
			links++
			if links > 255 {
				return "", errors.New("ELOOP: too many levels of symbolic links")
			}
			followed = true
			break
		}
		if !followed {
			if dir == "" {
				return sep, nil
			}
			return dir, nil
		}
	}
}

// targetRealpathMatchesOracle runs both the port's walk and the oracle model over path and fails unless
// they agree on whether the path resolves and on the resolved location. The port answers the caller's
// spelling for the components before the first link (CRW-652) and leaves the '..' components it walked in
// place, so the locations are compared after filepath.Clean -- which is what targetEscapesRoot does with
// both strings. The comparison is sound for a path whose components are plain names, which is why these
// cases spell no lone surrogate.
func targetRealpathMatchesOracle(t *testing.T, path string) {
	t.Helper()
	want, wantErr := targetOracleRealpath(path)
	got, gotErr := manifestTargetsRealpath(path)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("manifestTargetsRealpath(%q) = %q, %v; the oracle answers %q, %v", path, got, gotErr, want, wantErr)
	}
	if wantErr == nil && filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("manifestTargetsRealpath(%q) = %q; the oracle answers %q", path, got, want)
	}
}

// targetRealpathFixture builds the tree the walk cases use: root, root/sub, an outside directory with a
// deep child, and a hook.json in root and in outside. A path handed to the walk is built by hand where it
// must keep a '..', because filepath.Join would clean it away.
func targetRealpathFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, filepath.Join(root, "sub"), outside, filepath.Join(outside, "deep")} {
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

// TestManifestTargetsRealpathOracleAgreement drives the walk over every shape the issue names and requires
// the oracle's own answer: a normal hook file, '..' after an existing component, '..' after a missing
// component, a link chain, a link loop, a link leaving the root, and a '..' inside a link target.
func TestManifestTargetsRealpathOracleAgreement(t *testing.T) {
	root, outside := targetRealpathFixture(t)
	sep := string(filepath.Separator)
	t.Run("a_normal_hook_file", func(t *testing.T) {
		targetRealpathMatchesOracle(t, filepath.Join(root, "hook.json"))
	})
	t.Run("dotdot_after_an_existing_component", func(t *testing.T) {
		targetRealpathMatchesOracle(t, root+sep+"sub"+sep+".."+sep+"hook.json")
	})
	t.Run("dotdot_after_a_missing_component", func(t *testing.T) {
		targetRealpathMatchesOracle(t, root+sep+"missing"+sep+".."+sep+"hook.json")
	})
	t.Run("a_link_chain", func(t *testing.T) {
		if err := os.Symlink("hook.json", filepath.Join(root, "chain-1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("chain-1", filepath.Join(root, "chain-2")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesOracle(t, filepath.Join(root, "chain-2"))
	})
	t.Run("a_link_loop", func(t *testing.T) {
		if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesOracle(t, filepath.Join(root, "loop"))
	})
	t.Run("a_link_leaving_the_root", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesOracle(t, filepath.Join(root, "out", "hook.json"))
	})
	t.Run("dotdot_inside_a_link_target", func(t *testing.T) {
		// root/sublink -> ../outside/deep and root/bad.json -> sublink/../hook.json. The oracle stat's
		// bad.json (which reaches outside/hook.json), then resolves sublink's target lexically, so the
		// '..' drops 'deep' from the lexical prefix and the answer is root/hook.json -- not the
		// outside/hook.json the kernel's physical walk answers.
		if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("sublink/../hook.json", filepath.Join(root, "bad.json")); err != nil {
			t.Fatal(err)
		}
		targetRealpathMatchesOracle(t, filepath.Join(root, "bad.json"))
		got, err := manifestTargetsRealpath(filepath.Join(root, "bad.json"))
		if err != nil || filepath.Clean(got) != filepath.Join(root, "hook.json") {
			t.Fatalf("manifestTargetsRealpath(bad.json) = %q, %v; want the lexical %q", got, err, filepath.Join(root, "hook.json"))
		}
		if targetEscapesRoot(root, filepath.Join(root, "bad.json")) {
			t.Fatalf("the lexical answer inside the root was reported as escaped")
		}
	})
}

// TestManifestTargetsRealpathOracleAgreementMissingLeafBelowALink is the evaluation's own case: the link
// target names a component that does not exist before a '..'. Node's realpathSync stat's the link before
// reading it, so that stat is ENOENT and the walk answers an error; both paths then take the lexical
// fallback and the manifest verdict is "missing", not "escapes plugin root".
func TestManifestTargetsRealpathOracleAgreementMissingLeafBelowALink(t *testing.T) {
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
			targetRealpathMatchesOracle(t, link)
			if _, err := manifestTargetsRealpath(link); err == nil {
				t.Fatalf("the walk answered a path for a link target with a missing component")
			}
		})
	}
}

// TestManifestTargetsRealpathOracleAgreementVerdict is the user-visible answer of the evaluation's case:
// the manifest names a link whose target walks through a component that does not exist, so the file the
// manifest names is missing. dev answered "escapes plugin root" here because the walk resolved the target
// without first stat'ing the link.
func TestManifestTargetsRealpathOracleAgreementVerdict(t *testing.T) {
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

// TestManifestTargetsRealpathOracleRecorded compares the walk with answers recorded from the oracle itself,
// so the oracle model above is not the only witness. Each expectation was produced by running Node 24's
// fs.realpathSync on this same fixture, with the fixture root substituted for this test's root.
func TestManifestTargetsRealpathOracleRecorded(t *testing.T) {
	root, outside := targetRealpathFixture(t)
	sep := string(filepath.Separator)
	if err := os.Symlink(filepath.Join("..", "outside", "deep"), filepath.Join(root, "sublink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sublink/../hook.json", filepath.Join(root, "bad.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".."+sep+"outside"+sep+"missing"+sep+".."+sep+"hook.json", filepath.Join(root, "missing-component.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hook.json", filepath.Join(root, "chain-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("chain-1", filepath.Join(root, "chain-2")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		path   string
		want   string
		absent bool
	}{
		{"a_normal_hook_file", filepath.Join(root, "hook.json"), filepath.Join(root, "hook.json"), false},
		{"dotdot_after_an_existing_component", root + sep + "sub" + sep + ".." + sep + "hook.json", filepath.Join(root, "hook.json"), false},
		{"a_link_chain", filepath.Join(root, "chain-2"), filepath.Join(root, "hook.json"), false},
		{"dotdot_inside_a_link_target", filepath.Join(root, "bad.json"), filepath.Join(root, "hook.json"), false},
		{"a_link_leaving_the_root", filepath.Join(root, "out", "hook.json"), filepath.Join(outside, "hook.json"), false},
		{"a_missing_component_below_a_link", filepath.Join(root, "missing-component.json"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := manifestTargetsRealpath(tc.path)
			if tc.absent {
				if err == nil {
					t.Fatalf("manifestTargetsRealpath(%q) = %q; the oracle answers ENOENT", tc.path, got)
				}
				return
			}
			if err != nil || filepath.Clean(got) != tc.want {
				t.Fatalf("manifestTargetsRealpath(%q) = %q, %v; the oracle answers %q", tc.path, got, err, tc.want)
			}
		})
	}
}

// TestManifestTargetsRealpathOracleAgreementKeepsContainment: the walk still answers the escape when a link
// really reaches outside the root through an existing component.
func TestManifestTargetsRealpathOracleAgreementKeepsContainment(t *testing.T) {
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
