package delivery_test

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// The omission reader and the Stop hook read the one stored receipt of a head event, and they
// judge it by one rule: the records it holds are the values json.loads made of them, so a path,
// digest or byte count of another type is compared, printed and refused as the guard refuses it
// rather than being a receipt nobody can read. Only a document that is not JSON is unreadable.
// Each case replaces what a hand-edited store would hold, and states the answer both readers owe.
type storedValueCase struct {
	name string
	// edit returns the stored receipt text; roots, when set, replaces the stored artifact roots.
	edit  func(t *testing.T, s *delivery.ReceiptStage, stored string) string
	roots string
	// evidence and detail are the answer. A hook that could not compare (unverifiable) leaves the
	// omission without a receipt (unverifiable, whose failure reason is receipt_unreadable).
	evidence     string
	detail       string
	unverifiable bool
}

func swap(t *testing.T, stored, old, with string) string {
	t.Helper()
	if !strings.Contains(stored, old) {
		t.Fatalf("the stored receipt does not hold %q: %s", old, stored)
	}
	return strings.Replace(stored, old, with, 1)
}

// manifestOf replaces the receipt's manifest value, which runs from its key to emittedAt.
func manifestOf(t *testing.T, stored, manifest string) string {
	t.Helper()
	const key, end = "\"manifest\": ", ", \"emittedAt\""
	i, j := strings.Index(stored, key), strings.Index(stored, end)
	if i < 0 || j < i {
		t.Fatalf("no manifest in %s", stored)
	}
	return stored[:i+len(key)] + manifest + stored[j:]
}

func storedValueCases() []storedValueCase {
	field := func(name, value string) func(*testing.T, *delivery.ReceiptStage, string) string {
		return func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return swap(t, stored, name, value)
		}
	}
	return []storedValueCase{
		{name: "normal", edit: func(_ *testing.T, _ *delivery.ReceiptStage, stored string) string { return stored }, evidence: "at_head"},
		{name: "bytes-integer-valued-float", edit: field("\"bytes\": 15}", "\"bytes\": 15.0}"), evidence: "at_head"},
		{name: "bytes-numeric-string", edit: field("\"bytes\": 15}", "\"bytes\": \"15\"}"),
			evidence: "artifacts_changed_since_receipt", detail: "size 15 but the manifest claims 15"},
		{name: "bytes-true", edit: field("\"bytes\": 15}", "\"bytes\": true}"),
			evidence: "artifacts_changed_since_receipt", detail: "size 15 but the manifest claims True"},
		{name: "bytes-list", edit: field("\"bytes\": 15}", "\"bytes\": [15]}"),
			evidence: "artifacts_changed_since_receipt", detail: "size 15 but the manifest claims [15]"},
		{name: "bytes-nan", edit: field("\"bytes\": 15}", "\"bytes\": NaN}"),
			evidence: "artifacts_changed_since_receipt", detail: "size 15 but the manifest claims nan"},
		{name: "bytes-past-int64", edit: field("\"bytes\": 15}", "\"bytes\": 99999999999999999999}"),
			evidence: "artifacts_changed_since_receipt", detail: "size 15 but the manifest claims 99999999999999999999"},
		{name: "path-number", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return swap(t, stored, "\"path\": \""+s.Artifact+"\"", "\"path\": 5")
		}, evidence: "artifacts_changed_since_receipt", detail: "AttributeError: 'int' object has no attribute 'encode'"},
		{name: "path-list", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return swap(t, stored, "\"path\": \""+s.Artifact+"\"", "\"path\": [\"x\"]")
		}, evidence: "artifacts_changed_since_receipt", detail: "AttributeError: 'list' object has no attribute 'encode'"},
		{name: "path-null", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return swap(t, stored, "\"path\": \""+s.Artifact+"\"", "\"path\": null")
		}, evidence: "artifacts_changed_since_receipt", detail: "AttributeError: 'NoneType' object has no attribute 'encode'"},
		{name: "path-with-a-lone-surrogate", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return swap(t, stored, "\"path\": \""+s.Artifact+"\"", `"path": "\ud800x"`)
		}, evidence: "artifacts_changed_since_receipt", detail: `UnicodeEncodeError: 'utf-8' codec can't encode character '\ud800' in position 0: surrogates not allowed`},
		// json.loads keeps the last of a repeated key, and a null in the last place is a path of None.
		{name: "path-repeated-and-null", edit: field("\"bytes\": 15}", "\"bytes\": 15, \"path\": null}"),
			evidence: "artifacts_changed_since_receipt", detail: "AttributeError: 'NoneType' object has no attribute 'encode'"},
		{name: "record-null", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "[null]")
		}, evidence: "artifacts_changed_since_receipt", detail: "TypeError: 'NoneType' object is not subscriptable"},
		// A field the typed decoder never read, holding an integer json.loads refuses past 4300 digits.
		{name: "integer-past-the-digit-limit-in-an-unread-field", edit: field("\"attempt\": 1", "\"attempt\": 1"+strings.Repeat("0", 4300)),
			evidence: "stored_receipt_unreadable", detail: "JSONDecodeError: Exceeds the limit (4300 digits)"},
		{name: "path-relative", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			// A path the revision hash refuses (not absolute) is a refusal, as a digest that is not a
			// digest is: neither reader can compare it.
			return swap(t, stored, "\"path\": \""+s.Artifact+"\"", "\"path\": \"relative/out.txt\"")
		}, unverifiable: true},
		{name: "key-spelled-in-another-case", edit: field("\"manifest\": [", "\"Manifest\": ["),
			evidence: "artifacts_changed_since_receipt", detail: "the stored receipt carries no manifest to verify"},
		{name: "digest-null", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return strings.Replace(stored, "\"sha256\": \"", "\"sha256\": null, \"x\": \"", 1)
		}, unverifiable: true},
		{name: "digest-number", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return strings.Replace(stored, "\"sha256\": \"", "\"sha256\": 5, \"x\": \"", 1)
		}, unverifiable: true},
		{name: "digest-not-hex", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return strings.Replace(stored, "\"sha256\": \"", "\"sha256\": \"xyz\", \"x\": \"", 1)
		}, unverifiable: true},
		{name: "record-a-list", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "[[1]]")
		}, evidence: "artifacts_changed_since_receipt", detail: "TypeError: list indices must be integers or slices, not str"},
		{name: "record-without-path", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "[{\"sha256\": \""+strings.Repeat("a", 64)+"\", \"bytes\": 15}]")
		}, evidence: "artifacts_changed_since_receipt", detail: "KeyError: 'path'"},
		{name: "record-without-digest", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "[{\"path\": \""+s.Artifact+"\", \"bytes\": 15}]")
		}, evidence: "artifacts_changed_since_receipt", detail: "KeyError: 'sha256'"},
		{name: "manifest-text", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "\"x\"")
		}, evidence: "artifacts_changed_since_receipt", detail: "the stored receipt carries no manifest to verify"},
		{name: "manifest-empty", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			return manifestOf(t, stored, "[]")
		}, evidence: "artifacts_changed_since_receipt", detail: "the stored receipt carries no manifest to verify"},
		{name: "receipt-a-list", edit: func(*testing.T, *delivery.ReceiptStage, string) string { return "[1]" },
			evidence: "artifacts_changed_since_receipt", detail: "AttributeError: 'list' object has no attribute 'get'"},
		{name: "revision-blank", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			i := strings.Index(stored, "\"revisionHash\": \"")
			return stored[:i] + "\"revisionHash\": \"\"" + stored[i+len("\"revisionHash\": \"")+65:]
		}, evidence: "artifacts_changed_since_receipt", detail: "the stored receipt names no revision"},
		{name: "revision-another", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			i := strings.Index(stored, "\"revisionHash\": \"") + len("\"revisionHash\": \"")
			return stored[:i] + strings.Repeat("0", 64) + stored[i+64:]
		}, evidence: "artifacts_changed_since_receipt", detail: "the stored manifest hashes to"},
		{name: "live-bytes-changed", edit: func(t *testing.T, s *delivery.ReceiptStage, stored string) string {
			if err := os.WriteFile(s.Artifact, []byte("a later revision"), 0o600); err != nil {
				t.Fatal(err)
			}
			return stored
		}, evidence: "artifacts_changed_since_receipt", detail: "bytes hash to"},
		{name: "roots-an-object", roots: "{}", evidence: "artifacts_changed_since_receipt", detail: "TypeError: artifact roots are not a list"},
		{name: "roots-null", roots: "null", evidence: "artifacts_changed_since_receipt", detail: "TypeError: artifact roots are not a list"},
		{name: "roots-a-number-in-a-list", roots: "[5]", evidence: "artifacts_changed_since_receipt", detail: "TypeError: artifact root is not a string"},
		{name: "roots-null-element", roots: "[null]", evidence: "artifacts_changed_since_receipt", detail: "TypeError: artifact root is not a string"},
		{name: "receipt-not-json", edit: func(*testing.T, *delivery.ReceiptStage, string) string { return "{\"manifest\": [" },
			evidence: "stored_receipt_unreadable", detail: "JSONDecodeError: "},
		{name: "receipt-empty", edit: func(*testing.T, *delivery.ReceiptStage, string) string { return "" },
			evidence: "stored_receipt_unreadable", detail: "JSONDecodeError: Expecting value"},
		{name: "roots-not-json", roots: "[", evidence: "stored_receipt_unreadable", detail: "JSONDecodeError: "},
	}
}

func TestOmissionReaderJudgesStoredReceiptAsTheStopHookDoes(t *testing.T) {
	ctx := context.Background()
	for _, c := range storedValueCases() {
		t.Run(c.name, func(t *testing.T) {
			s := delivery.StageReceipt(t)
			stored := s.Receipt()
			if c.edit != nil {
				if edited := c.edit(t, s, stored); edited != stored {
					s.SetReceipt(edited)
				}
			}
			if c.roots != "" {
				s.SetRoots(c.roots)
			}
			omission, omissionErr := s.OmissionReceipt(ctx)
			stop, readable, stopErr := hook.LookupReceipt(ctx, s.Path, nil, s.Relationship, s.Session, s.Turn, s.Generation, s.Dispatch)
			if stopErr != nil {
				t.Fatalf("the Stop hook faulted: %v", stopErr)
			}

			if c.unverifiable {
				// Neither reader could compare: the hook's lookup is unreadable and the omission is
				// left without a receipt, unmeasured as receipt_unreadable.
				if readable || omissionErr == nil {
					t.Fatalf("hook readable=%v omission err=%v, want neither to compare", readable, omissionErr)
				}
				if got := delivery.OmissionReceiptFailure(omissionErr); got != "receipt_unreadable" {
					t.Fatalf("the omission's failure = %q", got)
				}
				if evidence, _ := lookup(stop, "evidence"); evidence != "deliverable_unverifiable" {
					t.Fatalf("hook evidence = %v", evidence)
				}
				return
			}
			if omissionErr != nil {
				t.Fatalf("the omission reader could not read the receipt: %v", omissionErr)
			}
			if !readable {
				t.Fatalf("the Stop hook could not read the receipt: %v", stop)
			}
			if evidence, _ := lookup(omission, "evidence"); evidence != c.evidence {
				t.Fatalf("omission evidence = %v, want %s (receipt %v)", evidence, c.evidence, omission)
			}
			if detail, _ := lookup(omission, "detail"); !strings.Contains(detailText(detail), c.detail) {
				t.Fatalf("omission detail = %v, want it to hold %q", detail, c.detail)
			}
			// Both readers name the same receipt, answer and fields, in the same order.
			if !reflect.DeepEqual(omission, stop) {
				t.Fatalf("the readers disagree:\n omission %v\n hook     %v", omission, stop)
			}
		})
	}
}

func lookup(o []delivery.F, key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

func detailText(v any) string { s, _ := v.(string); return s }

// The generation a registration names is compared with the relationship's current one as the guard
// compares it: Python's != on the decoded value, so a bool is 0 or 1, an integer-valued float is that
// integer, and a string or a fractional float is never the integer. Both readers answer alike, and
// the mismatch names the registered value as str() spells it.
func TestStoredReceiptGenerationIsComparedAsTheGuardDoes(t *testing.T) {
	ctx := context.Background()
	s := delivery.StageReceipt(t)
	mismatch := func(registered string) string {
		return "the assignment registered generation " + registered + " and the relationship now stands on generation 1"
	}
	for _, c := range []struct {
		name       string
		generation any
		evidence   string
		detail     string
	}{
		{"integer-equal", int64(1), "at_head", ""},
		{"integer-different", int64(2), "registration_generation_mismatch", mismatch("2")},
		{"true-is-one", true, "at_head", ""},
		{"false-is-zero", false, "registration_generation_mismatch", mismatch("False")},
		{"integral-float", 1.0, "at_head", ""},
		{"integral-float-different", 2.0, "registration_generation_mismatch", mismatch("2.0")},
		{"fractional-float", 1.5, "registration_generation_mismatch", mismatch("1.5")},
		{"numeric-string-is-not-the-integer", "1", "registration_generation_mismatch", mismatch("1")},
		{"no-stamp", nil, "at_head", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			omission, err := s.OmissionReceiptFor(ctx, c.generation)
			if err != nil {
				t.Fatalf("the omission reader could not read the receipt: %v", err)
			}
			stop, readable, err := hook.LookupReceipt(ctx, s.Path, nil, s.Relationship, s.Session, s.Turn, c.generation, s.Dispatch)
			if err != nil || !readable {
				t.Fatalf("the Stop hook: readable=%v err=%v", readable, err)
			}
			for name, got := range map[string][]delivery.F{"omission": omission, "hook": stop} {
				if evidence, _ := lookup(got, "evidence"); evidence != c.evidence {
					t.Errorf("%s evidence = %v, want %s", name, evidence, c.evidence)
				}
				if detail, _ := lookup(got, "detail"); c.detail != "" && detailText(detail) != c.detail {
					t.Errorf("%s detail = %v, want %q", name, detail, c.detail)
				}
			}
			if !reflect.DeepEqual(omission, stop) {
				t.Errorf("the readers disagree:\n omission %v\n hook     %v", omission, stop)
			}
		})
	}
}

// hook.DeliverableState is the rule the omission reader now shares: for the same stored receipt it
// names the state, binding and detail the omission's lookup carries.
func TestOmissionReaderCarriesDeliverableState(t *testing.T) {
	ctx := context.Background()
	for _, c := range storedValueCases() {
		if c.evidence == "stored_receipt_unreadable" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			s := delivery.StageReceipt(t)
			stored := s.Receipt()
			if c.edit != nil {
				stored = c.edit(t, s, stored)
			}
			roots := s.Roots()
			if c.roots != "" {
				roots = c.roots
			}
			receipt, err := hook.Decode([]byte(stored))
			if err != nil {
				t.Fatal(err)
			}
			rootsValue, err := hook.Decode([]byte(roots))
			if err != nil {
				t.Fatal(err)
			}
			state, binding, detail, raised := hook.DeliverableState(ctx, receipt, s.ManifestRef, rootsValue)
			if raised != nil {
				t.Fatalf("hook.DeliverableState raised %v", raised)
			}
			s.SetReceipt(stored)
			s.SetRoots(roots)
			omission, omissionErr := s.OmissionReceipt(ctx)
			switch state {
			case "unverifiable":
				if omissionErr == nil {
					t.Fatalf("hook.DeliverableState is unverifiable (%s) and the omission answered %v", detail, omission)
				}
			case "changed":
				got, _ := lookup(omission, "evidence")
				gotDetail, _ := lookup(omission, "detail")
				if omissionErr != nil || got != "artifacts_changed_since_receipt" || gotDetail != detail {
					t.Fatalf("hook.DeliverableState is changed (%s) and the omission answered %v, %v", detail, omission, omissionErr)
				}
			case "current":
				got, _ := lookup(omission, "deliverableBinding")
				if omissionErr != nil || got != binding {
					t.Fatalf("hook.DeliverableState is current (%s) and the omission answered %v, %v", binding, omission, omissionErr)
				}
			default:
				t.Fatalf("state %q", state)
			}
		})
	}
}
