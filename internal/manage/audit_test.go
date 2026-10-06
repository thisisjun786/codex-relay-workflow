package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	for _, needle := range []string{auditGradeFile, auditResultSchema, "PASS", "PARTIAL", "FAIL", "P0", "P3", "score", "candidate/diff.patch", "candidate/tree/"} {
		if !strings.Contains(pr, needle) {
			t.Errorf("the pull request prompt does not mention %q", needle)
		}
	}
	pkg := auditPrompt(&auditBundle{Mode: auditModePackage})
	if !strings.Contains(pkg, "candidate/tree/") || strings.Contains(pkg, "candidate/diff.patch") {
		t.Errorf("the package prompt does not describe a package audit: %q", pkg)
	}
	if !strings.Contains(auditPrompt(&auditBundle{Mode: auditModePR, CriteriaUnavailable: true}), "criteria_unavailable") {
		t.Error("a bundle without criteria is not told so")
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
