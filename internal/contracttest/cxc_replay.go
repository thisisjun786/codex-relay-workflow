package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// What a claim can need that the replayer lacks: the SQLite, rewrite-rule and network handling a
// later issue adds (a Go SQLite seed and dump, a given override, a closed-network seam).
const (
	cxcNeedSQLite          = "a SQLite database"
	cxcNeedNetwork         = "the closed network"
	cxcNeedRewriteGiven    = "a rewrite rule in the given"
	cxcNeedRewriteExpected = "a rewrite rule in the expectation"
)

// cxcReplayer replays the CXC corpus against one crw build (contract/schema/cxc/README.md,
// "Replaying against the Go build"); it is the corpus engine's Runtime for that build.
type cxcReplayer struct {
	crw, plugin string // the build under test and its plugin root
	git, exe    string // the real git, and this test binary, which plays every stub and the git wrapper
	sub         *cxccorpus.Substituter
	rules       *cxccorpus.Normaliser
	decls       map[string]cxccorpus.Declaration
	long        []string       // fixtures whose answer depends on the case-root length
	mask        *regexp.Regexp // text no rule may rename: upstream addresses and rewrite-rule text
	rewrite     *regexp.Regexp // text a rewrite-kind rule (R19, R29, R30) names
}

func newCXCReplayer(root, crw string) (*cxcReplayer, error) {
	r := &cxcReplayer{crw: crw, plugin: filepath.Join(root, "plugins", "crw")}
	var err error
	if r.exe, err = os.Executable(); err != nil {
		return nil, err
	}
	if r.git, err = exec.LookPath("git"); err != nil {
		return nil, err
	}
	if r.sub, err = cxccorpus.LoadSubstitution(root); err != nil {
		return nil, err
	}
	rules, err := cxccorpus.LoadRules(root)
	if err != nil {
		return nil, err
	}
	if r.rules, err = rules.Renamed(r.sub); err != nil {
		return nil, err
	}
	if _, r.decls, err = cxccorpus.LoadDeclarations(root); err != nil {
		return nil, err
	}
	coverage, err := cxccorpus.LoadCoverage(root)
	if err != nil {
		return nil, err
	}
	r.long = coverage.PathLengthDependent
	var rewrite, never []string
	for _, rule := range r.sub.File.Rules {
		if rule.Kind == "rewrite" {
			rewrite = append(rewrite, "(?:"+rule.Regex+")")
		}
	}
	for _, entry := range r.sub.File.Never { // prose: the upstream addresses are its literal tokens
		for _, token := range strings.Split(entry.Text, ", ") {
			if strings.Contains(token, "codexclaw") && strings.Contains(token, "/") && !strings.ContainsAny(token, " ()*") {
				never = append(never, regexp.QuoteMeta(token))
			}
		}
	}
	if r.rewrite, err = regexp.Compile(strings.Join(rewrite, "|")); err != nil {
		return nil, err
	}
	r.mask, err = regexp.Compile(strings.Join(append(rewrite, never...), "|"))
	return r, err
}

// rename is the name substitution of the README's steps 2 and 3 for one string: the table's rules,
// except that a case path keeps its root's name (the engine's CXC home root is the directory cxc)
// and text a rewrite rule or the never list names keeps the oracle's spelling (no textual rule
// can replace it; a claim says what changed).
func (r *cxcReplayer) rename(s string) string {
	if first, rest, ok := strings.Cut(s, "/"); ok && slices.Contains(cxccorpus.Roots, first) {
		return first + "/" + r.rename(rest)
	}
	var kept []string
	s = r.mask.ReplaceAllStringFunc(s, func(m string) string {
		kept = append(kept, m)
		return "\x00" + strconv.Itoa(len(kept)-1) + "\x00"
	})
	parts := strings.Split(r.sub.Expected(s), "\x00")
	for i := 1; i < len(parts); i += 2 {
		n, _ := strconv.Atoi(parts[i])
		parts[i] = kept[n]
	}
	return strings.Join(parts, "")
}

// mapStrings rewrites every string of v, map keys included, through its compact JSON form; a
// string f leaves alone keeps its spelling, and documents come back compact.
func mapStrings[T any](v T, f func(string) string) (T, error) {
	var out T
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return out, err
	}
	var failure error
	mapped := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).ReplaceAllStringFunc(encoded.String(), func(literal string) string {
		var s string
		failure = errors.Join(failure, json.Unmarshal([]byte(literal), &s))
		if renamed := f(s); renamed != s {
			raw, _ := json.Marshal(renamed)
			return string(raw)
		}
		return literal
	})
	if failure != nil {
		return out, failure
	}
	return out, json.Unmarshal([]byte(mapped), &out)
}

// scenario is README step 2: the fixture's scenario with names substituted and cli argv mapped to
// crw's. A step of no kind is a cli step without arguments (the empty cli list is not stored); a
// bare root in observe names a whole root and keeps its name.
func (r *cxcReplayer) scenario(id string, fix cxccorpus.Fixture) (cxccorpus.Scenario, error) {
	s := cxccorpus.Scenario{ID: id, Covers: fix.Covers, Note: fix.Note, Given: fix.Given}
	for _, step := range fix.Run.Steps {
		if step.Hook == "" && step.MCP == nil && step.Node == nil && step.Write == nil {
			if argv, ok := r.sub.MapArgv(step.CLI); ok {
				step.CLI = argv
			}
			step.Payload = false
		}
		s.Steps = append(s.Steps, step)
	}
	s, err := mapStrings(s, r.rename)
	for _, root := range fix.Run.Observe {
		if !slices.Contains(cxccorpus.Roots, root) {
			root = r.rename(root)
		}
		s.Observe = append(s.Observe, root)
	}
	return s, err
}

// Setup puts the build under test (bin/crw, a symlink, so the invocation is as long in every
// checkout) and the helper programs in the case: the stubs and the git wrapper are symlinks to
// this test binary (cxcHelper), with the variables that point at them.
func (r *cxcReplayer) Setup(c *cxccorpus.Case, s cxccorpus.Scenario) error {
	link := func(target, dir, name string) error { return os.Symlink(target, filepath.Join(c.Root, dir, name)) }
	if err := errors.Join(link(r.crw, "bin", "crw"), link(r.exe, "bin", "git")); err != nil {
		return err
	}
	rec := filepath.Join(c.Root, ".rec")
	c.Env = append(c.Env, "CRW_BIN="+filepath.Join(c.Root, "bin", "crw"), cxcRecDir+"="+rec, "CXC_REC_LOG="+filepath.Join(rec, "calls.jsonl"), "CXC_REC_GIT="+r.git)
	return cxccorpus.InstallStubs(c, s.Given, func(name string) error { return link(r.exe, "stubs", name) })
}

func (r *cxcReplayer) Bindings(c *cxccorpus.Case) []cxccorpus.Binding {
	return append([]cxccorpus.Binding{{Placeholder: "${PLUGIN_ROOT}", Path: r.plugin}}, c.Bindings()...)
}

// Command maps a step to crw (README step 1): a hook leg to crw hook <event> --leg <leg> with
// PLUGIN_ROOT set, an mcp step to crw bridge in the plugin root with a line per request, a cli step
// to its argv (mapped in scenario). It runs under umask 022, the modes of the recording.
func (r *cxcReplayer) Command(c *cxccorpus.Case, s cxccorpus.Scenario, step cxccorpus.Step) (cxccorpus.Invocation, error) {
	inv := cxccorpus.Invocation{Argv: []string{"/bin/sh", "-c", `umask 022 && exec "$0" "$@"`, "${BIN}/crw"}}
	switch {
	case step.Hook != "":
		decl, ok := r.decls[step.Hook]
		if !ok {
			return inv, fmt.Errorf("hook leg %q is not declared in hook-declarations.json", step.Hook)
		}
		inv.Argv = append(inv.Argv, "hook", cxccorpus.Kebab(decl.Event), "--leg", step.Hook)
		inv.Env = []string{"PLUGIN_ROOT=" + r.plugin}
	case step.MCP != nil:
		inv.Argv, inv.Dir = append(inv.Argv, "bridge"), r.plugin
		for _, request := range step.MCP {
			var line bytes.Buffer
			if err := json.Compact(&line, request); err != nil {
				return inv, err
			}
			inv.Stdin = append(inv.Stdin, append(line.Bytes(), '\n')...)
		}
	case step.Node != nil:
		return inv, errors.New("a node step runs the oracle's own files: it has no crw counterpart")
	default:
		inv.Argv = append(inv.Argv, step.CLI...)
	}
	return inv, nil
}

var errSQLite = errors.New("a SQLite database is not replayable yet: its Go seed and dump belong to a later issue")

func (r *cxcReplayer) SeedSQLite(*cxccorpus.Case, map[string][]string) error { return errSQLite }
func (r *cxcReplayer) DumpSQLite(string) (string, error)                     { return "", errSQLite }
func (r *cxcReplayer) GitPath() string                                       { return r.git }
func (r *cxcReplayer) HookObservations() string                              { return r.rename(cxccorpus.HookObservations) }

// needs lists what a claim on the fixture needs that the replayer lacks. An identical claim also
// cannot cover a rewrite rule's text in the expectation, which keeps the oracle's spelling: only
// a claim that sets or removes that key can pass.
func (r *cxcReplayer) needs(fix cxccorpus.Fixture, identical bool) (out []string) {
	given, _ := json.Marshal(fix.Given)
	expected, _ := json.Marshal(fix.Expect)
	add := func(hit bool, need string) {
		if hit {
			out = append(out, need)
		}
	}
	add(len(fix.Given.SQLite) > 0 || bytes.Contains(expected, []byte(`"form":"sqlite"`)) || bytes.Contains(expected, []byte(`"form":"sqlite-text"`)), cxcNeedSQLite)
	add(len(fix.Given.Fetch) > 0 || slices.ContainsFunc(fix.Expect.Calls, func(c cxccorpus.Call) bool {
		return slices.Contains([]string{"fetch", "connect", "dns"}, c.Cmd)
	}), cxcNeedNetwork)
	add(r.rewrite.Match(given), cxcNeedRewriteGiven)
	add(identical && r.rewrite.Match(expected), cxcNeedRewriteExpected)
	return out
}

// check decides one fixture by its claim: unregistered fails, pending is not run, any other claim
// is replayed against the build and must match the expectation (README steps 2 to 4).
func (r *cxcReplayer) check(id string, fix cxccorpus.Fixture, claim cxcClaim, tmp func() string) error {
	switch claim.State {
	case "":
		return fmt.Errorf("%s is registered in no status file under contract/notes/cxc: the issue that ports it claims it in a file of its own", id)
	case cxcPending:
		return nil
	}
	if needs := r.needs(fix, claim.State == cxcIdentical); len(needs) > 0 {
		return fmt.Errorf("%s: the %s claim by %s needs %s, which belongs to the later issue for the SQLite, rewrite-rule and network handling of this replayer", id, claim.State, claim.Issue, strings.Join(needs, " and "))
	}
	scenario, err := r.scenario(id, fix)
	if err != nil {
		return err
	}
	scratch := "/var/tmp" // a case root /var/tmp/cxc-rec-<16 hex> is the recorded 33 bytes
	if !slices.Contains(r.long, id) {
		scratch = tmp()
	}
	got, err := cxccorpus.RunScenario(r, cxccorpus.RunOptions{Scratch: scratch, HomeVar: "CRW_HOME", Rules: r.rules}, scenario)
	if err != nil {
		return err
	}
	want, err := mapStrings(fix.Expect, r.rename)
	if err != nil {
		return err
	}
	invocation := strings.NewReplacer(`"${BIN}/crw"`, "{CRW}", "${BIN}/crw", "{CRW}")
	if got, err = mapStrings(got, invocation.Replace); err != nil {
		return err
	}
	if err := compare(flatten(want), flatten(got), claim); err != nil {
		return fmt.Errorf("%s (%s by %s): %w", id, claim.State, claim.Issue, err)
	}
	return nil
}

// flatten is the comparison model, every observation a key and a text, in crw's names, which is
// how a claim addresses them. Both sides come through mapStrings, so documents are compact: the
// form (pretty or compact, final newline) and the text together are the exact bytes.
func flatten(e cxccorpus.Expect) map[string]string {
	flat := map[string]string{"exit": strconv.Itoa(e.Exit)}
	set := func(prefix string, fields ...string) { // name, value pairs
		for i := 0; i < len(fields); i += 2 {
			flat[prefix+fields[i]] = fields[i+1]
		}
	}
	text := func(doc json.RawMessage, lines []json.RawMessage, plain *string) (out string) {
		if plain != nil {
			out = *plain
		}
		out += string(doc)
		for _, line := range lines {
			out += string(line) + "\n"
		}
		return out
	}
	for i, s := range e.Steps {
		set(fmt.Sprintf("steps/%d/", i), "action", s.Action, "exit", strconv.Itoa(s.Exit), "signal", s.Signal, "timeout", strconv.FormatBool(s.Timeout),
			"form", s.StdoutForm, "stdout", text(s.StdoutJSON, s.StdoutJSONL, s.Stdout), "stderr", s.Stderr)
	}
	for path, entry := range e.Tree {
		size := ""
		if entry.Size != nil {
			size = strconv.FormatInt(*entry.Size, 10)
		}
		set("tree/"+path+"/", "type", entry.Type, "mode", entry.Mode, "target", entry.Target, "form", entry.Form, "sha256", entry.SHA256, "size", size,
			"content", text(entry.JSON, entry.JSONL, entry.Text))
	}
	for i, call := range e.Calls {
		raw, _ := json.Marshal(call)
		flat[fmt.Sprintf("calls/%d", i)] = string(raw)
	}
	return flat
}

// compare applies the claim's patch to the expectation and lists up to eight differences.
func compare(want, got map[string]string, claim cxcClaim) error {
	for _, prefix := range claim.Remove {
		before := len(want)
		maps.DeleteFunc(want, func(key, _ string) bool { return strings.HasPrefix(key, prefix) })
		if len(want) == before {
			return fmt.Errorf("remove %q matches no expected key", prefix)
		}
	}
	maps.Copy(want, claim.Set)
	keys := append(slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got))...)
	slices.Sort(keys)
	var diffs []string
	for _, key := range slices.Compact(keys) {
		if want[key] != got[key] {
			diffs = append(diffs, fmt.Sprintf("  %s: want %.300q, got %.300q", key, want[key], got[key]))
		}
	}
	if n := len(diffs); n > 8 {
		diffs = append(diffs[:8], fmt.Sprintf("  ... and %d more", n-8))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("crw differs from the expectation:\n%s", strings.Join(diffs, "\n"))
	}
	return nil
}

func loadCXCFixtures(root string) ([]string, map[string]cxccorpus.Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(root, cxccorpus.FixtureDir, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	var ids []string
	fixtures := map[string]cxccorpus.Fixture{}
	for _, path := range paths {
		fix, err := cxccorpus.LoadFixture(path)
		if err != nil {
			return nil, nil, err
		}
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		ids, fixtures[id] = append(ids, id), fix
	}
	return ids, fixtures, nil
}

// replayCXC is TestDomain's cxc domain: a subtest per fixture, decided by the status files.
func replayCXC(t *testing.T, root, crw string) {
	r, err := newCXCReplayer(root, crw)
	if err != nil {
		t.Fatal(err)
	}
	ids, fixtures, err := loadCXCFixtures(root)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := loadCXCNotes(filepath.Join(root, "contract", "notes", "cxc"), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if err := r.check(id, fixtures[id], claims[id], t.TempDir); err != nil {
				t.Fatal(err)
			}
		})
	}
}
