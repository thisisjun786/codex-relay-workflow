package contracttest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// fakeCRW stands in for the build under test: it prints what it was given (argv one word per line,
// its directory, PLUGIN_ROOT, stdin), then runs the script file the scenario's given put in the
// workspace. Builtins only: a case's PATH holds just its stub and bin directories.
const fakeCRW = `#!/bin/sh
for a in "$@"; do printf 'a:%s\n' "$a"; done
printf 'cwd:%s\n' "$PWD"
[ -z "$PLUGIN_ROOT" ] || printf 'plugin:%s\n' "$PLUGIN_ROOT"
while IFS= read -r l || [ -n "$l" ]; do printf 'in:%s\n' "$l"; done
[ ! -f script ] || . ./script
exit 0
`

func cxcTestReplayer(t *testing.T) *cxcReplayer {
	t.Helper()
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	crw := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(crw, []byte(fakeCRW), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := newCXCReplayer(root, crw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cxcFixture(t *testing.T, doc string) cxccorpus.Fixture {
	t.Helper()
	var f cxccorpus.Fixture
	if err := json.Unmarshal([]byte(doc), &f); err != nil {
		t.Fatalf("%v in %s", err, doc)
	}
	return f
}

// cxcRow is a fixture whose recorded answer is one text step, unless res says otherwise; the
// fields are JSON (given, steps, observe, tree, calls) and carry defaults.
type cxcRow struct {
	name, id, given, steps, observe, stdout, stderr, res, tree, calls string
	claim                                                             cxcClaim
	err                                                               string // the replay fails naming this; empty: it passes
}

func (w cxcRow) doc() string {
	or := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	res := w.res
	if res == "" {
		res = fmt.Sprintf(`[{"exit":0,"stdout_form":"text","stdout":%q,"stderr":%q}]`, or(w.stdout, "a:x\ncwd:${WS}\n"), w.stderr)
	}
	return fmt.Sprintf(`{"given":%s,"run":{"kind":"cxc","steps":%s,"observe":%s},"expect":{"exit":0,"steps":%s,"tree":%s,"calls":%s}}`,
		or(w.given, "{}"), or(w.steps, `[{"cli":["x"]}]`), or(w.observe, `["tmp"]`), res, or(w.tree, "{}"), or(w.calls, "[]"))
}

func script(text string) string { return `{"files":{"ws/script":"` + text + `"}}` }

var cxcRows = []cxcRow{
	{name: "cli prefix is mapped through the name table", steps: `[{"cli":["orchestrate","status"]}]`, stdout: "a:pabcd\na:orchestrate\na:status\ncwd:${WS}\n"},
	{name: "a dropped verb passes through", steps: `[{"cli":["-v"]}]`, stdout: "a:-v\ncwd:${WS}\n"},
	{name: "an empty step is a cli step without arguments", steps: "[{}]", stdout: "cwd:${WS}\n"},
	{name: "a hook leg runs crw hook event --leg", steps: `[{"hook":"session-start-bootstrapping-pabcd-state","stdin":{"session_id":"s"}}]`,
		stdout: "a:hook\na:session-start\na:--leg\na:session-start-bootstrapping-pabcd-state\ncwd:${WS}\nplugin:${PLUGIN_ROOT}\nin:{\"session_id\":\"s\"}\n"},
	{name: "an mcp step writes a line per request in the plugin root", steps: `[{"mcp":[{"jsonrpc":"2.0","id":1},{"id":2}]}]`,
		stdout: "a:bridge\ncwd:${PLUGIN_ROOT}\nin:{\"jsonrpc\":\"2.0\",\"id\":1}\nin:{\"id\":2}\n"},
	{name: "a write step lands between invocations", steps: `[{"write":{"ws/script":"printf 'written\\n'"}},{"cli":["x"]}]`,
		res: `[{"action":"write","exit":0,"stdout_form":"empty","stderr":""},{"exit":0,"stdout_form":"text","stdout":"a:x\ncwd:${WS}\nwritten\n","stderr":""}]`},
	{name: "names are substituted in the given and the expectation", given: `{"files":{"ws/codexclaw.json":"{}","ws/script":"[ ! -f crw.json ] || printf 'found crw.json\\n'"}}`,
		stdout: "a:x\ncwd:${WS}\nfound codexclaw.json\n"},
	{name: "the resolved invocation and the verb after it follow the table", given: script(`printf 'run \"%s\" pabcd orchestrate P\\n' \"$CRW_BIN\"`),
		stdout: "a:x\ncwd:${WS}\nrun node \"${PLUGIN_ROOT}/bin/cxc.mjs\" orchestrate P\n"},
	{name: "the invocation is the placeholder quoted or not", given: script(`printf 'run %s pabcd orchestrate P\\n' \"$CRW_BIN\"`),
		stdout: "a:x\ncwd:${WS}\nrun node \"${PLUGIN_ROOT}/bin/cxc.mjs\" orchestrate P\n"},
	{name: "the cxc home root keeps its name", given: `{"files":{"cxc/x.json":"1"}}`, observe: `["cxc"]`,
		tree: `{"cxc/x.json":{"type":"file","mode":"0644","form":"json","json":1}}`},
	{name: "a stub named cxc becomes a stub named crw", given: `{"stubs":{"cxc":{"stdout":"hi\n","exit":0}},"files":{"ws/script":"cxc"}}`,
		stdout: "a:x\ncwd:${WS}\nhi\n", calls: `[{"cmd":"cxc","argv":[],"cwd":"${WS}"}]`},
	{name: "an upstream address survives the rename", given: script(`printf 'see github.com/lidge-jun/codex%s\\n' claw`),
		stdout: "a:x\ncwd:${WS}\nsee github.com/lidge-jun/codexclaw\n"},
	{name: "a pretty JSON file, a mode and the umask", given: script(`printf '{\\n  \"a\": 1\\n}\\n' > \"$TMPDIR/p.json\"; umask > \"$TMPDIR/umask\"`),
		tree: `{"tmp/p.json":{"type":"file","mode":"0644","form":"json-pretty","json":{"a":1}},"tmp/umask":{"type":"file","mode":"0644","form":"text","text":"0022\n"}}`},
	{name: "text with markup characters survives the rename in a JSON file", given: script(`printf '{\"u\":\"crw relay job get <id>\"}\\n' > \"$TMPDIR/u.json\"`),
		tree: `{"tmp/u.json":{"type":"file","mode":"0644","form":"json-line","json":{"u":"cxc bg get <id>"}}}`},
	{name: "compact against pretty is a difference", given: script(`printf '{\\n  \"a\": 1\\n}\\n' > \"$TMPDIR/p.json\"`),
		tree: `{"tmp/p.json":{"type":"file","mode":"0644","form":"json-line","json":{"a":1}}}`, err: "tree/tmp/p.json/form"},
	{name: "a path-length fixture runs in a 33-byte case root", id: "cli__chat__index_status", given: script(`printf '%s\\n' \"${#CRW_HOME}\"`),
		stdout: "a:x\ncwd:${WS}\n37\n"},
	{name: "another fixture does not", given: script(`printf '%s\\n' \"${#CRW_HOME}\"`), stdout: "a:x\ncwd:${WS}\n37\n", err: "steps/0/stdout"},
	{name: "stubs answer by prefix, by default, unscripted or not at all; git is logged",
		given:  `{"stubs":{"codex":{"stdout":"x\n","exit":0,"cases":[{"argv":["features","list"],"stdout":"listing\n","exit":0}]},"gh":null},"files":{"ws/script":"codex features list; codex other; ocx; command -v gh >/dev/null || printf 'nogh\\n'; git --version >/dev/null"}}`,
		stdout: "a:x\ncwd:${WS}\nlisting\nx\nnogh\n", stderr: "stub ocx: not scripted\n",
		calls: `[{"cmd":"codex","argv":["features","list"],"cwd":"${WS}"},{"cmd":"codex","argv":["other"],"cwd":"${WS}"},{"cmd":"ocx","argv":[],"cwd":"${WS}"},{"cmd":"git","argv":["--version"],"cwd":"${WS}"}]`},
	{name: "a different stdout is reported by key", stdout: "a:y\ncwd:${WS}\n", err: "steps/0/stdout"},
	// An identical claim on text a rewrite rule changes with no textual replacement is refused; a
	// changed claim runs against the oracle's own spelling, so a port printing the new name must
	// say so with a set patch.
	{name: "an identical claim on rewrite-rule text is refused", given: script(`printf '.codex%s-install.json\\n' claw`), stdout: "a:x\ncwd:${WS}\n.codexclaw-install.json\n", err: "rewrite rule"},
	{name: "a changed claim keeps the oracle spelling", given: script(`printf '.codex%s-install.json\\n' claw`), stdout: "a:x\ncwd:${WS}\n.codexclaw-install.json\n", claim: cxcClaim{State: cxcChanged}},
	{name: "a port printing the new name fails the unpatched claim", given: script(`printf '.crw-install.json\\n'`), stdout: "a:x\ncwd:${WS}\n.codexclaw-install.json\n", claim: cxcClaim{State: cxcChanged}, err: "steps/0/stdout"},
	{name: "set replaces an expected value", given: script(`printf 'new\\n'`), stdout: "a:x\ncwd:${WS}\nold\n",
		claim: cxcClaim{State: cxcChanged, Set: map[string]string{"steps/0/stdout": "a:x\ncwd:${WS}\nnew\n"}}},
	{name: "remove drops an expected key the port does not produce", tree: `{"tmp/gone":{"type":"file","mode":"0644","form":"text","text":"x"}}`,
		claim: cxcClaim{State: cxcChanged, Remove: []string{"tree/tmp/gone"}}},
	{name: "a set value is not a stand-in for a missing key", claim: cxcClaim{State: cxcChanged, Set: map[string]string{"tree/tmp/new/content": ""}}, err: "<absent>"},
	{name: "remove must match a key", claim: cxcClaim{State: cxcChanged, Remove: []string{"tree/none"}}, err: "matches no"},
}

func TestCXCReplay_against_a_fake_crw(t *testing.T) {
	r := cxcTestReplayer(t)
	// The child runs under umask 022 whatever the test process has.
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	for _, row := range cxcRows {
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

// Pending is not run, an unregistered fixture fails, and a claim that needs what the replayer does
// not have yet is refused before anything runs, naming the work.
func TestCXCReplay_states_and_refusals(t *testing.T) {
	r := cxcTestReplayer(t)
	bare := func(given, expect string) cxccorpus.Fixture {
		if expect == "" {
			expect = `{"exit":0,"steps":[],"tree":{},"calls":[]}`
		}
		return cxcFixture(t, fmt.Sprintf(`{"given":%s,"run":{"kind":"cxc","steps":[]},"expect":%s}`, given, expect))
	}
	if err := r.check("a__b__c", bare("{}", ""), cxcClaim{State: cxcPending}, t.TempDir); err != nil {
		t.Errorf("pending: %v", err)
	}
	if err := r.check("a__b__c", bare("{}", ""), cxcClaim{}, t.TempDir); err == nil || !strings.Contains(err.Error(), "no status file") {
		t.Errorf("unregistered: %v", err)
	}
	calls := func(c string) string {
		return `{"exit":0,"steps":[],"tree":{},"calls":[{"cmd":"` + c + `","argv":[],"cwd":"${WS}"}]}`
	}
	out := func(s string) string {
		return `{"exit":0,"steps":[{"exit":0,"stdout_form":"text","stdout":"` + s + `","stderr":""}],"tree":{},"calls":[]}`
	}
	for _, tc := range []struct {
		name  string
		fix   cxccorpus.Fixture
		state string
		want  string
	}{
		{"sqlite given", bare(`{"sqlite":{"codex/x.sqlite":["CREATE TABLE t(a)"]}}`, ""), cxcChanged, "SQLite"},
		{"sqlite tree", bare("{}", `{"exit":0,"steps":[],"tree":{"codex/y.sqlite":{"type":"file","mode":"0644","form":"sqlite","json":{"tables":[]}}},"calls":[]}`), cxcIdentical, "SQLite"},
		{"fetch given", bare(`{"fetch":{"https://example.invalid/":{"status":200,"body":"{}"}}}`, ""), cxcIdentical, "network"},
		{"fetch call", bare("{}", calls("fetch")), cxcIdentical, "network"},
		{"connect call", bare("{}", calls("connect")), cxcChanged, "network"},
		{"dns call", bare("{}", calls("dns")), cxcIdentical, "network"},
		{"R19 given", bare(`{"files":{"ws/.codexclaw-install.json":"{}"}}`, ""), cxcChanged, "rewrite rule"},
		{"R29 expected", bare("{}", out("cxc-ops error")), cxcIdentical, "rewrite rule"},
		{"R30 expected", bare("{}", out("codexclaw-subagent-config")), cxcIdentical, "rewrite rule"},
	} {
		err := r.check("a__b__c", tc.fix, cxcClaim{State: tc.state}, t.TempDir)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "later issue") {
			t.Errorf("%s: error = %v, want a refusal naming %q and the later issue", tc.name, err, tc.want)
		}
	}
}

// The classes the follow-up owns, counted over the real corpus.
func TestCXCCorpus_refused_classes(t *testing.T) {
	root, _ := Root()
	_, fixtures, err := loadCXCFixtures(root)
	if err != nil {
		t.Fatal(err)
	}
	r, got := cxcTestReplayer(t), map[string]int{}
	for _, f := range fixtures {
		for _, need := range r.needs(f, true) {
			got[need]++
		}
	}
	want := map[string]int{cxcNeedSQLite: 39, cxcNeedNetwork: 10, cxcNeedRewriteGiven: 4, cxcNeedRewriteExpected: 33}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refused classes over %d fixtures = %v, want %v", len(fixtures), got, want)
	}
}

func TestCXCNotes(t *testing.T) {
	ids := []string{"a", "b", "c"}
	pending := `{"issue":"pending","pending":["a","b","c"]}`
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  map[string]string // id -> "state file"; "" is unregistered
		err   string
	}{
		{"all pending", map[string]string{"pending.json": pending}, map[string]string{"a": "pending pending", "c": "pending pending"}, ""},
		{"an issue file beats the shared one, whatever the file order",
			map[string]string{"pending.json": pending, "0.json": `{"issue":"0","identical":["a"],"intentionally-changed":[{"id":"b","reason":"r","set":{"k":"v"},"remove":["p"]}]}`},
			map[string]string{"a": "identical 0", "b": "intentionally-changed 0 map[k:v] [p]", "c": "pending pending"}, ""},
		{"an unregistered fixture has no claim", map[string]string{"x.json": `{"issue":"x","pending":["a","b"]}`}, map[string]string{"c": ""}, ""},
		{"two files claiming one fixture", map[string]string{"p.json": `{"issue":"p","identical":["a"]}`, "q.json": `{"issue":"q","identical":["a"]}`}, nil, "claimed by"},
		{"one file claiming it twice", map[string]string{"p.json": `{"issue":"p","identical":["a"],"intentionally-changed":[{"id":"a","reason":"r"}]}`}, nil, "claimed by"},
		{"a changed claim needs a reason", map[string]string{"p.json": `{"issue":"p","intentionally-changed":[{"id":"a","reason":" "}]}`}, nil, "reason"},
		{"the issue is the file name", map[string]string{"p.json": `{"issue":"q","pending":["a"]}`}, nil, "file name"},
		{"a claim names a fixture", map[string]string{"p.json": `{"issue":"p","identical":["zzz"]}`}, nil, "not a fixture"},
		{"a pending list names a fixture", map[string]string{"p.json": `{"issue":"p","pending":["zzz"]}`}, nil, "not a fixture"},
		{"a second document is refused", map[string]string{"p.json": `{"issue":"p","pending":["a"]} {"identical":["a"]}`}, nil, "after the JSON value"},
		{"an unknown field is refused", map[string]string{"p.json": `{"issue":"p","identica":["a"]}`}, nil, "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadCXCNotes(dir, ids)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want one naming %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for id, want := range tc.want {
				claim := got[id]
				have := strings.TrimSpace(claim.State + " " + claim.Issue)
				if len(claim.Set)+len(claim.Remove) > 0 {
					have += fmt.Sprint(" ", claim.Set, " ", claim.Remove)
				}
				if have != want {
					t.Errorf("fixture %s is %q, want %q", id, have, want)
				}
			}
		})
	}
}
