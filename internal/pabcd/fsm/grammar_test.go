package fsm

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
)

// The tests below are the 11 of orchestrate-grammar.test.ts in the oracle's order, with the crw spellings of the prefixes
// (name-substitution R1, R10, R27), then the checks that belong to the Go port alone.

func verbOf(prompt string) OrchestrateVerb {
	if c := ParseOrchestrateCommand(prompt); c != nil {
		return c.Verb
	}
	return ""
}

func TestParsesBarePhaseCommandsCaseInsensitively(t *testing.T) {
	for p, want := range map[string]*OrchestrateCommand{"orchestrate p": {Verb: VerbP}, "orchestrate A": {Verb: VerbA}, "ORCHESTRATE d": {Verb: VerbD}} {
		if got := ParseOrchestrateCommand(p); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v, want %+v", p, got, want)
		}
	}
}

func TestParsesControlVerbs(t *testing.T) {
	if verbOf("orchestrate status") != VerbStatus || verbOf("orchestrate reset") != VerbReset {
		t.Error("status or reset not parsed")
	}
}

func TestAcceptsNamespacedAndShorthandPrefixes(t *testing.T) {
	for p, want := range map[string]OrchestrateVerb{"$crw:crw-orchestrate P": VerbP, "$crw-orchestrate p": VerbP, "crw orchestrate a": VerbA, "/orchestrate c": VerbC} {
		if got := verbOf(p); got != want {
			t.Errorf("%q: got %q, want %q", p, got, want)
		}
	}
}

func TestRejectsIdleUnknownVerbsProseAndTheBareWord(t *testing.T) {
	for _, p := range []string{"orchestrate idle", "orchestrate x", "orchestrate proper testing", "orchestrate", ""} {
		if c := ParseOrchestrateCommand(p); c != nil {
			t.Errorf("%q parsed as %+v", p, c)
		}
	}
}

func TestDoesNotMatchAVerbBuriedMidSentence(t *testing.T) {
	for _, p := range []string{"please orchestrate p for me", "we should orchestrate proper tests"} {
		if c := ParseOrchestrateCommand(p); c != nil {
			t.Errorf("%q parsed as %+v", p, c)
		}
	}
}

func TestFindsTheCommandOnItsOwnLine(t *testing.T) {
	if got := verbOf("here is some context\norchestrate b\nthanks"); got != VerbB {
		t.Errorf("got %q", got)
	}
}

func TestAttestWithASpaceContainingDidParses(t *testing.T) {
	c := ParseOrchestrateCommand("orchestrate A --attest {\"from\":\"P\",\"to\":\"A\",\"did\":\"challenged the plan\"}")
	if c == nil || c.Verb != VerbA || c.AttestError != "" || !reflect.DeepEqual(c.Attest, &attest.Attestation{From: "P", To: "A", Did: "challenged the plan"}) {
		t.Errorf("got %+v", c)
	}
}

func TestAttestKeepsCheckOutputAndExitCode(t *testing.T) {
	c := ParseOrchestrateCommand("orchestrate d --attest {\"from\":\"C\",\"to\":\"D\",\"did\":\"ran tests\",\"checkOutput\":\"233 pass\",\"exitCode\":0}")
	if c == nil || c.Attest == nil || c.Attest.CheckOutput != "233 pass" || c.Attest.ExitCode == nil || *c.Attest.ExitCode != 0 {
		t.Errorf("got %+v", c)
	}
}

func TestMalformedAttestSetsAnErrorAndNeverFails(t *testing.T) {
	bad := ParseOrchestrateCommand("orchestrate a --attest {\"from\":\"P\",\"to\":\"A\"")
	if bad == nil || bad.Verb != VerbA || bad.Attest != nil || !regexp.MustCompile("(?i)balanced|valid").MatchString(bad.AttestError) {
		t.Errorf("unbalanced: %+v", bad)
	}
	if notJSON := ParseOrchestrateCommand("orchestrate a --attest {nope}"); notJSON == nil || notJSON.Attest != nil || notJSON.AttestError == "" {
		t.Errorf("not JSON: %+v", notJSON)
	}
}

func TestAttestWithoutFromAndToIsCoercedToNil(t *testing.T) {
	c := ParseOrchestrateCommand("orchestrate a --attest {\"did\":\"x\"}")
	if c == nil || c.Attest != nil || !regexp.MustCompile("(?i)from/to").MatchString(c.AttestError) {
		t.Errorf("got %+v", c)
	}
}

func TestExtractBalancedJSONRespectsBracesInStrings(t *testing.T) {
	for in, want := range map[string]string{"{\"did\":\"a } b\"} trailing": "{\"did\":\"a } b\"}", "no json here": "", "{\"unbalanced\":true": "", "noise {\"a\":1} x": "{\"a\":1}"} {
		if got, ok := ExtractBalancedJSON(in); got != want || ok != (want != "") {
			t.Errorf("%q: got %q, %v", in, got, ok)
		}
	}
}

// CRW-1109 (:135): the oracle looks the verb up in a plain object, so the inherited key constructor answered with a verb of
// its own, which the chat hook refused quoting the function's source. Only the documented verbs are verbs now: constructor is
// no command, like any other unknown word, and the documented verbs are unchanged.
func TestConstructorIsNoVerb(t *testing.T) {
	for _, line := range []string{"orchestrate Constructor", "orchestrate constructor", "/orchestrate CONSTRUCTOR"} {
		if c := ParseOrchestrateCommand(line); c != nil {
			t.Errorf("%q: got %+v, want no command", line, c)
		}
	}
	for line, verb := range map[string]OrchestrateVerb{"orchestrate i": VerbI, "orchestrate P": VerbP, "orchestrate a": VerbA, "orchestrate B": VerbB,
		"orchestrate c": VerbC, "orchestrate D": VerbD, "orchestrate status": VerbStatus, "orchestrate RESET": VerbReset} {
		if c := ParseOrchestrateCommand(line); !reflect.DeepEqual(c, &OrchestrateCommand{Verb: verb}) {
			t.Errorf("%q: got %+v", line, c)
		}
	}
}

// CRW-1109 (:137): a would-be command whose --attest text holds a CR, U+2028 or U+2029 is answered with its FormError, where
// the oracle read it as chat; the same character written as a JSON escape, or in the white space after the verb, or a line that
// only mentions a command in prose, is unchanged.
func TestSeparatorInACommandIsAFormError(t *testing.T) {
	for _, sep := range []string{"\r", "\u2028", "\u2029"} {
		line := `orchestrate A --attest {"from":"P","to":"A","did":"a` + sep + `b"}`
		if c := ParseOrchestrateCommand(line); c == nil || c.Verb != VerbA || c.FormError != ErrCommandSeparator || c.Attest != nil {
			t.Errorf("%q: got %+v, want the form error", line, c)
		}
	}
	escaped := `orchestrate A --attest {"from":"P","to":"A","did":"a\u2028b"}`
	if c := ParseOrchestrateCommand(escaped); c == nil || c.FormError != "" || c.Attest == nil || c.Attest.Did != "a\u2028b" {
		t.Errorf("an escaped separator: %+v", c)
	}
	if c := ParseOrchestrateCommand("orchestrate A\u2028--attest {\"from\":\"P\",\"to\":\"A\"}"); c == nil || c.FormError != "" || c.Attest == nil {
		t.Errorf("a separator in the white space after the verb: %+v", c)
	}
	for _, prose := range []string{"please orchestrate A\u2028now", "orchestrate the release\u2028tomorrow", "we said orchestrate A --attest {\"x\":\"\u2028\"}"} {
		if c := ParseOrchestrateCommand(prose); c != nil {
			t.Errorf("%q: prose parsed as %+v", prose, c)
		}
	}
}

// The word-boundary check after --attest is reached only here: the public parser has refused such a line before.
func TestParseAttestTailDirect(t *testing.T) {
	if raw, att, msg := parseAttestTail("--attestx {}"); raw != nil || att != nil || msg != "" {
		t.Errorf("attestx: %v %v %q", raw, att, msg)
	}
	raw, att, msg := parseAttestTail("x --attest {\"from\":\"A\",\"to\":\"B\"}")
	if raw == nil || *raw != "{\"from\":\"A\",\"to\":\"B\"}" || att == nil || msg != "" {
		t.Errorf("non-zero index: %v %v %q", raw, att, msg)
	}
	if raw, _, _ := parseAttestTail("--attestx --attest {\"from\":\"A\",\"to\":\"B\"}"); raw == nil {
		t.Error("a later --attest after an --attestx is not found")
	}
	if raw, att, msg := parseAttestTail(""); raw != nil || att != nil || msg != "" {
		t.Errorf("empty: %v %v %q", raw, att, msg)
	}
}

// isJSSpace is the 25 code points of JavaScript's \s, written out here independently of text.Trim.
func TestIsJSSpaceIsTheJavaScriptSet(t *testing.T) {
	const set = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	if n := len([]rune(set)); n != 25 {
		t.Fatalf("set holds %d code points", n)
	}
	for r := rune(0); r < 0x10000; r++ {
		if got, want := isJSSpace(r), strings.ContainsRune(set, r); got != want {
			t.Fatalf("isJSSpace(%U) = %v, want %v", r, got, want)
		}
	}
}
