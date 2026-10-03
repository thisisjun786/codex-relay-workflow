package registry

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-454: the relay's role gate reads the same policy as the bridge, so a role that may run on
// several pairs must be accepted on each of them and refused on any other, with every pair named.

const (
	multiOpus   = "anthropic/claude-opus-5-5"
	multiSonnet = "anthropic/claude-sonnet-5-5"
	multiSol    = "gpt-6.1-sol"
)

var multiPairPolicyText = strings.ReplaceAll("{'allowed':[{'model':'"+multiOpus+"','efforts':['xhigh']},{'model':'"+multiSonnet+"','efforts':['xhigh']},{'model':'"+multiSol+"','efforts':['xhigh']}],"+
	"'roles':{'supervisor':{'expectation':'record'},'parent':{'model':'"+multiOpus+"','reasoningEffort':'xhigh'},"+
	"'child':{'expectation':'pair','pairs':[{'model':'"+multiSonnet+"','reasoningEffort':'xhigh'},{'model':'"+multiSol+"','reasoningEffort':'xhigh'}]}}}", "'", "\"")

func multiPairRun(t *testing.T) *roleRun {
	t.Helper()
	x := newRoleRun(t)
	x.policy(multiPairPolicyText)
	if !x.r.Policy.Declared {
		t.Fatalf("the two-pair policy was not read: %s", x.r.Policy.Detail)
	}
	return x
}

func pairSettings(model, effort string) contract.OrderedObject {
	return contract.OrderedObject{{Key: "model", Value: model}, {Key: "reasoningEffort", Value: effort}}
}

func bothChildPairs() []any {
	return []any{pairObject(multiSonnet, "xhigh"), pairObject(multiSol, "xhigh")}
}

func TestAMultiPairRoleIsReportedWithItsList(t *testing.T) {
	roles := multiPairRun(t).r.Policy.Summary().Get("roles").(contract.OrderedObject)
	wantChild := contract.OrderedObject{{Key: "role", Value: "child"}, {Key: "expectation", Value: "pair"}, {Key: "model", Value: nil}, {Key: "reasoningEffort", Value: nil}, {Key: "pairs", Value: bothChildPairs()}}
	wantParent := contract.OrderedObject{{Key: "role", Value: "parent"}, {Key: "expectation", Value: "pair"}, {Key: "model", Value: multiOpus}, {Key: "reasoningEffort", Value: "xhigh"}}
	if !reflect.DeepEqual(roles.Get("child"), wantChild) || !reflect.DeepEqual(roles.Get("parent"), wantParent) {
		t.Fatalf("child %#v parent %#v", roles.Get("child"), roles.Get("parent"))
	}
}

func TestARecordOnAnyPairOfTheRoleIsCurrentAndAnotherIsStale(t *testing.T) {
	x := multiPairRun(t)
	for _, pair := range [][2]string{{multiSonnet, "xhigh"}, {multiSol, "xhigh"}} {
		if finding := CheckRecord(pairSettings(pair[0], pair[1]), "child", x.r.Policy); finding != nil {
			t.Fatalf("%v: %v", pair, finding)
		}
		if !derivedFromRolePair(pairSettings(pair[0], pair[1]), "child", x.r.Policy) {
			t.Fatalf("%v: a record on a pair of the role is derived from the role", pair)
		}
	}
	for _, pair := range [][2]string{{multiSonnet, "high"}, {multiSol, "high"}, {multiOpus, "xhigh"}} {
		finding := CheckRecord(pairSettings(pair[0], pair[1]), "child", x.r.Policy)
		if finding == nil || finding.Get("code") != string(contract.RefusalSettingsRecordStaleForRole) {
			t.Fatalf("%v: %v", pair, finding)
		}
		if !reflect.DeepEqual(finding.Get("expected"), bothChildPairs()) || !reflect.DeepEqual(finding.Get("recorded"), pairObject(pair[0], pair[1])) {
			t.Fatalf("%v: %v", pair, finding)
		}
		if derivedFromRolePair(pairSettings(pair[0], pair[1]), "child", x.r.Policy) {
			t.Fatalf("%v: derived", pair)
		}
	}
	cited := pairSettings(multiSol, "xhigh").Set("citedException", "x")
	if derivedFromRolePair(cited, "child", x.r.Policy) {
		t.Fatal("a record citing an exception is not derived from the role")
	}
	// The parent keeps one pair: its finding still names that one pair as an object.
	finding := CheckRecord(pairSettings(multiSol, "xhigh"), "parent", x.r.Policy)
	if finding == nil || !reflect.DeepEqual(finding.Get("expected"), pairObject(multiOpus, "xhigh")) {
		t.Fatalf("%v", finding)
	}
}

func TestABindingToTheRoleAcceptsAnyOfItsPairs(t *testing.T) {
	x := multiPairRun(t)
	for _, pair := range [][2]string{{multiSonnet, "xhigh"}, {multiSol, "xhigh"}} {
		if finding := CheckBinding("child", "child", pairSettings(pair[0], pair[1]), x.r.Policy); finding != nil {
			t.Fatalf("%v: %v", pair, finding)
		}
	}
	finding := CheckBinding("child", "child", pairSettings(multiSonnet, "high"), x.r.Policy)
	if finding == nil || finding.Get("code") != string(contract.RefusalRoleBindingMismatch) || !reflect.DeepEqual(finding.Get("expected"), bothChildPairs()) {
		t.Fatalf("%v", finding)
	}
}

// Recording against a bound child: each pair of the role is recorded, another is refused, and a
// citation an earlier record carried is dropped once the recorded pair is any pair of the role.
func TestRecordingSettingsForABoundChildUsesEveryPairOfItsRole(t *testing.T) {
	x := multiPairRun(t)
	settings := func(model, effort string) contract.OrderedObject {
		return settingsFixture(x.root).Set("model", model).Set("reasoningEffort", effort)
	}
	bind := func(task, key string) {
		t.Helper()
		if _, err := x.r.BindScope(ctx(), "child", key, Endpoint{task, "host-a", ns(x.root), ns("cxc-child")}); err != nil {
			t.Fatal(err)
		}
	}
	for i, pair := range [][2]string{{multiSonnet, "xhigh"}, {multiSol, "xhigh"}} {
		task, key := []string{"child-sonnet", "child-sol"}[i], []string{"ISS-1", "ISS-2"}[i]
		bind(task, key)
		if _, err := x.r.RecordSettings(ctx(), task, settings(pair[0], pair[1]), "managed_start", "child", Citation{}); err != nil {
			t.Fatalf("%v: %v", pair, err)
		}
		if _, free, err := x.r.AuthorizedSettings(ctx(), task); err != nil || free {
			t.Fatalf("%v: settings-free=%v err=%v", pair, free, err)
		}
	}
	bind("child-other", "ISS-3")
	_, err := x.r.RecordSettings(ctx(), "child-other", settings(multiSonnet, "high"), "managed_start", "child", Citation{})
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != string(contract.RefusalRoleBindingMismatch) {
		t.Fatalf("want role_binding_mismatch, got %v", err)
	}

	// An earlier record cites an exception the policy does not carry; recording the role's own
	// second pair from managed_start does not carry that citation forward.
	bind("child-cited", "ISS-4")
	cited := settings(multiOpus, "xhigh").Set("citedException", "x")
	if _, err := x.r.Store.DB.ExecContext(ctx(), "INSERT OR REPLACE INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", "child-cited", pyjson.Dumps(cited, pyjson.Options{}), "test-raw", fakeISO); err != nil {
		t.Fatal(err)
	}
	if _, err := x.r.RecordSettings(ctx(), "child-cited", settings(multiSol, "xhigh"), "managed_start", "child", Citation{}); err != nil {
		t.Fatalf("the citation was carried onto a pair of the role: %v", err)
	}
	if stored, _ := x.stored("child-cited").(contract.OrderedObject); stored.Get("citedException") != nil {
		t.Fatalf("%v", stored)
	}
}
