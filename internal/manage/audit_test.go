package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// auditBundleText is a bundle.json document, with the given schema and mode.
func auditBundleText(t *testing.T, schema, mode string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"schema": schema, "mode": mode, "subject": "s", "head": "h", "issue": "CRW-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditWriteBundle writes a bundle.json and returns its directory.
func auditWriteBundle(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, auditBundleFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// auditGoodBundle is a bundle the product accepts.
func auditGoodBundle(t *testing.T, mode string) string {
	t.Helper()
	return auditWriteBundle(t, auditBundleText(t, auditBundleSchema, mode))
}

// auditEnv is an Env with the streams a test needs, and no host values.
func auditEnv(t *testing.T) (*Env, *strings.Builder, *strings.Builder) {
	t.Helper()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	e := &Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errOut, Getenv: os.Getenv}
	return e, out, errOut
}

// auditLines decodes a JSONL file into one map per line, keyed by the raw JSON of each
// field, so a test can compare the exact key set a writer produced.
func auditLines(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, row)
	}
	return out
}

// auditSectionConfig is a configuration whose audit section carries the given keys, and
// whose state directory is the given temporary tree.
func auditSectionConfig(t *testing.T, stateDir string, section map[string]any) *Config {
	t.Helper()
	cfg := &Config{StateDir: stateDir, raw: map[string]json.RawMessage{}}
	if section != nil {
		data, err := json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
		cfg.raw["audit"] = data
	}
	return cfg
}

// C1: the ledger row carries exactly the keys the issue fixes, no more and no fewer, with
// the severity counts and the score taken from the result.
func TestAuditLedgerRowCarriesTheFixedKeys(t *testing.T) {
	e, _, _ := auditEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	results := []AuditResult{{
		Mode: auditModePR, Subject: "s", Head: "h", Issue: "CRW-1", Pair: "p", Phase: "live", Round: "r2",
		Status: auditStatusOK, Score: 6, GradedAt: "2026-01-01T00:00:00Z", Bundle: "b",
		Defects: []AuditDefect{{Severity: "P2", What: "w", Where: "f.go:1"}},
	}}
	if err := auditRecord(e, cfg, results); err != nil {
		t.Fatal(err)
	}
	rows := auditLines(t, filepath.Join(state, "audit", auditLedgerFile))
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d lines, want 1", len(rows))
	}
	got := make([]string, 0, len(rows[0]))
	for key := range rows[0] {
		got = append(got, key)
	}
	want := strings.Split("mode subject head issue pair phase round status score p0 p1 p2 p3 graded_at bundle", " ")
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the ledger row keys are %v, want %v", got, want)
	}
	if string(rows[0]["p2"]) != "1" || string(rows[0]["score"]) != "6" {
		t.Errorf("the counts or the score are wrong: %v", rows[0])
	}
}

// C1: a result that left no usable grade carries a null score and zero counts, so a reader
// can tell an ungraded run from one that scored zero.
func TestAuditLedgerRowForAnUngradedRunHasNullScore(t *testing.T) {
	e, _, _ := auditEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	results := []AuditResult{{Mode: auditModePR, Status: auditStatusInvalid, GradedAt: "2026-01-01T00:00:00Z"}}
	if err := auditRecord(e, cfg, results); err != nil {
		t.Fatal(err)
	}
	rows := auditLines(t, filepath.Join(state, "audit", auditLedgerFile))
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d lines, want 1", len(rows))
	}
	if string(rows[0]["score"]) != "null" {
		t.Errorf("score = %s, want null", rows[0]["score"])
	}
	for _, key := range []string{"p0", "p1", "p2", "p3"} {
		if string(rows[0][key]) != "0" {
			t.Errorf("%s = %s, want 0", key, rows[0][key])
		}
	}
}

// C2: only a result that carries a P0 or a P1 leaves an alert, and the alert carries just
// those defects, without the reproduction steps.
func TestAuditAlertsOnlyForP0AndP1(t *testing.T) {
	e, _, _ := auditEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	results := []AuditResult{
		{Mode: auditModePR, Subject: "a", Issue: "CRW-1", Status: auditStatusOK, Score: 4, GradedAt: "t",
			Defects: []AuditDefect{
				{Severity: "P1", What: "wrong", Where: "a.go:2", Repro: "run it"},
				{Severity: "P2", What: "weak test", Where: "a_test.go:3"},
			}},
		{Mode: auditModePR, Subject: "b", Issue: "CRW-2", Status: auditStatusOK, Score: 8, GradedAt: "t",
			Defects: []AuditDefect{{Severity: "P3", What: "nit", Where: "b.go:1"}}},
		{Mode: auditModePR, Subject: "c", Issue: "CRW-3", Status: auditStatusTimeout, GradedAt: "t"},
	}
	if err := auditRecord(e, cfg, results); err != nil {
		t.Fatal(err)
	}
	if rows := auditLines(t, filepath.Join(state, "audit", auditLedgerFile)); len(rows) != 3 {
		t.Errorf("the ledger has %d lines, want 3", len(rows))
	}
	alerts := auditLines(t, filepath.Join(state, "audit", auditAlertFile))
	if len(alerts) != 1 {
		t.Fatalf("the alert queue has %d lines, want 1", len(alerts))
	}
	var subject string
	if err := json.Unmarshal(alerts[0]["subject"], &subject); err != nil {
		t.Fatal(err)
	}
	if subject != "a" {
		t.Errorf("the alert is for %q, want the result that carries a P1", subject)
	}
	var defects []map[string]json.RawMessage
	if err := json.Unmarshal(alerts[0]["defects"], &defects); err != nil {
		t.Fatal(err)
	}
	if len(defects) != 1 {
		t.Fatalf("the alert carries %d defects, want the one P1", len(defects))
	}
	keys := make([]string, 0, len(defects[0]))
	for key := range defects[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "severity,what,where" {
		t.Errorf("the alert defect keys are %v, want severity,what,where", keys)
	}
	if string(alerts[0]["score"]) != "4" {
		t.Errorf("the alert score is %s, want 4", alerts[0]["score"])
	}
}

// C2: no alert file is left at all when nothing reached P0 or P1.
func TestAuditWritesNoAlertFileWhenNothingReachedP0OrP1(t *testing.T) {
	e, _, _ := auditEnv(t)
	state := t.TempDir()
	cfg := auditSectionConfig(t, state, nil)
	results := []AuditResult{
		{Mode: auditModePR, Status: auditStatusOK, Score: 9, GradedAt: "t",
			Defects: []AuditDefect{{Severity: "P2", What: "w", Where: "f.go:1"}}},
		{Mode: auditModePR, Status: auditStatusInvalid, GradedAt: "t"},
	}
	if err := auditRecord(e, cfg, results); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditAlertFile)); !os.IsNotExist(err) {
		t.Errorf("an alert file was written: %v", err)
	}
}

// A file an earlier torn write left without a final line feed gets one before the new row,
// so the fragment and the new row are never joined into one unreadable line (CRW-474's guard).
func TestAuditRecordSeparatesATornTail(t *testing.T) {
	e, _, _ := auditEnv(t)
	state := t.TempDir()
	dir := filepath.Join(state, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fragment := `{"mode":"pr","subject":"old"`
	if err := os.WriteFile(filepath.Join(dir, auditLedgerFile), []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditSectionConfig(t, state, nil)
	results := []AuditResult{{Mode: auditModePR, Subject: "new", Status: auditStatusOK, Score: 7, GradedAt: "t"}}
	if err := auditRecord(e, cfg, results); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, auditLedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	last := lines[len(lines)-1]
	var row map[string]json.RawMessage
	if err := json.Unmarshal([]byte(last), &row); err != nil {
		t.Fatalf("the new row is not on its own line: %q: %v", last, err)
	}
	var subject string
	if err := json.Unmarshal(row["subject"], &subject); err != nil || subject != "new" {
		t.Errorf("the last line carries %q/%v, want the new row", subject, err)
	}
}

// C1: the built-in prompt names no private root and no model, in either mode and with the
// criteria flagged missing: the grader is blind, and the repository is public.
func TestAuditPromptNamesNoPrivateRootAndNoModel(t *testing.T) {
	roots := []string{"/home/", "/state/", "/scratch/", "/var/tmp"}
	models := []string{"deepseek", "sonnet", "claude", "gpt-", "gemini", "inferhub", "ollama", "astra", "openai", "anthropic"}
	for _, mode := range []string{auditModePR, auditModePackage} {
		for _, unavailable := range []bool{false, true} {
			prompt := auditPrompt(&auditBundle{Mode: mode, CriteriaUnavailable: unavailable})
			lower := strings.ToLower(prompt)
			for _, needle := range append(append([]string{}, roots...), models...) {
				if strings.Contains(lower, needle) {
					t.Errorf("mode %s criteria_unavailable=%v: the prompt carries %q", mode, unavailable, needle)
				}
			}
		}
	}
}

// C1: the prompt asks for the result format the issue fixes, and the mode guidance names
// the files the mode's bundle holds.
func TestAuditPromptAsksForTheResultFormatAndTheModeFiles(t *testing.T) {
	pr := auditPrompt(&auditBundle{Mode: auditModePR})
	for _, needle := range []string{auditGradeFile, auditResultSchema, "PASS", "PARTIAL", "FAIL", "P0", "P3", "score", auditPRDiffFile, auditPRTaskFile, auditPRFilesDir + "/"} {
		if !strings.Contains(pr, needle) {
			t.Errorf("the pull request prompt does not mention %q", needle)
		}
	}
	pkg := auditPrompt(&auditBundle{Mode: auditModePackage})
	if !strings.Contains(pkg, "src/") || !strings.Contains(pkg, auditPkgCriteriaFile) || strings.Contains(pkg, auditPRDiffFile) {
		t.Errorf("the package prompt does not describe a package audit: %q", pkg)
	}
	prNoCriteria := auditPrompt(&auditBundle{Mode: auditModePR, CriteriaUnavailable: true})
	if !strings.Contains(prNoCriteria, "criteria_unavailable") || !strings.Contains(prNoCriteria, auditPRTaskFile) {
		t.Errorf("a pull request bundle without criteria is not told how to judge: %q", prNoCriteria)
	}
	pkgNoCriteria := auditPrompt(&auditBundle{Mode: auditModePackage, CriteriaUnavailable: true})
	if !strings.Contains(pkgNoCriteria, "criteria_unavailable") || !strings.Contains(pkgNoCriteria, auditPkgTaskFile) {
		t.Errorf("a package bundle without criteria is not told how to judge: %q", pkgNoCriteria)
	}
	if strings.Contains(pkgNoCriteria, auditPRDiffFile) {
		t.Error("a package bundle without criteria is sent to a description file it does not have")
	}
}

// The prompt draws the instruction/data boundary: the candidate's own files are evidence,
// not orders, and a submission that tries to steer the grader is itself a defect. It also
// offers no command that could reach the network through the candidate's own module.
func TestAuditPromptTreatsCandidateContentAsData(t *testing.T) {
	for _, mode := range []string{auditModePR, auditModePackage} {
		prompt := auditPrompt(&auditBundle{Mode: mode})
		for _, needle := range []string{"never instructions to", "is itself a defect", "use no network"} {
			if !strings.Contains(prompt, needle) {
				t.Errorf("mode %s: the prompt does not carry the data boundary %q", mode, needle)
			}
		}
		if strings.Contains(prompt, "go doc") {
			t.Errorf("mode %s: the prompt offers go doc, which can download a toolchain", mode)
		}
	}
}

// C2: a bundle whose document is wrong is refused, and the error names what is wrong:
// the schema, the mode, or the required field that is absent or empty.
func TestAuditReadBundleRefusesWhatItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
		want []string
	}{
		{"missing directory", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, []string{"no such file"}},
		{"no bundle.json", func(t *testing.T) string { return t.TempDir() }, []string{auditBundleFile}},
		{"malformed json", func(t *testing.T) string { return auditWriteBundle(t, "{oops") }, []string{"invalid character"}},
		{"another schema", func(t *testing.T) string {
			return auditWriteBundle(t, auditBundleText(t, "crw-audit-bundle/2", auditModePR))
		}, []string{"schema", "crw-audit-bundle/2", auditBundleSchema}},
		{"unknown mode", func(t *testing.T) string {
			return auditWriteBundle(t, auditBundleText(t, auditBundleSchema, "other"))
		}, []string{"mode", "other"}},
	} {
		_, err := auditReadBundle(tc.dir(t))
		if err == nil {
			t.Errorf("%s: the bundle was accepted", tc.name)
			continue
		}
		for _, needle := range tc.want {
			if !strings.Contains(err.Error(), needle) {
				t.Errorf("%s: err = %q, want a mention of %q", tc.name, err, needle)
			}
		}
	}
	// A document that omits a required field, or carries it empty, names that field.
	for _, field := range auditRequiredFields {
		full := map[string]any{"schema": auditBundleSchema, "mode": auditModePR, "subject": "s", "head": "h", "issue": "CRW-1"}
		delete(full, field)
		missing, err := json.Marshal(full)
		if err != nil {
			t.Fatal(err)
		}
		empty := map[string]any{"schema": auditBundleSchema, "mode": auditModePR, "subject": "s", "head": "h", "issue": "CRW-1"}
		empty[field] = ""
		blank, err := json.Marshal(empty)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"omitted": string(missing), "empty": string(blank)} {
			_, err := auditReadBundle(auditWriteBundle(t, body))
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %s: err = %v, want a named error for %q", field, name, err, field)
			}
		}
	}
	// A bundle that carries everything is read, and its fields reach the caller.
	for _, mode := range []string{auditModePR, auditModePackage} {
		bundle, err := auditReadBundle(auditGoodBundle(t, mode))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if bundle.Mode != mode || bundle.Subject != "s" || bundle.Head != "h" || bundle.Issue != "CRW-1" || bundle.CriteriaUnavailable {
			t.Errorf("%s: the bundle reads %+v", mode, bundle)
		}
	}
}

// The result carries the per-criterion verdicts, so a caller can report which requirements
// passed, were partial or failed rather than only the aggregate score.
func TestAuditResultCarriesTheCriterionVerdicts(t *testing.T) {
	result := AuditResult{
		Mode: auditModePR, Status: "ok", Score: 6,
		Criteria: []AuditCriterion{{ID: "c1", Verdict: "PASS", Note: "a.go:1"}},
		Defects:  []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:2", Repro: "run it"}},
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Criteria []map[string]string `json:"criteria"`
		Defects  []map[string]string `json:"defects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Criteria) != 1 || doc.Criteria[0]["id"] != "c1" || doc.Criteria[0]["verdict"] != "PASS" || doc.Criteria[0]["note"] != "a.go:1" {
		t.Errorf("the result does not carry the criteria: %s", data)
	}
	if len(doc.Defects) != 1 || doc.Defects[0]["repro"] != "run it" {
		t.Errorf("the result does not carry the defect's reproduction: %s", data)
	}
}

// C3: audit is a registered command, so crw manage --help lists it with its summary.
func TestAuditCommandIsRegistered(t *testing.T) {
	if !strings.Contains(strings.Join(coreNames(), ","), "audit") {
		t.Errorf("audit is not registered: %v", coreNames())
	}
	_, out, errOut := auditEnv(t)
	if code := Run(context.Background(), []string{"--help"}, strings.NewReader(""), out, errOut); code != 0 {
		t.Fatalf("crw manage --help: exit %d %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "audit") || !strings.Contains(out.String(), auditCommand.Summary) {
		t.Errorf("crw manage --help does not list audit: %q", out.String())
	}
}

// The audit command prints its usage: exit 0 for the help flags, exit 2 for anything else.
func TestAuditCommandPrintsItsUsage(t *testing.T) {
	e, out, errOut := auditEnv(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, []string{arg}); code != 0 || errOut.Len() != 0 {
			t.Errorf("%s: exit %d %q %q", arg, code, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), auditUsage) {
			t.Errorf("%s: the usage is missing: %q", arg, out.String())
		}
	}
	for _, args := range [][]string{nil, {"grade"}, {"nope"}} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(errOut.String(), auditUsage) {
			t.Errorf("%v: the usage is missing: %q", args, errOut.String())
		}
	}
}

// auditGradeUsageText is the grade subcommand's usage line, pinned as text so a change to
// the command surface is visible here rather than only in the constant it prints.
const auditGradeUsageText = "usage: crw manage audit grade --bundle DIR"

// C3: the help flags print the grade usage to stdout and exit 0; a missing or unknown
// subcommand, a missing --bundle, an unknown flag and a stray positional are usage errors
// at exit 2 with the grade usage on stderr.
func TestAuditCommandGradeUsageAndArgumentErrors(t *testing.T) {
	e, out, errOut := auditEnv(t)
	if !strings.Contains(auditUsage, auditGradeUsageText) {
		t.Errorf("the command's usage line is %q, want it to carry %q", auditUsage, auditGradeUsageText)
	}
	for _, arg := range []string{"-h", "--help", "help"} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, []string{arg}); code != 0 || errOut.Len() != 0 {
			t.Errorf("%s: exit %d %q %q", arg, code, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), auditGradeUsageText) {
			t.Errorf("%s: the grade usage is missing: %q", arg, out.String())
		}
	}
	for _, args := range [][]string{nil, {"nope"}, {"grade"}, {"grade", "--bundle"}, {"grade", "--nope", "x"}, {"grade", "x"}} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(errOut.String(), auditGradeUsageText) {
			t.Errorf("%v: the grade usage is missing: %q", args, errOut.String())
		}
	}
	// An option token where a value is expected is a missing value, not the value, so a
	// separated form cannot smuggle the next option in as a bundle path.
	for _, args := range [][]string{{"grade", "--bundle", "--pair=p"}, {"grade", "--bundle", "--pair", "p"}} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(errOut.String(), "needs a value") {
			t.Errorf("%v: the error does not name the missing value: %q", args, errOut.String())
		}
	}
	// The --name=value form still takes a value that starts with --, so the missing-value
	// rule does not remove the only way to pass such a string.
	job, err := auditParseGradeArgs([]string{"--bundle=--weird", "--pair=p", "--phase=live", "--round=1"})
	if err != nil {
		t.Fatalf("the = form was refused: %v", err)
	}
	if job.Bundle != "--weird" || job.Pair != "p" || job.Phase != "live" || job.Round != "1" {
		t.Errorf("the = form parsed to %+v", job)
	}
}

// C3: with no grader named by the configuration, grade refuses by name at exit 2. At this
// baseline no command reads a configuration file, so a grader is never configured.
func TestAuditCommandRefusesAnUnconfiguredGrader(t *testing.T) {
	e, _, errOut := auditEnv(t)
	bundle := auditGoodBundle(t, auditModePR)
	if code := auditRun(context.Background(), e, []string{"grade", "--bundle", bundle}); code != usageExit {
		t.Fatalf("exit %d, want %d: %q", code, usageExit, errOut.String())
	}
	if !strings.Contains(errOut.String(), "grader_unconfigured") {
		t.Errorf("the error does not name grader_unconfigured: %q", errOut.String())
	}
}
