package managed

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Codex 0.154 reports a thread's permission profile as an object, {"id": ..., "extends": ...}. The
// relay compares what a resume reports with the record's expectedPermissionProfile as one whole value,
// so a task running under a custom profile can be delivered to only when its record holds that same
// object. managed-start used to take the field as text and nothing else, which left such a task with
// no record it could be started under. These tests hold the object form: accepted with a bounded shape,
// recorded as the request gave it, verified by the host's own reports, and never copied from the
// creation response, which the request's value is verified against instead.

const (
	profileTrusted = `{"id": "trusted", "extends": null}`
	profileFlipped = `{"extends": null, "id": "trusted"}`
	profileRecord  = `"expectedPermissionProfile": {"extends": null, "id": "trusted"}`
	// The fixture's sandbox is workspaceWrite, so this is the built-in profile a host reports for it.
	profileBuiltin = `{"id": ":workspace", "extends": null}`
)

// profileHost is the fake host answering a creation the way the host does: the profile it reports is
// its own value, set apart from what the request named. managedFake copies the request's settings into
// the creation response, which would hide a creation that reports a different profile or none.
type profileHost struct {
	*managedFake
	reported string
}

func (h *profileHost) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	receipt, err := h.managedFake.CreateThread(ctx, in)
	if err != nil {
		return nil, err
	}
	created := obj(receipt["creation"])
	delete(created, "expectedPermissionProfile")
	if h.reported != "" {
		created["activePermissionProfile"] = jsonValue(h.reported)
	}
	return receipt, nil
}

func jsonValue(text string) any {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		panic(err)
	}
	return value
}

// profileRequest is base with requested as both roles' expectedPermissionProfile, spelled as given
// (key order included); "" leaves the key out.
func profileRequest(t *testing.T, base []byte, requested string) []byte {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(base, &request); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"parent", "child"} {
		settings := obj(obj(request[role])["settings"])
		delete(settings, "expectedPermissionProfile")
		if requested != "" {
			settings["expectedPermissionProfile"] = json.RawMessage(requested)
		}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type profileRun struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	host  *profileHost
	start *Start
}

// newProfileRun is one managed start over a fresh store whose host reports reported at creation.
func newProfileRun(t *testing.T, base []byte, reported string) *profileRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	parsed, err := ParseRequest(base)
	if err != nil {
		t.Fatal(err)
	}
	fake := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(parsed["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	host := &profileHost{managedFake: fake, reported: reported}
	return &profileRun{t: t, ctx: ctx, store: s, host: host, start: &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}}
}

func (x *profileRun) run(raw []byte) map[string]any {
	x.t.Helper()
	result, err := x.start.Run(x.ctx, raw)
	if err != nil {
		x.t.Fatalf("managed start refused: %v", err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	return got
}

// stored is the settings text the store holds for task, byte for byte; "" when it holds none.
func (x *profileRun) stored(task string) string {
	x.t.Helper()
	var text string
	err := x.store.DB.QueryRowContext(x.ctx, "SELECT settings FROM authorized_settings WHERE task_id = ?", task).Scan(&text)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		x.t.Fatal(err)
	}
	return text
}

func (x *profileRun) count(table string) int {
	x.t.Helper()
	var n int
	if err := x.store.DB.QueryRowContext(x.ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		x.t.Fatal(err)
	}
	return n
}

// storedProfile is the expectedPermissionProfile the store's record of task decodes to, and whether it names one.
func (x *profileRun) storedProfile(task string) (any, bool) {
	x.t.Helper()
	var settings map[string]any
	if err := json.Unmarshal([]byte(x.stored(task)), &settings); err != nil {
		x.t.Fatal(err)
	}
	profile, ok := settings["expectedPermissionProfile"]
	return profile, ok
}

// A request naming a custom profile object for both roles is accepted, and each role's record holds
// that object: the keys the request named, extends: null kept as a null, an absent extends left absent.
// The stored text spells the object as every other nested value of a record is spelled, its keys
// sorted, so two requests that differ only in the order of the profile's keys record the same bytes.
func TestPermissionProfileObjectIsRecordedAsRequested(t *testing.T) {
	base := requestFixture(t)
	for _, requested := range []string{
		profileTrusted,
		profileFlipped,
		`{"id": "trusted", "extends": ":workspace"}`,
		`{"id": "trusted"}`,
		`{"id": "trusted", "extends": null, "label": "r\u00e9sum\u00e9 \"q\"", "readOnly": true, "unset": null}`,
	} {
		t.Run(requested, func(t *testing.T) {
			x := newProfileRun(t, base, requested)
			got := x.run(profileRequest(t, base, requested))
			if got["state"] != "admitted" || x.host.created != 1 || x.host.sent != 1 {
				t.Fatalf("custom profile start: %v effects %d/%d", got, x.host.created, x.host.sent)
			}
			for _, task := range []string{"parent", "child-new"} {
				profile, ok := x.storedProfile(task)
				if !ok || !reflect.DeepEqual(profile, jsonValue(requested)) {
					t.Fatalf("%s's record holds %v (named: %v), the request named %s: %s", task, profile, ok, requested, x.stored(task))
				}
			}
		})
	}
	first := newProfileRun(t, base, profileTrusted)
	first.run(profileRequest(t, base, profileTrusted))
	second := newProfileRun(t, base, profileFlipped)
	second.run(profileRequest(t, base, profileFlipped))
	for _, task := range []string{"parent", "child-new"} {
		if first.stored(task) != second.stored(task) || !strings.Contains(first.stored(task), profileRecord) {
			t.Fatalf("%s's record depends on the key order of the profile:\n%s\n%s", task, first.stored(task), second.stored(task))
		}
	}
}

func findingCodes(found []contract.OrderedObject) []string {
	codes := []string{}
	for _, f := range found {
		codes = append(codes, str(field(f, "code")))
	}
	return codes
}

// The record managed-start wrote is what a resume is checked against. A host reporting the recorded
// object, in either key order, leaves nothing to mismatch; any other profile is
// unverifiable_permission_profile, and one the answer leaves out is unobservable. An id alone does not
// match a record that holds extends: null, because an absent extends and a null one are different objects.
func TestPermissionProfileResumeIsCheckedAgainstTheRecordedObject(t *testing.T) {
	base := requestFixture(t)
	x := newProfileRun(t, base, profileTrusted)
	if got := x.run(profileRequest(t, base, profileTrusted)); got["state"] != "admitted" {
		t.Fatalf("custom profile start: %v", got)
	}
	reg := &registry.Registry{Store: x.store, Now: x.start.now}
	recorded, ok, err := reg.LoadSettings(x.ctx, "child-new")
	if err != nil || !ok {
		t.Fatalf("the child has no record: %v %v", ok, err)
	}
	if err := recorded.RequireUsable(); err != nil {
		t.Fatalf("the child's record is not usable: %v", err)
	}
	resume := func(reported string) []string {
		answer := map[string]any{}
		for key, value := range obj(x.host.creationReceipt["creation"]) {
			if key != "activePermissionProfile" {
				answer[key] = value
			}
		}
		response := deliveryValue(answer).(contract.OrderedObject)
		if reported != "" {
			profile, err := hook.Decode([]byte(reported))
			if err != nil {
				t.Fatal(err)
			}
			response = append(response, contract.Field{Key: "activePermissionProfile", Value: profile})
		}
		return findingCodes(recorded.Mismatches(response, true, true, false))
	}
	for _, same := range []string{profileTrusted, profileFlipped} {
		if codes := resume(same); len(codes) != 0 {
			t.Fatalf("the recorded profile reported as %s mismatches: %v", same, codes)
		}
	}
	for _, other := range []string{
		`{"id": "other", "extends": null}`,
		`{"id": "trusted"}`,
		`{"id": "trusted", "extends": ":workspace"}`,
		`{"id": "trusted", "extends": null, "network": true}`,
		profileBuiltin,
		`"trusted"`,
	} {
		if codes := resume(other); !reflect.DeepEqual(codes, []string{"unverifiable_permission_profile"}) {
			t.Fatalf("the recorded profile against %s: %v", other, codes)
		}
	}
	if codes := resume(""); !reflect.DeepEqual(codes, []string{"setting_unobservable"}) {
		t.Fatalf("a resume that reports no profile: %v", codes)
	}
}

// The object form is bounded: an id of text, an extends of null or text, and scalar text, boolean or
// null for anything else, at most sixteen keys. A number is refused because a request decodes it to a
// float64 and a host's report keeps its own spelling, so one profile would be two values to the
// comparison. The string form and an absent or null field are accepted as they were.
func TestPermissionProfileShapeIsChecked(t *testing.T) {
	base := requestFixture(t)
	many := []string{`"id": "p"`}
	for i := range 16 {
		many = append(many, `"k`+string(rune('a'+i))+`": true`)
	}
	accepted := []string{
		profileTrusted, profileFlipped, `{"id": "p"}`, `{"id": "p", "extends": ":workspace"}`,
		`{"id": "p", "a": "text", "b": false, "c": null}`,
		`{"id": "` + strings.Repeat("x", 500) + `"}`,
		`"trusted"`, `null`,
		"{" + strings.Join(many[:16], ", ") + "}",
	}
	for _, requested := range accepted {
		t.Run("accepts "+requested[:min(len(requested), 40)], func(t *testing.T) {
			parsed, err := ParseRequest(profileRequest(t, base, requested))
			if err != nil {
				t.Fatalf("%s refused: %v", requested, err)
			}
			for _, role := range []string{"parent", "child"} {
				got := obj(obj(parsed[role])["settings"])["expectedPermissionProfile"]
				if !reflect.DeepEqual(got, jsonValue(requested)) {
					t.Fatalf("%s's profile parsed to %v, not %s", role, got, requested)
				}
			}
		})
	}
	refused := []struct{ requested, names string }{
		{`{}`, "id"},
		{`{"extends": null}`, "id"},
		{`{"id": ""}`, "id"},
		{`{"id": "   "}`, "id"},
		{`{"id": 3}`, "id"},
		{`{"id": null}`, "id"},
		{`{"id": "` + strings.Repeat("x", 501) + `"}`, "id"},
		{`{"id": "a\u0000b"}`, "id"},
		{`{"id": "p", "extends": 3}`, "extends"},
		{`{"id": "p", "extends": ""}`, "extends"},
		{`{"id": "p", "extends": {"id": "q"}}`, "extends"},
		{`{"id": "p", "n": 1}`, "n"},
		{`{"id": "p", "n": 1.0}`, "n"},
		{`{"id": "p", "n": 9007199254740993}`, "n"},
		{`{"id": "p", "nested": {"a": true}}`, "nested"},
		{`{"id": "p", "list": []}`, "list"},
		{`{"id": "p", "blank": ""}`, "blank"},
		{`{"id": "p", "": true}`, "key"},
		{"{" + strings.Join(many, ", ") + "}", "at most 16"},
		{`["trusted"]`, "expectedPermissionProfile"},
		{`5`, "expectedPermissionProfile"},
		{`true`, "expectedPermissionProfile"},
		{`""`, "expectedPermissionProfile"},
		{`"` + strings.Repeat("x", 501) + `"`, "expectedPermissionProfile"},
	}
	for _, c := range refused {
		t.Run("refuses "+c.requested[:min(len(c.requested), 40)], func(t *testing.T) {
			_, err := ParseRequest(profileRequest(t, base, c.requested))
			if err == nil || !strings.Contains(err.Error(), "parent.settings.expectedPermissionProfile") || !strings.Contains(err.Error(), c.names) {
				t.Fatalf("%s: %v", c.requested, err)
			}
		})
	}
	// The child's settings are held to the same shape as the parent's.
	var request map[string]any
	if err := json.Unmarshal(profileRequest(t, base, profileTrusted), &request); err != nil {
		t.Fatal(err)
	}
	obj(obj(request["child"])["settings"])["expectedPermissionProfile"] = map[string]any{"extends": nil}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRequest(raw); err == nil || !strings.Contains(err.Error(), "child.settings.expectedPermissionProfile") {
		t.Fatalf("a child profile without an id: %v", err)
	}
}

// The profile is part of the request: a request that differs only in the order of its keys is the same
// request and replays, and one naming another profile under the same request id is refused.
func TestPermissionProfileIsPartOfTheRequestFingerprint(t *testing.T) {
	base := requestFixture(t)
	x := newProfileRun(t, base, profileTrusted)
	if got := x.run(profileRequest(t, base, profileTrusted)); got["state"] != "admitted" {
		t.Fatalf("first start: %v", got)
	}
	row, err := x.store.ManagedStartRequest(x.ctx, "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := x.run(profileRequest(t, base, profileFlipped)); got["state"] != "admitted" || x.host.created != 1 || x.host.sent != 1 {
		t.Fatalf("a replay with the profile's keys in another order: %v effects %d/%d", got, x.host.created, x.host.sent)
	}
	if again, err := x.store.ManagedStartRequest(x.ctx, "managed-1"); err != nil || again.RequestFingerprint != row.RequestFingerprint {
		t.Fatalf("the fingerprint moved with the key order: %v %v", again.RequestFingerprint, err)
	}
	for _, other := range []string{`{"id": "other", "extends": null}`, `{"id": "trusted"}`, `"trusted"`, ""} {
		_, err := x.start.Run(x.ctx, profileRequest(t, base, other))
		reasonIs(t, err, "relationship_conflict")
	}
	if x.host.created != 1 || x.host.sent != 1 {
		t.Fatalf("a refused replay reached the host: %d/%d", x.host.created, x.host.sent)
	}
}

// The creation response is verified against what the request named and never copied into the record.
// Copying would record what the host chose and not what was authorized, which is the widening
// relay.md forbids; it would also leave a record the later guards refuse, because they hold the stored
// settings equal to the request's. A request naming no profile therefore still ends with a record
// naming none, and a creation that reports a custom profile for it is refused with no record at all.
func TestPermissionProfileCreationResponseIsVerifiedNotCopied(t *testing.T) {
	base := requestFixture(t)
	cases := []struct {
		name, requested, reported string
		admitted                  bool
	}{
		{"the same custom object", profileTrusted, profileFlipped, true},
		{"another custom object", profileTrusted, `{"id": "other", "extends": null}`, false},
		{"the same id without extends", profileTrusted, `{"id": "trusted"}`, false},
		{"a built-in where a custom one is named", profileTrusted, profileBuiltin, false},
		{"no profile where one is named", profileTrusted, "", false},
		{"a built-in where none is named", "", profileBuiltin, true},
		{"a null where none is named", "null", profileBuiltin, true},
		{"nothing where none is named", "", "", true},
		{"a custom profile where none is named", "", profileTrusted, false},
		{"another sandbox's built-in where none is named", "", `{"id": ":danger-full-access", "extends": null}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newProfileRun(t, base, c.reported)
			got := x.run(profileRequest(t, base, c.requested))
			if x.host.created != 1 {
				t.Fatalf("the creation was asked %d times: %v", x.host.created, got)
			}
			if !c.admitted {
				if got["state"] != "refused" || got["stage"] != "creation" || got["reason"] != "creation_settings_unverified" || x.host.sent != 0 || x.count("relationships") != 0 || x.count("authorized_settings") != 0 {
					t.Fatalf("an unverified creation: %v sent %d, %d relationships, %d records", got, x.host.sent, x.count("relationships"), x.count("authorized_settings"))
				}
				return
			}
			if got["state"] != "admitted" || x.host.sent != 1 {
				t.Fatalf("a verified creation: %v sent %d", got, x.host.sent)
			}
			profile, named := x.storedProfile("child-new")
			want := jsonValue(orNull(c.requested))
			if c.requested == "" {
				if named {
					t.Fatalf("the creation's profile was copied into the record: %v", profile)
				}
			} else if !named || !reflect.DeepEqual(profile, want) {
				t.Fatalf("the child's record holds %v (named: %v), the request named %s", profile, named, c.requested)
			}
		})
	}
}

func orNull(text string) string {
	if text == "" {
		return "null"
	}
	return text
}

// A creation that stopped after thread/start is recovered through its retained shell only when what
// the host reported still matches what the request named; a creation reporting another profile
// is left incomplete with no recovery message sent, and a replay does not create again.
func TestPermissionProfilePartialCreationIsRecoveredOnlyWhenItMatches(t *testing.T) {
	base := requestFixture(t)
	t.Run("matching", func(t *testing.T) {
		x := newProfileRun(t, base, profileTrusted)
		x.host.partial = true
		for range 2 {
			got := x.run(profileRequest(t, base, profileFlipped))
			if got["state"] != "admitted" || got["standbyTurnId"] != "recovered-standby" {
				t.Fatalf("recovery: %v", got)
			}
		}
		if x.host.created != 1 || x.host.sent != 2 {
			t.Fatalf("recovery repeated a host effect: %d/%d", x.host.created, x.host.sent)
		}
		if profile, ok := x.storedProfile("child-new"); !ok || !reflect.DeepEqual(profile, jsonValue(profileTrusted)) {
			t.Fatalf("the recovered child's record holds %v", profile)
		}
	})
	t.Run("mismatching", func(t *testing.T) {
		x := newProfileRun(t, base, `{"id": "other", "extends": null}`)
		x.host.partial = true
		for range 2 {
			got := x.run(profileRequest(t, base, profileTrusted))
			if got["state"] != "incomplete" || got["reason"] != "creation_failed" {
				t.Fatalf("a mismatching partial creation: %v", got)
			}
		}
		if x.host.created != 1 || x.host.sent != 0 || x.count("authorized_settings") != 0 {
			t.Fatalf("a mismatching partial creation reached further: %d/%d, %d records", x.host.created, x.host.sent, x.count("authorized_settings"))
		}
	})
}
