package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// runCXCRows replays rows against the fake crw as TestCXCReplay_against_a_fake_crw does.
func runCXCRows(t *testing.T, rows []cxcRow) {
	t.Helper()
	r := cxcTestReplayer(t)
	old := syscall.Umask(0o002) // the child runs under umask 022 whatever the test process has
	defer syscall.Umask(old)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			id, claim := row.id, row.claim
			if id == "" {
				id = "cli__x__y"
			}
			if claim.State == "" {
				claim.State = cxcIdentical
			}
			err := r.check(id, cxcFixture(t, row.doc()), claim, t.TempDir)
			switch {
			case row.err == "" && err != nil:
				t.Fatalf("replay failed: %v", err)
			case row.err != "" && (err == nil || !strings.Contains(err.Error(), row.err)):
				t.Fatalf("replay error = %v, want one naming %q", err, row.err)
			}
		})
	}
}

// givenDoc is a given as JSON, built from its fields so no text needs escaping by hand.
func givenDoc(fields map[string]any) string {
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// The given override of a claim: a fixture whose given holds rewrite-rule text (R19) is built with
// the claim's override instead, in crw's names.
func TestCXCReplay_given_override(t *testing.T) {
	r19 := func(extra map[string]string) string {
		files := map[string]string{"ws/.codexclaw-install.json": "{}", "ws/script": "[ ! -f .codex''claw-install.json ] || printf 'oracle record\\n'; [ ! -f .crw-record ] || printf 'crw record\\n'"}
		for k, v := range extra {
			files[k] = v
		}
		return givenDoc(map[string]any{"files": files})
	}
	changed := func(override string) cxcClaim { return cxcClaim{State: cxcChanged, Given: json.RawMessage(override)} }
	const remove = `{"files":{"ws/.codexclaw-install.json":null,"ws/.crw-record":"{}"}}`
	runCXCRows(t, []cxcRow{
		{name: "an override replaces the rewrite-rule file with the one crw expects", given: r19(nil), claim: changed(remove),
			stdout: "a:x\ncwd:${WS}\ncrw record\n"},
		{name: "the same given without an override is refused", given: r19(nil), claim: cxcClaim{State: cxcChanged},
			stdout: "a:x\ncwd:${WS}\ncrw record\n", err: "rewrite rule"},
		{name: "an override that keeps the rewrite-rule text is refused", given: r19(nil), claim: changed(`{"files":{"ws/other":"x"}}`),
			stdout: "a:x\ncwd:${WS}\noracle record\n", err: "rewrite rule"},
		{name: "an override of a given that needs none is applied", given: givenDoc(map[string]any{"files": map[string]string{"ws/script": "[ ! -f f ] || { read -r l < f; printf '%s\\n' \"$l\"; }"}}),
			claim: changed(`{"files":{"ws/f":"patched\n"}}`), stdout: "a:x\ncwd:${WS}\npatched\n"},
		{name: "an unknown field is an error", given: r19(nil), claim: changed(`{"fils":{}}`), err: "unknown field"},
		{name: "an unknown field is an error whatever its value", given: r19(nil), claim: changed(`{"fils":null}`), err: "unknown field"},
		{name: "an override that is no object is an error", given: r19(nil), claim: changed(`[1]`), err: "given override"},
		{name: "a null field removes it", given: givenDoc(map[string]any{"files": map[string]string{"ws/script": "[ ! -d d ] || printf 'd\\n'"}, "dirs": []string{"ws/d"}}),
			claim: changed(`{"dirs":null}`), stdout: "a:x\ncwd:${WS}\n"},
		{name: "an untouched JSON entry keeps its key order and its markup characters", claim: changed(`{"json":{"ws/other.json":{"k":1}}}`),
			given:  `{"files":{"ws/script":"read -r l < state.json; printf '%s\\n' \"$l\""},"json":{"ws/state.json":{"z":1,"a":"<b>&"}}}`,
			stdout: "a:x\ncwd:${WS}\n{\"z\":1,\"a\":\"<b>&\"}\n"},
	})
}

// A status file's "given" reaches the replay: loaded, copied to the claim and applied.
func TestCXCNotes_given_override_end_to_end(t *testing.T) {
	dir := t.TempDir()
	file := `{"issue":"p","intentionally-changed":[{"id":"cli__x__y","reason":"the install record is absorbed","given":{"files":{"ws/.codexclaw-install.json":null,"ws/.crw-record":"{}"}}}]}`
	if err := os.WriteFile(filepath.Join(dir, "p.json"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	claims, err := loadCXCNotes(dir, []string{"cli__x__y"})
	if err != nil {
		t.Fatal(err)
	}
	claim := claims["cli__x__y"]
	if claim.State != cxcChanged || !strings.Contains(string(claim.Given), ".crw-record") {
		t.Fatalf("claim = %+v, want an intentionally-changed claim carrying the given override", claim)
	}
	row := cxcRow{given: givenDoc(map[string]any{"files": map[string]string{
		"ws/.codexclaw-install.json": "{}", "ws/script": "[ ! -f .crw-record ] || printf 'crw record\\n'"}}),
		stdout: "a:x\ncwd:${WS}\ncrw record\n"}
	if err := cxcTestReplayer(t).check("cli__x__y", cxcFixture(t, row.doc()), claim, t.TempDir); err != nil {
		t.Fatal(err)
	}
}

// patchGiven on its own: the top-level shapes and the two kinds of field the replay rows do not reach.
func TestCXCPatchGiven(t *testing.T) {
	base := cxccorpus.Given{Dirs: []string{"ws/a"}, Git: &cxccorpus.Git{Dir: "ws", Commit: "initial"}}
	got, err := patchGiven(base, json.RawMessage(`{"git":{"origin":"https://example.invalid/r.git"},"dirs":["ws/b"]}`))
	if err != nil || got.Git == nil || got.Git.Commit != "initial" || got.Git.Origin != "https://example.invalid/r.git" || !slices.Equal(got.Dirs, []string{"ws/b"}) {
		t.Errorf("git keys are merged and dirs replaced: got %+v %+v, %v", got, got.Git, err)
	}
	for _, bad := range []string{"null", "1", `"x"`} {
		if _, err := patchGiven(base, json.RawMessage(bad)); err == nil || !strings.Contains(err.Error(), "given override") {
			t.Errorf("override %s: error = %v, want a given override error", bad, err)
		}
	}
}
