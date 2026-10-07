package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The tests in this file pin the three reading defects the post-merge evaluation of the
// audit list pull request found: a round document that is not a round, the word "help" in a
// value position, and a repeated option. Every helper here carries the auditListReview822
// prefix so it stays identifiable inside the shared package.

// auditListReview822State points HOME, CODEX_HOME, CRW_HOME and XDG_STATE_HOME at a fresh
// temporary tree and returns the manage state directory below it, so no test in this file
// reaches the real ones.
func auditListReview822State(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	t.Setenv("XDG_STATE_HOME", state)
	return filepath.Join(state, "crw", "manage")
}

// auditListReview822WriteRoundBody writes one round file below a state directory with the
// exact bytes given, so a test can put a document that is not a round there.
func auditListReview822WriteRoundBody(t *testing.T, state, name, body string) {
	t.Helper()
	dir := filepath.Join(state, "audit", "rounds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// auditListReview822RoundBody is a round document with one pending package.
func auditListReview822RoundBody(name string) string {
	return fmt.Sprintf("{\"round\":%q,\"started_at\":\"2026-01-01T00:00:00Z\",\"packages\":[{\"package\":\"pkg/a\",\"state\":\"pending\"}]}\n", name)
}

// auditListReview822Document runs the list command and decodes whatever it printed, returning
// the parse error instead of failing the test: these tests compare a wrong answer with the
// right one, so every case has to be observed rather than cut short at the first.
func auditListReview822Document(t *testing.T, e *Env, args ...string) (AuditListing, int, string, error) {
	t.Helper()
	e.Stdout.(*strings.Builder).Reset()
	e.Stderr.(*strings.Builder).Reset()
	code := auditRun(context.Background(), e, append([]string{"list"}, args...))
	out, _ := e.Stdout.(*strings.Builder)
	raw := out.String()
	if strings.TrimSpace(raw) == "" {
		return AuditListing{}, code, raw, nil
	}
	var listing AuditListing
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &listing); err != nil {
		return AuditListing{}, code, raw, err
	}
	return listing, code, raw, nil
}

// C1: a round document that is not a JSON object, and a round whose name is empty, are
// refused; in the listing that source is unknown with a reason, a null list and exit 1.
func TestAuditListReview822NullRoundIsUnknown(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	state := auditListReview822State(t)
	auditListReview822WriteRoundBody(t, state, "r1", "null\n")

	listing, code, raw := auditListCommandDocument(t, e)
	source := auditListSourceOf(t, listing, auditListNameRounds)
	if source.State != auditListStateUnknown {
		t.Errorf("a null round file is %q, want %q", source.State, auditListStateUnknown)
	}
	if !strings.Contains(source.Reason, "not a round document") {
		t.Errorf("the reason is %q, want it to name a document that is not a round", source.Reason)
	}
	if listing.Rounds != nil {
		t.Errorf("the rounds list is %+v, want null", listing.Rounds)
	}
	if !strings.Contains(raw, "\"rounds\":null") {
		t.Errorf("the document printed %s, want a null rounds list", raw)
	}
	if code != auditListUnknownExit {
		t.Errorf("the command exited %d, want %d", code, auditListUnknownExit)
	}

	// The loader is shared, so every document that is not an object and every round that
	// names no round reaches the same judgement.
	dir := t.TempDir()
	bodies := []string{"null", "[]", "7", "\"r1\"", "true", "{}", "{\"started_at\":\"2026-01-01T00:00:00Z\"}", "{\"round\":\"\",\"packages\":[]}"}
	for i, body := range bodies {
		path := filepath.Join(dir, fmt.Sprintf("r%d.json", i))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		doc, err := auditRoundLoad(path)
		if err == nil {
			t.Errorf("auditRoundLoad accepted %s as %+v", body, doc)
			continue
		}
		if !strings.Contains(err.Error(), "not a round document") {
			t.Errorf("auditRoundLoad(%s) said %q, want it to name a document that is not a round", body, err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("auditRoundLoad(%s) said %q, want it to name the file", body, err)
		}
	}
	// A document that is malformed JSON keeps its own message rather than reading as a
	// document of another shape, and a whole round still loads.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auditRoundLoad(broken); err == nil {
		t.Error("auditRoundLoad accepted a truncated document")
	} else if strings.Contains(err.Error(), "not a round document") {
		t.Errorf("a malformed document said %q, want the JSON error", err)
	}
	whole := filepath.Join(dir, "whole.json")
	if err := os.WriteFile(whole, []byte(auditListReview822RoundBody("r2")), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := auditRoundLoad(whole)
	if err != nil {
		t.Fatalf("auditRoundLoad over a whole round: %v", err)
	}
	if doc.Round != "r2" || len(doc.Packages) != 1 || doc.Packages[0].Package != "pkg/a" {
		t.Errorf("a whole round read %+v", doc)
	}
}

// C2: the word after --round, --issue and --since is a value whatever it is, so "help" names
// the round help and reads the same as --round=help; help comes only from an option-position
// -h/--help or a first argument help.
func TestAuditListReview822HelpIsARoundName(t *testing.T) {
	e, out, errOut := auditTestEnv(t)
	state := auditListReview822State(t)
	auditListReview822WriteRoundBody(t, state, "help", auditListReview822RoundBody("help"))
	auditListReview822WriteRoundBody(t, state, "r1", auditListReview822RoundBody("r1"))

	// Help is still reachable where the rule says it is: a first argument help, and -h or
	// --help in an option position.
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"}, {"--round", "r1", "--help"},
		// The ordering pin: a help request in an option position wins over a repeated
		// option, so this input is the usage and not the refusal.
		{"--round", "a", "--round", "b", "--help"},
	} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, append([]string{"list"}, args...)); code != 0 {
			t.Errorf("%q exited %d, want 0", args, code)
		}
		if !strings.Contains(out.String(), auditListUsage) {
			t.Errorf("%q printed %q, want the usage", args, out.String())
		}
	}
	// --issue and --since consume their value the same way, so the word after either is a
	// value and not a help request: an issue named help filters and matches nothing, and a
	// since of help is a time this build cannot read rather than a help request.
	for _, tc := range []struct {
		args []string
		want int
		says string
	}{
		{args: []string{"--issue", "help"}, want: 0},
		{args: []string{"--since", "help"}, want: usageExit, says: "not an RFC 3339 time"},
		{args: []string{"--round", "help", "--issue", "help", "--since", "help"}, want: usageExit, says: "not an RFC 3339 time"},
		// The value slot is consumed whatever the token looks like, so a --help that a
		// preceding option consumes is a missing value rather than a help request.
		{args: []string{"--round", "--help"}, want: usageExit, says: "needs a value"},
	} {
		out.Reset()
		errOut.Reset()
		code := auditRun(context.Background(), e, append([]string{"list"}, tc.args...))
		if code != tc.want {
			t.Errorf("%q exited %d, want %d: %q", tc.args, code, tc.want, errOut.String())
		}
		if strings.Contains(out.String(), "usage:") {
			t.Errorf("%q printed the usage: %q", tc.args, out.String())
		}
		if tc.says != "" && !strings.Contains(errOut.String(), tc.says) {
			t.Errorf("%q said %q, want it to name %q", tc.args, errOut.String(), tc.says)
		}
	}
	// A word that is neither an option position nor the first argument is not help.
	out.Reset()
	errOut.Reset()
	if code := auditRun(context.Background(), e, []string{"list", "--round", "r1", "help"}); code != usageExit {
		t.Errorf("a trailing help exited %d, want %d", code, usageExit)
	}

	// The word after --round names the round help, exactly as --round=help does. This runs
	// last so the value-slot and ordering pins above are observed even while this one fails.
	spaced, code, raw, err := auditListReview822Document(t, e, "--round", "help")
	if err != nil {
		t.Fatalf("--round help printed %q, which is not one document: %v", raw, err)
	}
	if code != 0 {
		t.Fatalf("--round help exited %d, want 0: %q", code, errOut.String())
	}
	if strings.Contains(raw, "usage:") {
		t.Errorf("--round help printed the usage: %q", raw)
	}
	if len(spaced.Rounds) != 1 || spaced.Rounds[0].Round != "help" {
		t.Errorf("--round help selected %+v, want the round named help", spaced.Rounds)
	}
	equals, code, _, err := auditListReview822Document(t, e, "--round=help")
	if err != nil {
		t.Fatalf("--round=help printed something that is not one document: %v", err)
	}
	if code != 0 {
		t.Fatalf("--round=help exited %d, want 0: %q", code, errOut.String())
	}
	if !reflect.DeepEqual(spaced, equals) {
		t.Errorf("--round help read %+v, --round=help read %+v", spaced, equals)
	}

	// The same round read through the function the GUI calls.
	opts, err := auditListParseArgs([]string{"--round", "help"})
	if err != nil {
		t.Fatalf("auditListParseArgs over --round help: %v", err)
	}
	if opts.Round != "help" {
		t.Errorf("--round help parsed round %q, want help", opts.Round)
	}
}

// C3: the shared audit parser refuses a repeated option, so an empty value cannot be
// overwritten by a later one and escape the empty-value refusal.
func TestAuditListReview822RepeatedOptionIsRefused(t *testing.T) {
	e, out, errOut := auditTestEnv(t)
	auditListReview822State(t)

	for _, args := range [][]string{
		{"--round=", "--round=r1"},
		{"--issue=", "--issue=CRW-1"},
		{"--since=", "--since=2026-01-01T00:00:00Z"},
		{"--round=r1", "--round=r2"},
		{"--round", "r1", "--round", "r2"},
	} {
		out.Reset()
		errOut.Reset()
		if code := auditRun(context.Background(), e, append([]string{"list"}, args...)); code != usageExit {
			t.Errorf("%q exited %d, want %d: %q", args, code, usageExit, errOut.String())
		}
		if !strings.Contains(errOut.String(), "given twice") {
			t.Errorf("%q said %q, want it to refuse the repeated option", args, errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("%q printed %q, want no document", args, out.String())
		}
	}

	// The parser itself, and the two judgements it makes in order: an unknown option stays
	// unknown, and a repeat names the option it repeats.
	allowed := map[string]bool{"round": true, "issue": true, "since": true}
	if _, err := auditPkgParseArgs([]string{"--round=", "--round=r1"}, allowed); err == nil {
		t.Error("auditPkgParseArgs accepted a repeated option")
	} else if err.Error() != "the option --round is given twice" {
		t.Errorf("the refusal is %q, want it to name the option given twice", err)
	}
	if _, err := auditPkgParseArgs([]string{"--nope=x", "--nope=x"}, allowed); err == nil {
		t.Error("auditPkgParseArgs accepted an unknown option")
	} else if !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("a repeated unknown option said %q, want it to stay unknown", err)
	}
	// The other callers of the shared parser inherit the rule.
	if code := auditRun(context.Background(), e, []string{"round", "status", "--name", "r1", "--name", "r2"}); code != usageExit {
		t.Errorf("round status with a repeated --name exited %d, want %d", code, usageExit)
	}
	if code := auditRun(context.Background(), e, []string{"drafts", "--round", "r1", "--round", "r2"}); code != usageExit {
		t.Errorf("drafts with a repeated --round exited %d, want %d", code, usageExit)
	}
	// A single empty value is still refused by the listing, not by the parser.
	out.Reset()
	errOut.Reset()
	if code := auditRun(context.Background(), e, []string{"list", "--round="}); code != usageExit {
		t.Errorf("--round= exited %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), "needs a value") {
		t.Errorf("--round= said %q, want the empty-value refusal", errOut.String())
	}
}
