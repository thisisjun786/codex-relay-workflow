package cli_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1558 (U2). report-up's attribution line ("Reported by thread X, which is not the initiative's
// lead (Y).") is the channel's, not the reporter's words: it goes to the channel as the message's
// preface, so the message body still starts with it, and who reported a report does not make a new
// statement. A report the management seat acknowledged stays parked when the same report is made
// again by another thread or by the lead; a changed text releases it.

// proxyAttribution is the line report-up writes above a report made by thread reporter, which is not
// the initiative's lead.
func proxyAttribution(reporter, lead string) string {
	return "Reported by thread " + reporter + ", which is not the initiative's lead (" + lead + ")."
}

// cutText is the channel's cut of a message body to limit bytes, without splitting a character.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// parkedReport binds the management seat 01mgmtA over the lead 01lead of INI-1, reports lrp-proxy
// from thread first with args, and has 01mgmtA acknowledge it, so it is parked under
// superseded_by_report. It answers the store and the report's message id.
func parkedReport(t *testing.T, first string, args ...string) (*listStore, string) {
	t.Helper()
	s := newListStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", first)
	id := s.must(append([]string{"report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-proxy"}, args...)...)["messageId"].(string)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("supervisor-acknowledge", "--recipient", "01mgmtA", "--actor", "01mgmtA", "--all-informational")
	if row := s.message(id); row.HoldReason.String != store.SupervisorHoldSuperseded {
		t.Fatalf("the acknowledged report is held %q, want %s", row.HoldReason.String, store.SupervisorHoldSuperseded)
	}
	return s, id
}

// reportAgain reports lrp-proxy again from thread with args.
func reportAgain(t *testing.T, s *listStore, thread string, args ...string) {
	t.Helper()
	t.Setenv("CODEX_THREAD_ID", thread)
	s.must(append([]string{"report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-proxy"}, args...)...)
	t.Setenv("CODEX_THREAD_ID", "")
}

func messageBody(t *testing.T, s *listStore, id string) string {
	t.Helper()
	var packet map[string]any
	if err := json.Unmarshal([]byte(s.message(id).Packet), &packet); err != nil {
		t.Fatal(err)
	}
	body, _ := packet["body"].(string)
	return body
}

func TestReportUpProxyReportKeepsItsAcknowledgedParkWhoeverReportsIt(t *testing.T) {
	long := filepath.Join(t.TempDir(), "long.txt")
	longText := strings.Repeat("the delivery scope is complete; ", 7000/32+1)[:7000]
	if err := os.WriteFile(long, []byte(longText), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, first, second string
		args                []string
	}{
		{"another thread", "01someone", "01other", []string{"--subject", "scope", "--text", "the delivery scope is complete"}},
		{"a non-lead then the lead", "01someone", "01lead", []string{"--subject", "scope", "--text", "the delivery scope is complete"}},
		{"title only", "01someone", "01other", []string{"--subject", "the delivery scope is complete"}},
		{"a 7000-byte text", "01someone", "01other", []string{"--subject", "scope", "--text", "@" + long}},
		{"an indented text, a non-lead then the lead", "01someone", "01lead", []string{"--subject", "scope", "--text", "    the delivery scope is complete"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := parkedReport(t, tc.first, tc.args...)
			reportAgain(t, s, tc.second, tc.args...)
			if row := s.message(id); row.HoldReason.String != store.SupervisorHoldSuperseded || row.State != "queued" {
				t.Fatalf("the same report from %s after %s is %s held %q, want it still parked %s", tc.second, tc.first, row.State, row.HoldReason.String, store.SupervisorHoldSuperseded)
			}
		})
	}

	// The body is the bytes report-up has always staged: the attribution line, a blank line and the
	// text, trimmed and cut to 6000 bytes, so the management seat still reads who reported it first.
	t.Run("the body starts with the attribution", func(t *testing.T) {
		for _, text := range []string{"the delivery scope is complete", "   indented text", longText} {
			s := newListStore(t)
			t.Setenv("CODEX_THREAD_ID", "")
			s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
			s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
			t.Setenv("CODEX_THREAD_ID", "01someone")
			id := s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", "lrp-proxy", "--text", text)["messageId"].(string)
			t.Setenv("CODEX_THREAD_ID", "")
			body := messageBody(t, s, id)
			want := cutText(strings.TrimSpace(proxyAttribution("01someone", "01lead")+"\n\n"+text), 6000)
			got, base := sha256.Sum256([]byte(body)), sha256.Sum256([]byte(want))
			if !strings.HasPrefix(body, proxyAttribution("01someone", "01lead")+"\n\n") || got != base {
				t.Fatalf("body sha %s (%d bytes, %.80q), want %s, the base body", hex.EncodeToString(got[:]), len(body), body, hex.EncodeToString(base[:]))
			}
		}
	})

	t.Run("a changed text releases", func(t *testing.T) {
		s, id := parkedReport(t, "01someone", "--subject", "scope", "--text", "the delivery scope is complete")
		reportAgain(t, s, "01someone", "--subject", "scope", "--text", "the delivery scope is not complete")
		if row := s.message(id); row.HoldReason.String == store.SupervisorHoldSuperseded {
			t.Fatalf("a changed report is still held %q, want the park released", row.HoldReason.String)
		}
	})

	// Below the attribution the body keeps the text's first-line indentation, so a report whose
	// first line changed only there is another report; the same text again is not.
	t.Run("a first line indented differently releases", func(t *testing.T) {
		s, id := parkedReport(t, "01someone", "--subject", "scope", "--text", "the delivery scope is complete")
		reportAgain(t, s, "01other", "--subject", "scope", "--text", "the delivery scope is complete")
		if row := s.message(id); row.HoldReason.String != store.SupervisorHoldSuperseded {
			t.Fatalf("the same report is held %q, want it still parked", row.HoldReason.String)
		}
		reportAgain(t, s, "01other", "--subject", "scope", "--text", "    the delivery scope is complete")
		if row := s.message(id); row.HoldReason.String == store.SupervisorHoldSuperseded {
			t.Fatalf("a report whose first line was indented is still held %q, want the park released", row.HoldReason.String)
		}
		if body := messageBody(t, s, id); body != proxyAttribution("01other", "01lead")+"\n\n    the delivery scope is complete" {
			t.Fatalf("body %q, want the attribution and the indented text", body)
		}
	})
}
