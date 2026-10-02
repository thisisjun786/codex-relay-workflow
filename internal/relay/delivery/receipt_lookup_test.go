package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The stored-receipt lookup is one implementation, LookupStoredReceipt (over a store the caller
// holds) and LookupStoredReceiptAt (over a path), and these tests state what it answers. Each
// expected answer is written out as the ordered object the guard returns, field by field, from the
// test's own constants, so a missing, extra or reordered field fails here and not only a changed
// evidence string.

// stagedReceipt is a registered relationship whose head event stands for a ready receipt over one
// artifact, in a store of its own, with the original stored texts kept to put back.
type stagedReceipt struct {
	f                                *fixture
	event, artifact, revision, stage string
	receipt, roots                   string
	artifactBytes                    []byte
}

func stageReceipt(t *testing.T) *stagedReceipt {
	t.Helper()
	f := newFixture(t, "")
	event := f.readyEvent(regOpts{})
	row := f.one("SELECT revision_hash, stage, receipt FROM events WHERE event_id = ?", event)
	artifact := f.root + "/out.txt"
	bytes, err := os.ReadFile(artifact)
	mustDo(t, err)
	return &stagedReceipt{f: f, event: event, artifact: artifact, revision: row.S("revision_hash"), stage: row.S("stage"),
		receipt: row.S("receipt"), roots: f.one("SELECT artifact_roots FROM relationships WHERE relationship_id = ?", f.rid).S("artifact_roots"), artifactBytes: bytes}
}

// query is what the Stop hook and the omission reader both ask of the staged turn.
func (s *stagedReceipt) query() ReceiptQuery {
	return ReceiptQuery{Relationship: s.f.rid, Session: child, Turn: dispatchTurn, Generation: int64(1), Dispatch: "dispatch-1"}
}

func (s *stagedReceipt) exec(query string, args ...any) {
	s.f.t.Helper()
	_, err := execSQL(s.f.ctx, s.f.store, query, args...)
	mustDo(s.f.t, err)
}

func (s *stagedReceipt) setReceipt(text string) {
	s.f.t.Helper()
	s.exec("UPDATE events SET receipt = ? WHERE event_id = ?", text, s.event)
}

func (s *stagedReceipt) setRoots(text string) {
	s.f.t.Helper()
	s.exec("UPDATE relationships SET artifact_roots = ? WHERE relationship_id = ?", text, s.f.rid)
}

// restore puts back what a case rewrote: the stored receipt, the roots and the artifact's bytes.
func (s *stagedReceipt) restore() {
	s.f.t.Helper()
	s.setReceipt(s.receipt)
	s.setRoots(s.roots)
	mustDo(s.f.t, os.WriteFile(s.artifact, s.artifactBytes, 0o644))
}

// answer is the guard's answer for the staged turn: the four identity fields, the evidence, then
// what the evidence carries.
func (s *stagedReceipt) answer(atHead bool, evidence string, extra ...F) Obj {
	return append(Obj{{Key: "relationshipId", Value: s.f.rid}, {Key: "sessionId", Value: child}, {Key: "turnId", Value: dispatchTurn}, {Key: "atCurrentHead", Value: atHead}, {Key: "evidence", Value: evidence}}, extra...)
}

func (s *stagedReceipt) atHead() Obj {
	return s.answer(true, "at_head", F{Key: "eventId", Value: s.event}, F{Key: "revisionHash", Value: s.revision}, F{Key: "stage", Value: s.stage}, F{Key: "deliverableBinding", Value: "live"})
}

func (s *stagedReceipt) changed(detail string) Obj {
	return s.answer(false, "artifacts_changed_since_receipt", F{Key: "detail", Value: detail}, F{Key: "eventId", Value: s.event}, F{Key: "revisionHash", Value: s.revision})
}

func (s *stagedReceipt) unreadable(detail string) Obj {
	return s.answer(false, "stored_receipt_unreadable", F{Key: "detail", Value: detail}, F{Key: "eventId", Value: s.event})
}

// unverifiable is the answer of a comparison that could not happen: not readable.
func (s *stagedReceipt) unverifiable(detail string) Obj {
	return s.answer(false, "deliverable_unverifiable", F{Key: "detail", Value: detail})
}

type lookedUp struct {
	answer   Obj
	readable bool
	err      error
}

func equalObj(got, want Obj) bool { return reflect.DeepEqual(got, want) }

// both asks the same question of both entry points, over the staged store: through the store the
// fixture holds and through its path.
func (s *stagedReceipt) both(ctx context.Context, q ReceiptQuery) (viaStore, viaPath lookedUp) {
	s.f.t.Helper()
	viaStore.answer, viaStore.readable, viaStore.err = LookupStoredReceipt(ctx, s.f.store, q)
	viaPath.answer, viaPath.readable, viaPath.err = LookupStoredReceiptAt(ctx, s.f.store.Path, nil, time.Second, q)
	return viaStore, viaPath
}

// expect requires both entry points to answer exactly want.
func (s *stagedReceipt) expect(ctx context.Context, q ReceiptQuery, want Obj, readable bool) {
	s.f.t.Helper()
	viaStore, viaPath := s.both(ctx, q)
	for name, got := range map[string]lookedUp{"LookupStoredReceipt": viaStore, "LookupStoredReceiptAt": viaPath} {
		if got.err != nil {
			s.f.t.Errorf("%s: error %v", name, got.err)
		}
		if got.readable != readable {
			s.f.t.Errorf("%s: readable = %v, want %v", name, got.readable, readable)
		}
		if !reflect.DeepEqual(got.answer, want) {
			s.f.t.Errorf("%s answered\n %v\nwant\n %v", name, got.answer, want)
		}
	}
}

// storedCase rewrites what a hand-edited store would hold and states the answer owed.
type storedCase struct {
	name string
	// edit returns the stored receipt text; roots, when set, replaces the stored artifact roots.
	edit  func(t *testing.T, s *stagedReceipt, stored string) string
	roots string
	// want is the whole answer, readable unless unverifiable.
	want         func(s *stagedReceipt) Obj
	unverifiable bool
}

func swapText(t *testing.T, stored, old, with string) string {
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

func storedCases() []storedCase {
	field := func(name, value string) func(*testing.T, *stagedReceipt, string) string {
		return func(t *testing.T, s *stagedReceipt, stored string) string { return swapText(t, stored, name, value) }
	}
	at := func(s *stagedReceipt) Obj { return s.atHead() }
	changedAs := func(detail func(s *stagedReceipt) string) func(*stagedReceipt) Obj {
		return func(s *stagedReceipt) Obj { return s.changed(detail(s)) }
	}
	changedText := func(detail string) func(*stagedReceipt) Obj {
		return changedAs(func(*stagedReceipt) string { return detail })
	}
	size := func(claims string) func(*stagedReceipt) Obj {
		return changedAs(func(s *stagedReceipt) string { return s.artifact + ": size 15 but the manifest claims " + claims })
	}
	pathEdit := func(with string) func(*testing.T, *stagedReceipt, string) string {
		return func(t *testing.T, s *stagedReceipt, stored string) string {
			return swapText(t, stored, "\"path\": \""+s.artifact+"\"", "\"path\": "+with)
		}
	}
	unreadableAs := func(detail string) func(*stagedReceipt) Obj {
		return func(s *stagedReceipt) Obj { return s.unreadable(detail) }
	}
	unverifiableAs := func(detail string) func(*stagedReceipt) Obj {
		return func(s *stagedReceipt) Obj { return s.unverifiable(detail) }
	}
	// refused is what the revision hash says of an entry whose digest is not a digest.
	refused := func(digest string) func(*stagedReceipt) Obj {
		return func(s *stagedReceipt) Obj {
			return s.unverifiable("ScopeError: manifest_unverified: entry '" + s.artifact + "' has a digest that is not 64 lowercase hex characters: " + digest)
		}
	}
	digestOf := func(text string) string { sum := sha256.Sum256([]byte(text)); return hex.EncodeToString(sum[:]) }
	digestEdit := func(with string) func(*testing.T, *stagedReceipt, string) string {
		return func(t *testing.T, s *stagedReceipt, stored string) string {
			return strings.Replace(stored, "\"sha256\": \"", "\"sha256\": "+with+", \"x\": \"", 1)
		}
	}
	return []storedCase{
		{name: "normal", want: at},
		{name: "bytes-integer-valued-float", edit: field("\"bytes\": 15}", "\"bytes\": 15.0}"), want: at},
		{name: "bytes-numeric-string", edit: field("\"bytes\": 15}", "\"bytes\": \"15\"}"), want: size("15")},
		{name: "bytes-true", edit: field("\"bytes\": 15}", "\"bytes\": true}"), want: size("True")},
		{name: "bytes-list", edit: field("\"bytes\": 15}", "\"bytes\": [15]}"), want: size("[15]")},
		{name: "bytes-nan", edit: field("\"bytes\": 15}", "\"bytes\": NaN}"), want: size("nan")},
		{name: "bytes-past-int64", edit: field("\"bytes\": 15}", "\"bytes\": 99999999999999999999}"), want: size("99999999999999999999")},
		{name: "path-number", edit: pathEdit("5"), want: changedText("AttributeError: 'int' object has no attribute 'encode'")},
		{name: "path-list", edit: pathEdit("[\"x\"]"), want: changedText("AttributeError: 'list' object has no attribute 'encode'")},
		{name: "path-null", edit: pathEdit("null"), want: changedText("AttributeError: 'NoneType' object has no attribute 'encode'")},
		{name: "path-with-a-lone-surrogate", edit: pathEdit("\"\\ud800x\""),
			want: changedText("UnicodeEncodeError: 'utf-8' codec can't encode character '\\ud800' in position 0: surrogates not allowed")},
		// json.loads keeps the last of a repeated key, and a null in the last place is a path of None.
		{name: "path-repeated-and-null", edit: field("\"bytes\": 15}", "\"bytes\": 15, \"path\": null}"), want: changedText("AttributeError: 'NoneType' object has no attribute 'encode'")},
		{name: "record-null", edit: func(t *testing.T, s *stagedReceipt, stored string) string { return manifestOf(t, stored, "[null]") },
			want: changedText("TypeError: 'NoneType' object is not subscriptable")},
		// A field no typed decoder would read, holding an integer json.loads refuses past 4300 digits.
		{name: "integer-past-the-digit-limit-in-an-unread-field", edit: field("\"attempt\": 1", "\"attempt\": 1"+strings.Repeat("0", 4300)),
			want: unreadableAs("JSONDecodeError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit")},
		// A path the revision hash refuses (not absolute) is a refusal, as a digest that is not a digest is.
		{name: "path-relative", edit: pathEdit("\"relative/out.txt\""), unverifiable: true, want: unverifiableAs("ScopeError: scope_escape: path must be absolute: 'relative/out.txt'")},
		{name: "key-spelled-in-another-case", edit: field("\"manifest\": [", "\"Manifest\": ["), want: changedText("the stored receipt carries no manifest to verify")},
		{name: "digest-null", edit: digestEdit("null"), unverifiable: true, want: refused("None")},
		{name: "digest-number", edit: digestEdit("5"), unverifiable: true, want: refused("5")},
		{name: "digest-not-hex", edit: digestEdit("\"xyz\""), unverifiable: true, want: refused("'xyz'")},
		{name: "record-a-list", edit: func(t *testing.T, s *stagedReceipt, stored string) string { return manifestOf(t, stored, "[[1]]") },
			want: changedText("TypeError: list indices must be integers or slices, not str")},
		{name: "record-without-path", edit: func(t *testing.T, s *stagedReceipt, stored string) string {
			return manifestOf(t, stored, "[{\"sha256\": \""+strings.Repeat("a", 64)+"\", \"bytes\": 15}]")
		}, want: changedText("KeyError: 'path'")},
		{name: "record-without-digest", edit: func(t *testing.T, s *stagedReceipt, stored string) string {
			return manifestOf(t, stored, "[{\"path\": \""+s.artifact+"\", \"bytes\": 15}]")
		}, want: changedText("KeyError: 'sha256'")},
		{name: "manifest-text", edit: func(t *testing.T, s *stagedReceipt, stored string) string { return manifestOf(t, stored, "\"x\"") },
			want: changedText("the stored receipt carries no manifest to verify")},
		{name: "manifest-empty", edit: func(t *testing.T, s *stagedReceipt, stored string) string { return manifestOf(t, stored, "[]") },
			want: changedText("the stored receipt carries no manifest to verify")},
		{name: "receipt-a-list", edit: func(*testing.T, *stagedReceipt, string) string { return "[1]" },
			want: changedText("AttributeError: 'list' object has no attribute 'get'")},
		{name: "revision-blank", edit: func(t *testing.T, s *stagedReceipt, stored string) string {
			i := strings.Index(stored, "\"revisionHash\": \"")
			return stored[:i] + "\"revisionHash\": \"\"" + stored[i+len("\"revisionHash\": \"")+65:]
		}, want: changedText("the stored receipt names no revision")},
		{name: "revision-another", edit: func(t *testing.T, s *stagedReceipt, stored string) string {
			i := strings.Index(stored, "\"revisionHash\": \"") + len("\"revisionHash\": \"")
			return stored[:i] + strings.Repeat("0", 64) + stored[i+64:]
		}, want: changedAs(func(s *stagedReceipt) string {
			return "the stored manifest hashes to " + s.revision + " but the receipt claims " + strings.Repeat("0", 64)
		})},
		{name: "live-bytes-changed", edit: func(t *testing.T, s *stagedReceipt, stored string) string {
			mustDo(t, os.WriteFile(s.artifact, []byte("a later revision"), 0o600))
			return stored
		}, want: changedAs(func(s *stagedReceipt) string {
			return s.artifact + ": bytes hash to " + digestOf("a later revision") + " but the manifest claims " + digestOf("the deliverable")
		})},
		{name: "roots-an-object", roots: "{}", want: changedText("TypeError: artifact roots are not a list")},
		{name: "roots-null", roots: "null", want: changedText("TypeError: artifact roots are not a list")},
		{name: "roots-a-number-in-a-list", roots: "[5]", want: changedText("TypeError: artifact root is not a string")},
		{name: "roots-null-element", roots: "[null]", want: changedText("TypeError: artifact root is not a string")},
		{name: "receipt-not-json", edit: func(*testing.T, *stagedReceipt, string) string { return "{\"manifest\": [" },
			want: unreadableAs("JSONDecodeError: Expecting value: line 1 column 15 (char 14)")},
		{name: "receipt-empty", edit: func(*testing.T, *stagedReceipt, string) string { return "" },
			want: unreadableAs("JSONDecodeError: Expecting value: line 1 column 1 (char 0)")},
		{name: "roots-not-json", roots: "[", want: unreadableAs("JSONDecodeError: Expecting value: line 1 column 2 (char 1)")},
	}
}

// A stored receipt is judged by the values it holds: a path, digest or byte count of another type
// is compared, printed and refused as the guard refuses it, and only text that is not JSON is
// unreadable. One staged store serves every case; each case rewrites what a hand-edited store
// would hold, both entry points must give the same whole answer, and what the case rewrote is put
// back before the next one, with the normal receipt asked again at the end.
func TestStoredReceiptIsJudgedByTheValuesItHolds(t *testing.T) {
	ctx := context.Background()
	s := stageReceipt(t)
	for _, c := range storedCases() {
		t.Run(c.name, func(t *testing.T) {
			s.f.t = t
			defer s.restore()
			stored := s.receipt
			if c.edit != nil {
				if edited := c.edit(t, s, stored); edited != stored {
					s.setReceipt(edited)
				}
			}
			if c.roots != "" {
				s.setRoots(c.roots)
			}
			s.expect(ctx, s.query(), c.want(s), !c.unverifiable)
		})
	}
	s.f.t = t
	s.expect(ctx, s.query(), s.atHead(), true)
}
