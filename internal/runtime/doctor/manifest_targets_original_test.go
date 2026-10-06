package doctor

// This file is the CRW-652 test for the two doctor manifest-target differences CRW-555 left behind
// (PR #539, found by the operator's pair evaluation of that merged pull request). It is written red
// first against the baseline and green after the fix.
//
// The oracle's validator (plugins/codexclaw/components/cxc-ops/src/manifest-targets.ts, CXC v0.2.40
// commit 3c1459acadeb1906d97c00a598e1457327ae372d) works on JavaScript strings, which keep a lone
// UTF-16 surrogate, and hands such a string to the file system only for the look-up, where Node's
// UTF-8 encoding writes one U+FFFD for that surrogate. So the string a finding reports, and the two
// strings escapesRoot compares, keep the lone surrogate, while existsSync, realpathSync and statSync
// see the U+FFFD spelling. The port converted each lone surrogate to U+FFFD where the manifest string
// was taken up, which changed the verdict whenever U+FFFD names the same file and put U+FFFD into a
// message the oracle keeps whole.
//
// escapesRoot is the subtle one. Node's realpathSync resolves symlinks with a JavaScript walk that
// returns each component as the caller spelled it, so a component spelled with a lone surrogate comes
// back with the lone surrogate even though the system call used the U+FFFD name (realpathSync.native,
// which Go's filepath.EvalSymlinks behaves like, answers the on-disk name instead). The containment
// test then compares two strings that differ exactly when one of them was spelled with a surrogate.
//
// Every expectation below is the oracle's own answer, recorded with Node 24 against that commit's
// dist/manifest-targets.js.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// TestManifestTargetsOriginalEscapeVerdict is the first difference, with the leaf absent: the root
// really holds U+FFFD and the manifest names the same directory with a lone surrogate, so the file
// system resolves both spellings to one directory and only the strings themselves tell them apart.
// The oracle compares those strings and reports an escape; the port compared U+FFFD spellings and
// reported the target missing.
func TestManifestTargetsOriginalEscapeVerdict(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["../plugin-\ud800/missing.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ../plugin-" + targetSurrogateDecode(t, `"\ud800"`) + "/missing.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsOriginalEscapeVerdictWithTheLeafPresent is the same difference where the file the
// manifest names exists under the U+FFFD name, so both realpath calls succeed and the oracle compares
// the two resolved strings. Node's realpathSync keeps the spelling it was given, so they differ and the
// oracle reports an escape from every entry point; a port that answers the on-disk name from both sides
// reports no finding at all.
func TestManifestTargetsOriginalEscapeVerdictWithTheLeafPresent(t *testing.T) {
	hi := targetSurrogateDecode(t, `"\ud800"`)
	t.Run("manifest_hooks_entry", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["../plugin-\ud800/hook.json"]}`)
		targetTestWrite(t, root, "hook.json", `{"hooks":{}}`)
		want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ../plugin-" + hi + "/hook.json"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
	t.Run("manifest_mcp_servers_file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"../plugin-\ud800/mcp.json"}`)
		targetTestWrite(t, root, "mcp.json", `{"mcpServers":{}}`)
		want := []TargetIssue{{TargetMCP, "manifest mcpServers file escapes plugin root: ../plugin-" + hi + "/mcp.json"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
	t.Run("hook_command_target", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`)
		targetTestWrite(t, root, "hooks/a.json", `{"hooks":{"Start":[{"hooks":[{"command":"x ${PLUGIN_ROOT}/../plugin-\ud800/hook.js"}]}]}}`)
		targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":[]}}}`)
		targetTestWrite(t, root, "hook.js", "data")
		want := []TargetIssue{{TargetHook, "target escapes plugin root: ../plugin-" + hi + "/hook.js"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
	t.Run("mcp_js_argument", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`)
		targetTestWrite(t, root, "hooks/a.json", `{"hooks":{}}`)
		targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":["./../plugin-\ud800/arg.js"]}}}`)
		targetTestWrite(t, root, "arg.js", "data")
		// checkTarget reports the argument as it was written, a leading "./" included.
		want := []TargetIssue{{TargetMCP, "target escapes plugin root: ./../plugin-" + hi + "/arg.js"}}
		if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("issues: %+v %v; want %+v", got, err, want)
		}
	})
}

// TestManifestTargetsOriginalEscapeVerdictForCommandTarget is the same difference on the command path
// with the leaf absent: checkTarget resolves the target of a hook command and asks the same containment
// question, so a command target must keep its lone surrogate too.
func TestManifestTargetsOriginalEscapeVerdictForCommandTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`)
	targetTestWrite(t, root, "hooks/a.json", `{"hooks":{"Start":[{"hooks":[{"command":"x ${PLUGIN_ROOT}/../plugin-\ud800/missing.js"}]}]}}`)
	targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":[]}}}`)
	want := []TargetIssue{{TargetHook, "target escapes plugin root: ../plugin-" + targetSurrogateDecode(t, `"\ud800"`) + "/missing.js"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsOriginalMessageKeepsLoneSurrogate is the second difference: the message of a
// manifest hook entry that is not a string. The oracle's String(value) keeps the lone surrogate the
// entry holds, so its JSON report writes the escape and its text report writes U+FFFD (what the UTF-8
// encoder writes for a lone surrogate); the port held U+FFFD in both.
func TestManifestTargetsOriginalMessageKeepsLoneSurrogate(t *testing.T) {
	root := t.TempDir()
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[["a\ud800"]]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file must be a string: a" + targetSurrogateDecode(t, `"\ud800"`)}}
	got, err := ValidateManifestTargets(root)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
	// The message holds the lone surrogate's three WTF-8 bytes, which is the JavaScript string the
	// oracle's JSON.stringify writes as its escape and its UTF-8 encoder writes as U+FFFD.
	if json, text := pyjson.Dumps(got[0].Message, pyjson.Options{Unicode: true}), targetNodeText(got[0].Message); json != `"manifest hook file must be a string: a\ud800"` || text != "manifest hook file must be a string: a\uFFFD" {
		t.Fatalf("JSON %s; text %q", json, text)
	}
}

// TestManifestTargetsRealpathKeepsTheCallersSpelling pins the helper the verdict depends on: the
// components before the first symlink keep the spelling the caller used, and a symlink is replaced by
// its target so a link still cannot smuggle a path outside the root.
func TestManifestTargetsRealpathKeepsTheCallersSpelling(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "plugin-\uFFFD")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "leaf.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(base, "plugin-"+targetSurrogateDecode(t, `"\ud800"`))
	got, err := manifestTargetsRealpath(filepath.Join(spelled, "leaf.json"))
	if err != nil {
		t.Fatalf("realpath: %v", err)
	}
	if want := filepath.Join(spelled, "leaf.json"); got != want {
		t.Fatalf("realpath %q; want the caller's spelling %q", got, want)
	}
	// A symlink is still followed, and the components after it come from the link's target.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "real.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	got, err = manifestTargetsRealpath(filepath.Join(base, "link", "real.json"))
	if err != nil {
		t.Fatalf("realpath through a link: %v", err)
	}
	if want := filepath.Join(outside, "real.json"); got != want {
		t.Fatalf("realpath through a link %q; want %q", got, want)
	}
}
