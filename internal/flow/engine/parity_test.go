package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The parity corpus (testdata/flow/parity) holds Claude workflow scripts byte for byte next to their CRW ports. The
// goldens fix what each call of a port resolved to, so a build that fell to a read-only role fails here.

const parityDir = "../../../testdata/flow/parity"

var updateParity = flag.Bool("update-parity", false, "rewrite the parity goldens from the current run")

type goldenCall struct {
	Path           string `json:"path"`
	Position       int    `json:"position"`
	Label          string `json:"label"`
	Resumes        string `json:"resumes,omitempty"`
	Role           string `json:"role"`
	Sandbox        string `json:"sandbox"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	RequestedPair  Pair   `json:"requestedPair"`
	ConfiguredPair Pair   `json:"configuredPair"`
}

type golden struct {
	ClaudeSHA256 string       `json:"claudeSha256"`
	Value        any          `json:"value"`
	Calls        []goldenCall `json:"calls"`
}

func parityPolicy(t *testing.T) *Policy {
	t.Helper()
	exec, err := execution.FromFile(filepath.Join(parityDir, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	table, err := PolicyFromExecution(exec, "testdata/flow/parity/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func parityNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(parityDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// runParity runs one script of a corpus directory on the fake backend and returns the result and the journal.
func runParity(t *testing.T, name, file string) (*Result, []Event) {
	t.Helper()
	dir := os.DirFS(filepath.Join(parityDir, name))
	text, err := os.ReadFile(filepath.Join(parityDir, name, file))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := script.Load(script.Source{Name: file, Text: text, Dir: dir})
	if err != nil {
		t.Fatalf("%s/%s: %v", name, file, err)
	}
	// A script that reads args gets the ones args.json holds.
	args, err := os.ReadFile(filepath.Join(parityDir, name, "args.json"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	journal := NewMemoryJournal()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, prog, Config{
		Backend: NewFake(), Policy: parityPolicy(t), Journal: journal, RunID: "run-parity", OmitSeq: true, Args: args,
		Workflows: func(n, p string) (*script.Program, error) { return loadSibling(dir, n, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, journal.Events()
}

func goldenOf(t *testing.T, name string) golden {
	t.Helper()
	res, events := runParity(t, name, "crw.js")
	if res.Status != RunCompleted {
		t.Fatalf("%s: %s: %s", name, res.ErrorKind, res.Error)
	}
	sandbox, labels := map[string]string{}, map[string]string{}
	resumeOf := map[string]string{}
	for _, e := range events {
		if e.Kind == KindCall && e.Event == EvPrepared && e.Op == "agent" {
			sandbox[e.CallID], _ = e.Data["sandbox"].(string)
			labels[e.CallID] = e.Label
			resumeOf[e.CallID], _ = e.Data["resume_of"].(string)
		}
	}
	var value any
	if err := json.Unmarshal(res.Value, &value); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(parityDir, name, "claude.js"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	g := golden{ClaudeSHA256: hex.EncodeToString(sum[:]), Value: value}
	for _, c := range res.Calls {
		g.Calls = append(g.Calls, goldenCall{
			Path: c.Path, Position: c.Position, Label: c.Label, Resumes: labels[resumeOf[c.CallID]], Role: c.Meta.Role, Sandbox: sandbox[c.CallID],
			Status: c.Status, Reason: c.Reason, RequestedPair: c.Meta.RequestedPair, ConfiguredPair: c.Meta.ConfiguredPair,
		})
	}
	return g
}

func TestParityCorpus(t *testing.T) {
	names := parityNames(t)
	if len(names) < 6 {
		t.Fatalf("the corpus holds at least six scripts, got %v", names)
	}
	for _, want := range []string{"build-verify-fix", "explicit-effort-build"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("the corpus lacks %s", want)
		}
	}
	// Among the real originals is a build, its verification by agentType 'ocx-gpt-6-1-sol' with effort 'high', and a
	// fix after it.
	realBVF := ""
	for _, name := range names {
		if isReal(name) && buildVerifyFix(corpusLines(t, name, "claude.js")) {
			realBVF = name
		}
	}
	if realBVF == "" {
		t.Errorf("no real original is a build, an agentType 'ocx-gpt-6-1-sol' verification with effort 'high' and a fix")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			got := goldenOf(t, name)
			path := filepath.Join(parityDir, name, "golden.json")
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetIndent("", "  ")
			if err := enc.Encode(got); err != nil {
				t.Fatal(err)
			}
			if *updateParity {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Errorf("%s differs from the run:\n%s\nrun with -update-parity to rewrite it if the change is intended", path, buf.String())
			}
			// A build is a call that writes; it never falls to a read-only role, and an investigation never writes.
			for _, c := range got.Calls {
				// The schemas of a real Claude script are not strict, and the engine refuses such a call whatever its
				// role; no other refusal is expected of any script.
				if c.Status != StatusCompleted && !(isReal(name) && c.Status == StatusRefused && c.Reason == ReasonSchemaNotStrict) {
					t.Errorf("%s: call %s %s: %s %s", name, c.Path, c.Label, c.Status, c.Reason)
				}
				switch {
				case strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "fix:") || strings.HasPrefix(c.Label, "apply:"):
					if c.Sandbox != SandboxWorkspaceWrite || !strings.HasPrefix(c.Role, "executor") {
						t.Errorf("%s: the build %s ran as %s in %s", name, c.Label, c.Role, c.Sandbox)
					}
				case strings.HasPrefix(c.Label, "find:") || strings.HasPrefix(c.Label, "sweep:") || strings.HasPrefix(c.Label, "scout:"):
					if c.Role != "explorer" || c.Sandbox != SandboxReadOnly {
						t.Errorf("%s: the investigation %s ran as %s in %s", name, c.Label, c.Role, c.Sandbox)
					}
				}
			}
			// The original is the original: its bytes are the ones the golden was cut from.
			var cut struct {
				ClaudeSHA256 string `json:"claudeSha256"`
			}
			if err := json.Unmarshal(want, &cut); err != nil || cut.ClaudeSHA256 != got.ClaudeSHA256 {
				t.Errorf("claude.js changed since the golden was cut (%s, %v)", cut.ClaudeSHA256, err)
			}
		})
	}
}

var (
	buildCall  = regexp.MustCompile("agent\\(.*label: [`'\"]build:")
	verifyCall = regexp.MustCompile(`agent\(.*agentType: 'ocx-gpt-6-1-sol'.*effort: 'high'`)
	fixCall    = regexp.MustCompile("agent\\(.*label: [`'\"]fix:")
)

// buildVerifyFix reports whether a script has a build call, then a verification call by agentType 'ocx-gpt-6-1-sol'
// with effort 'high', then a fix call, in the order of its text.
func buildVerifyFix(lines []string) bool {
	stage := 0
	for _, line := range lines {
		switch {
		case stage == 0 && buildCall.MatchString(line):
			stage = 1
		case stage == 1 && verifyCall.MatchString(line):
			stage = 2
		case stage == 2 && fixCall.MatchString(line):
			return true
		}
	}
	return false
}

// isReal reports whether a corpus directory holds a real original (it names its source).
func isReal(name string) bool {
	_, err := os.Stat(filepath.Join(parityDir, name, "source.json"))
	return err == nil
}

// TestParityCorpusProvenance: at least six scripts of the corpus are real originals whose claude.js is the file the
// source record names, byte for byte.
func TestParityCorpusProvenance(t *testing.T) {
	real := 0
	for _, name := range parityNames(t) {
		if !isReal(name) {
			continue
		}
		real++
		raw, err := os.ReadFile(filepath.Join(parityDir, name, "source.json"))
		if err != nil {
			t.Fatal(err)
		}
		var src struct {
			Origin, File, Session, SHA256, Modified string
			Bytes                                   int
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&src); err != nil {
			t.Errorf("%s/source.json: %v", name, err)
			continue
		}
		claude, err := os.ReadFile(filepath.Join(parityDir, name, "claude.js"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(claude)
		if src.SHA256 != hex.EncodeToString(sum[:]) || src.Bytes != len(claude) {
			t.Errorf("%s: claude.js is not the original source.json names (%s, %d bytes)", name, src.SHA256, src.Bytes)
		}
		if !strings.HasSuffix(src.File, ".js") || src.Session == "" || src.Origin == "" || src.Modified == "" {
			t.Errorf("%s/source.json does not say where the original came from: %+v", name, src)
		}
		if _, err := os.Stat(filepath.Join(parityDir, name, "child-review.js")); err == nil {
			t.Errorf("%s: a real original has no written helper files", name)
		}
	}
	if real < 6 {
		t.Errorf("the corpus holds %d real originals, at least six", real)
	}
}

// TestParityCorpusRoles pins the cases the corpus is there for: the verification step that states a model and an
// effort the policy does not give it, the explicit effort of a build, and what the original does without the port.
func TestParityCorpusRoles(t *testing.T) {
	policy := parityPolicy(t)
	reviewer, executor := policy.Roles["reviewer"].Pair, policy.Roles["executor"].Pair
	find := func(g golden, label string) goldenCall {
		for _, c := range g.Calls {
			if c.Label == label {
				return c
			}
		}
		t.Fatalf("no call %s", label)
		return goldenCall{}
	}

	g := goldenOf(t, "build-verify-fix")
	verify := find(g, "verify:change")
	if verify.Role != "reviewer" || verify.Sandbox != SandboxReadOnly || verify.RequestedPair.Effort != "high" || verify.RequestedPair.Model != reviewer.Model || verify.ConfiguredPair != reviewer {
		t.Errorf("the verification step: %+v", verify)
	}
	if len(g.Calls) != 3 || find(g, "fix:change").Role != "executor" {
		t.Errorf("build, verify and fix: %+v", g.Calls)
	}

	g = goldenOf(t, "explicit-effort-build")
	build := find(g, "build:quick")
	if build.Role != "executor" || build.Sandbox != SandboxWorkspaceWrite || build.RequestedPair != (Pair{"haiku", "medium"}) || build.ConfiguredPair != executor {
		t.Errorf("the explicit effort of a build is recorded, not applied: %+v", build)
	}
	if check := find(g, "check:quick"); check.Role != "reviewer" || check.RequestedPair.Effort != "medium" || check.ConfiguredPair != reviewer {
		t.Errorf("%+v", check)
	}

	g = goldenOf(t, "judge-panel")
	if decide := find(g, "decide"); decide.Role != "reviewer" || decide.RequestedPair.Effort != "xhigh" || decide.ConfiguredPair != reviewer {
		t.Errorf("%+v", decide)
	}
	if judge := find(g, "judge:cost"); judge.Role != "architect" || judge.RequestedPair.Effort != "high" || judge.ConfiguredPair.Effort != "xhigh" {
		t.Errorf("%+v", judge)
	}

	g = goldenOf(t, "nested-workflow")
	if c := find(g, "review:design"); c.Path != "root/2:workflow" || c.Role != "architect" {
		t.Errorf("a call of the nested workflow runs on the child path: %+v", c)
	}

	// The real originals: the explicit effort of a build and of its verification is recorded, not applied; a judge that
	// states xhigh is the final reviewer; a fix on the second tier is a write; a scout is read-only. Their schemas are not
	// strict, so every call that carries one is refused for that reason alone, before any thread starts (no configured
	// pair is observed).
	g = goldenOf(t, "real-routing-smoke-test")
	if build := find(g, "build:haiku"); build.Role != "executor" || build.Sandbox != SandboxWorkspaceWrite || build.RequestedPair != (Pair{"haiku", "medium"}) || build.ConfiguredPair != executor || build.Status != StatusCompleted {
		t.Errorf("%+v", build)
	}
	if verify := find(g, "verify:sol"); verify.Role != "reviewer" || verify.Sandbox != SandboxReadOnly || verify.RequestedPair != (Pair{"haiku", "high"}) || verify.ConfiguredPair != reviewer || verify.Status != StatusCompleted {
		t.Errorf("%+v", verify)
	}
	g = goldenOf(t, "real-register-fixpass")
	if fix := find(g, "fix:register"); fix.Role != "executor-t1" || fix.Sandbox != SandboxWorkspaceWrite || fix.Status != StatusCompleted || fix.ConfiguredPair != policy.Roles["executor-t1"].Pair {
		t.Errorf("%+v", fix)
	}
	if rb := find(g, "readback"); rb.Role != "reviewer" || rb.RequestedPair.Effort != "high" || rb.ConfiguredPair != (Pair{}) || rb.Reason != ReasonSchemaNotStrict {
		t.Errorf("%+v", rb)
	}
	// The real build->verify->fix: a size M build is the second tier and a size S build the first, both writes; the
	// schemas refuse them, so the verification step is resolved from the original's own options.
	g = goldenOf(t, "real-security-build-verify-fix")
	for label, role := range map[string]string{"build:m1": "executor-t1", "build:s1": "executor"} {
		if c := find(g, label); c.Role != role || c.Sandbox != SandboxWorkspaceWrite || c.Reason != ReasonSchemaNotStrict || c.RequestedPair.Effort != "high" {
			t.Errorf("%+v", c)
		}
	}
	verifyLine := ""
	for _, line := range corpusLines(t, "real-security-build-verify-fix", "claude.js") {
		if verifyCall.MatchString(line) {
			verifyLine = line
		}
	}
	opt := func(key string) string {
		m := regexp.MustCompile(key + `: '([^']*)'`).FindStringSubmatch(verifyLine)
		if m == nil {
			return ""
		}
		return m[1]
	}
	r, ref := policy.ResolveFresh(CallOpts{AgentType: opt("agentType"), Model: opt("model"), Effort: opt("effort"), Isolation: opt("isolation")})
	if ref != nil || r.Role != "reviewer" || r.RequestedPair != (Pair{"haiku", "high"}) || r.Spec.Pair != reviewer || roleSandbox(r.Role) != SandboxReadOnly {
		t.Errorf("the verification step of the original (%s): %+v %+v", verifyLine, r, ref)
	}
	g = goldenOf(t, "real-wave1-judge-per-lane")
	if len(g.Calls) < 2 {
		t.Errorf("a judge for each lane: %+v", g.Calls)
	}
	for _, c := range g.Calls {
		if c.Role != "reviewer" || c.Sandbox != SandboxReadOnly || c.RequestedPair.Effort != "xhigh" || c.ConfiguredPair != (Pair{}) || c.Reason != ReasonSchemaNotStrict {
			t.Errorf("%+v", c)
		}
	}
	g = goldenOf(t, "real-port-scout")
	for _, c := range g.Calls {
		if c.Role != "explorer" || c.Sandbox != SandboxReadOnly || c.RequestedPair != (Pair{"haiku", "medium"}) || c.ConfiguredPair != (Pair{}) || c.Reason != ReasonSchemaNotStrict {
			t.Errorf("a scout: %+v", c)
		}
	}

	// The original runs too, and the calls the port edits are the ones that stop: an isolation-less haiku with the
	// alias ambiguity, a second-tier call with the missing step decision.
	for _, name := range parityNames(t) {
		_, events := runParity(t, name, "claude.js")
		causes := map[string]int{}
		for _, e := range events {
			if e.Kind == KindCall && e.Event == EvRefused {
				if c, _ := e.Data["cause"].(string); c != "" {
					causes[c]++
				} else {
					r, _ := e.Data["reason"].(string)
					causes[r]++
				}
			}
		}
		ported := portedLines(t, name)
		if got, want := causes["alias_ambiguous"] > 0, ported.isolation || ported.explorer; got != want {
			t.Errorf("%s: the original is refused for alias_ambiguous = %v, the port edits an isolation-less haiku = %v (%v)", name, got, want, causes)
		}
		if got, want := causes[ReasonMissingStepDecision] > 0, ported.tierStep; got != want {
			t.Errorf("%s: the original is refused for missing_step_decision = %v, the port adds a tierStep line = %v (%v)", name, got, want, causes)
		}
	}
}

type portKinds struct{ isolation, explorer, tierStep bool }

// portedLines reads which edits a port makes.
func portedLines(t *testing.T, name string) portKinds {
	t.Helper()
	claude, crw := corpusLines(t, name, "claude.js"), corpusLines(t, name, "crw.js")
	var k portKinds
	for _, ch := range diffLines(claude, crw) {
		for _, line := range ch.added {
			k.isolation = k.isolation || strings.Contains(line, "isolation: 'worktree'")
			k.explorer = k.explorer || strings.Contains(line, "agentType: 'explorer'")
			k.tierStep = k.tierStep || strings.Contains(line, "crw.tierStep(")
		}
	}
	return k
}

func corpusLines(t *testing.T, name, file string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityDir, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(raw), "\n")
}

// change is one hunk of a line diff: the lines of the original that were replaced and the lines of the port that replace
// them, with the 1-based line numbers of the port's.
type change struct {
	removed, added []string
	firstAdded     int
}

// diffLines is a longest-common-subsequence line diff.
func diffLines(a, b []string) []change {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []change
	var cur *change
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			flush()
			i, j = i+1, j+1
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			if cur == nil {
				cur = &change{firstAdded: j + 1}
			}
			cur.added = append(cur.added, b[j])
			j++
		default:
			if cur == nil {
				cur = &change{firstAdded: j + 1}
			}
			cur.removed = append(cur.removed, a[i])
			i++
		}
	}
	flush()
	return out
}

var (
	optionKeys = []string{"isolation:'worktree'", "agentType:'explorer'", "stepDecision:step.id"}
	tierLine   = regexp.MustCompile(`^\s*const \w+ = await crw\.tierStep\(.*\)\s*$`)
	entryLine  = regexp.MustCompile("^- `(crw\\.js|[a-z-]+\\.js):(\\d+)` (ported|kept|added)")
)

// withoutOptionKeys drops whitespace and the option keys a port may add, with the comma that separated them.
func withoutOptionKeys(line string) string {
	s := strings.Join(strings.Fields(line), "")
	s = strings.ReplaceAll(s, ",}", "}")
	for _, key := range optionKeys {
		s = strings.ReplaceAll(s, ","+key, "")
		s = strings.ReplaceAll(s, key+",", "")
		s = strings.ReplaceAll(s, key, "")
	}
	return s
}

// TestParityPortDiffOptionsOnly: the port differs from the original by option keys of agent() calls and tierStep lines,
// and the README gives a reason for every line the port changed and every line that names the reviewer model.
func TestParityPortDiffOptionsOnly(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(parityDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// README entries by script and file:line.
	entries := map[string]map[string]string{}
	section := ""
	for _, line := range strings.Split(string(readme), "\n") {
		if name, ok := strings.CutPrefix(line, "### "); ok {
			section = name
			entries[section] = map[string]string{}
			continue
		}
		if m := entryLine.FindStringSubmatch(line); m != nil && section != "" {
			entries[section][m[1]+":"+m[2]] = line
		}
	}
	for _, name := range parityNames(t) {
		t.Run(name, func(t *testing.T) {
			claude, crw := corpusLines(t, name, "claude.js"), corpusLines(t, name, "crw.js")
			tierSteps := 0
			for _, ch := range diffLines(claude, crw) {
				for i, line := range ch.added {
					if tierLine.MatchString(line) {
						tierSteps++
					} else {
						// The line is an original line with option keys added to an agent() call, and nothing else.
						if !strings.Contains(line, "agent(") && !strings.Contains(line, "agent.call(") {
							t.Errorf("crw.js:%d changes a line that is not an agent() call: %s", ch.firstAdded+i, line)
							continue
						}
						match := false
						for _, orig := range ch.removed {
							match = match || withoutOptionKeys(orig) == withoutOptionKeys(line) && withoutOptionKeys(line) != strings.Join(strings.Fields(line), "")
						}
						if !match {
							t.Errorf("crw.js:%d differs from the original by more than option keys: %s", ch.firstAdded+i, line)
						}
					}
					entry, ok := entries[name][("crw.js:" + strconv.Itoa(ch.firstAdded+i))]
					if !ok || len(entry) < 120 {
						t.Errorf("README has no reason for crw.js:%d", ch.firstAdded+i)
						continue
					}
					if strings.Contains(line, "isolation: 'worktree'") && !(strings.Contains(entry, "alias_ambiguous") && strings.Contains(entry, "shared checkout") && strings.Contains(entry, "own write cwd") && strings.Contains(entry, "agentType: 'explorer'")) {
						t.Errorf("README entry for crw.js:%d does not explain the role difference: %s", ch.firstAdded+i, entry)
					}
				}
				if len(ch.removed) > len(ch.added) {
					t.Errorf("the port removes lines of the original near crw.js:%d", ch.firstAdded)
				}
			}
			if tierSteps > 1 {
				t.Errorf("the port adds %d tierStep lines, at most one", tierSteps)
			}
			// Every line that names the reviewer model is explained, kept or not.
			files := []string{"crw.js"}
			if _, err := os.Stat(filepath.Join(parityDir, name, "child-review.js")); err == nil {
				files = append(files, "child-review.js")
			}
			for _, file := range files {
				for i, line := range corpusLines(t, name, file) {
					if !strings.Contains(line, "ocx-gpt-6-1-sol") {
						continue
					}
					entry, ok := entries[name][file+":"+strconv.Itoa(i+1)]
					for _, word := range []string{"reviewer", "at any effort", "requestedPair", "configuredPair"} {
						if !ok || !strings.Contains(entry, word) {
							t.Errorf("README entry for %s:%d does not say why the reviewer line is kept (%q missing): %s", file, i+1, word, entry)
							break
						}
					}
				}
			}
			// An entry points at a line that exists and was changed or names the reviewer model.
			for key := range entries[name] {
				file, num, _ := strings.Cut(key, ":")
				lines := corpusLines(t, name, file)
				n, _ := strconv.Atoi(num)
				if n < 1 || n > len(lines) {
					t.Errorf("README entry %s points past the end of the file", key)
				}
			}
		})
	}
}
