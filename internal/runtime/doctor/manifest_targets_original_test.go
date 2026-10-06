package doctor

// This file is the CRW-652 test for the two doctor manifest-target differences CRW-555 left behind
// (PR #539, found by the operator's pair evaluation of that merged pull request). It is written red
// first against the baseline and green after the fix.
//
// The oracle's validator (plugins/codexclaw/components/cxc-ops/src/manifest-targets.ts, CXC v0.2.40
// commit 3c1459acadeb1906d97c00a598e1457327ae372d) works on JavaScript strings, which keep a lone
// UTF-16 surrogate, and hands such a string to the file system only for the look-up, where Node's
// UTF-8 encoding writes one U+FFFD for that surrogate. So the string a finding reports, and the
// string escapesRoot compares when its two realpath calls fail, keep the lone surrogate, while
// existsSync, realpathSync and statSync see the U+FFFD spelling. The port converted each lone
// surrogate to U+FFFD where the manifest string was taken up, which changed the verdict whenever
// U+FFFD names the same file and put U+FFFD into a message the oracle keeps whole.
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

// TestManifestTargetsOriginalEscapeVerdict is the first difference: the containment test of a
// manifest hook file. The root really holds U+FFFD and the manifest names the same directory with a
// lone surrogate, so the file system resolves both spellings to one directory and only the strings
// themselves tell them apart. The oracle compares those strings and reports an escape; the port
// compared U+FFFD spellings and reported the target missing.
func TestManifestTargetsOriginalEscapeVerdict(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
	if err := os.MkdirAll(filepath.Join(root, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["../plugin-\ud800/missing.json"]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ../plugin-" + targetSurrogateDecode(t, `"\ud800"`) + "/missing.json"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsOriginalEscapeVerdictForCommandTarget is the same difference on the command
// path: checkTarget resolves the target of a hook command and asks the same containment question,
// so a command target must keep its lone surrogate too.
func TestManifestTargetsOriginalEscapeVerdictForCommandTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin-\uFFFD")
	command := `"x ${PLUGIN_ROOT}/../plugin-\ud800/missing.js"`
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`)
	targetTestWrite(t, root, "hooks/a.json", `{"hooks":{"Start":[{"hooks":[{"command":`+command+`}]}]}}`)
	targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":[]}}}`)
	want := []TargetIssue{{TargetHook, "target escapes plugin root: ../plugin-" + targetSurrogateDecode(t, `"\ud800"`) + "/missing.js"}}
	if got, err := ValidateManifestTargets(root); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

// TestManifestTargetsOriginalMessageKeepsLoneSurrogate is the second difference: the message of a
// manifest hook entry that is not a string. The oracle's String(value) keeps the lone surrogate the
// entry holds, so its JSON report writes the escape \ud800 and its text report writes U+FFFD (what
// the UTF-8 encoder writes for a lone surrogate); the port held U+FFFD in both.
func TestManifestTargetsOriginalMessageKeepsLoneSurrogate(t *testing.T) {
	root := t.TempDir()
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[["a\ud800"]]}`)
	want := []TargetIssue{{TargetHook, "manifest hook file must be a string: a" + targetSurrogateDecode(t, `"\ud800"`)}}
	got, err := ValidateManifestTargets(root)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
	// The three WTF-8 bytes the message holds are the lone surrogate the oracle's JavaScript string
	// holds, so the report's JSON writer spells it as the escape JSON.stringify writes (the doctor
	// harness report's own writer uses this spelling for its strings).
	const spelling = `"manifest hook file must be a string: a\ud800"`
	if json := pyjson.Dumps(got[0].Message, pyjson.Options{Unicode: true, Bytes: pyjson.ReplacedBytes}); json != spelling {
		t.Fatalf("JSON spelling %s; want %s", json, spelling)
	}
}
