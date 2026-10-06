package cli

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestDecodeCliAttestKeepsALoneSurrogate: decodeCliAttest checks the syntax with encoding/json and
// then reads the value with pyjson.Loads, so a lone-surrogate escape reaches Coerce as the three
// WTF-8 bytes the attest lowerJS keeps, where encoding/json alone would turn it into U+FFFD first.
func TestDecodeCliAttestKeepsALoneSurrogate(t *testing.T) {
	wtf8 := "\xed\xa0\x80" // U+D800 as WTF-8, the spelling pyjson.CodePoint reads back
	for _, tc := range []struct {
		name, doc, wantDid, wantVerdict string
	}{
		{"alone", `{"from":"P","to":"A","auditVerdict":"\ud800"}`, "", wtf8},
		{"between_ascii", `{"from":"P","to":"A","auditVerdict":"X\ud800Y"}`, "", "x" + wtf8 + "y"},
		{"did_is_not_lowered", `{"from":"P","to":"A","did":"a\ud800b"}`, "a" + wtf8 + "b", ""},
		{"surrogate_pair_stays_one_code_point", `{"from":"P","to":"A","auditVerdict":"\ud83d\ude00"}`, "", "\U0001F600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att, err := decodeCliAttest(tc.doc)
			if err != nil {
				t.Fatalf("decodeCliAttest: %v", err)
			}
			if att == nil {
				t.Fatal("attestation is nil")
			}
			if att.Did != tc.wantDid {
				t.Errorf("did = %q, want %q", att.Did, tc.wantDid)
			}
			if att.AuditVerdict != tc.wantVerdict {
				t.Errorf("auditVerdict = %q, want %q", att.AuditVerdict, tc.wantVerdict)
			}
		})
	}
}

// TestDecodeCliAttestKeepsTheSyntaxAndNumberReading: the first check is still encoding/json's, with
// its error text, and the value reading still gives Coerce what it had: a spelled number, an
// overflowing one dropped as Infinity, a non-object top level refused, and a repeated key keeping
// its last value.
func TestDecodeCliAttestKeepsTheSyntaxAndNumberReading(t *testing.T) {
	t.Run("syntax error is encoding json's", func(t *testing.T) {
		var raw json.RawMessage
		want := json.Unmarshal([]byte("{nope}"), &raw)
		_, got := decodeCliAttest("{nope}")
		var syntax *json.SyntaxError
		if got == nil || !errors.As(got, &syntax) || got.Error() != want.Error() {
			t.Errorf("error = %v, want encoding/json's %v", got, want)
		}
	})
	t.Run("numbers", func(t *testing.T) {
		att, err := decodeCliAttest(`{"from":"P","to":"A","auditRounds":2,"exitCode":1e999}`)
		if err != nil {
			t.Fatalf("decodeCliAttest: %v", err)
		}
		if att == nil || att.AuditRounds == nil || *att.AuditRounds != 2 {
			t.Fatalf("auditRounds = %+v, want 2", att)
		}
		if att.ExitCode != nil {
			t.Errorf("exitCode = %v, want nil for an overflowing number", *att.ExitCode)
		}
	})
	t.Run("shape", func(t *testing.T) {
		for _, doc := range []string{`["from","to"]`, `{"to":"A"}`} {
			if att, err := decodeCliAttest(doc); err != nil || att != nil {
				t.Errorf("decodeCliAttest(%q) = %+v, %v; want nil, nil", doc, att, err)
			}
		}
		if att, err := decodeCliAttest(`{"from":"P","to":"A","did":"first","did":"last"}`); err != nil || att == nil || att.Did != "last" {
			t.Errorf("repeated key = %+v, %v; want the last value", att, err)
		}
	})
}

// TestDecodeCliAttestStillCoercesAPlainAttestation is the regression pin for the ordinary document
// the parser reads today.
func TestDecodeCliAttestStillCoercesAPlainAttestation(t *testing.T) {
	att, err := decodeCliAttest(`{"from":"P","to":"A","did":"x y z","auditVerdict":"PASS","planUnit":"devlog/_plan/260101_x","workPhaseId":"wp1"}`)
	if err != nil {
		t.Fatalf("decodeCliAttest: %v", err)
	}
	if att == nil || att.Did != "x y z" || att.AuditVerdict != "pass" || att.PlanUnit != "devlog/_plan/260101_x" || att.WorkPhaseID != "wp1" {
		t.Errorf("attestation = %+v", att)
	}
}
