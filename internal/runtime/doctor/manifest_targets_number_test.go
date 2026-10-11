package doctor

import (
	"encoding/json"
	"testing"
)

// A number the manifest declares where an array of hook files belongs is spelled as JavaScript's String() spells it. A literal past the double
// range is an infinity there, and the copy of the number formatter this replaced (CRW-451) wrote Go's "+Inf" for it.
func TestTargetStringSpellsANumberAsJavaScriptDoes(t *testing.T) {
	for in, want := range map[string]string{
		"1e999":   "Infinity",
		"-1e999":  "-Infinity",
		"-0":      "0",
		"0":       "0",
		"1.5":     "1.5",
		"1e21":    "1e+21",
		"1e-7":    "1e-7",
		"1234567": "1234567",
	} {
		if got := targetString(json.Number(in)); got != want {
			t.Errorf("targetString(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestManifestTargetsNameAnOverflowingHooksNumberAsInfinity(t *testing.T) {
	root := t.TempDir()
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":1e999}`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hooks must be an array of hook file paths: Infinity"}})
}
