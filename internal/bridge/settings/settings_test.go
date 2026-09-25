package settings

import (
	"reflect"
	"strings"
	"testing"
)

func TestReceipts_whenLimitsAreReported(t *testing.T) {
	// Given: an observed thread with the default approval policy.
	contract := Contract{}
	response := map[string]any{"approvalPolicy": "never"}
	// When: both observation receipts are produced.
	approvals := contract.Approvals(response)
	settings := contract.Receipt(response, "resume")
	// Then: each states its Python-compatible observation boundary.
	if approvals["limits"] != approvalLimits || settings["observationLimits"] != observationLimits {
		t.Fatalf("approvals=%v settings=%v", approvals, settings)
	}
}

func TestResume_whenApprovalDeclared(t *testing.T) {
	// Given: a caller declares a policy but asks for an effort and sandbox.
	c := Contract{ApprovalPolicy: "on-request", ReasoningEffort: "high", Sandbox: "workspace-write"}
	// When: resume parameters are built.
	p := c.ResumeParams("thread-1")
	// Then: approval is not transmitted; effort travels in config.
	if _, ok := p["approvalPolicy"]; ok {
		t.Fatal(p)
	}
	if p["config"].(map[string]any)["model_reasoning_effort"] != "high" {
		t.Fatal(p)
	}
}
func TestFindings_whenApprovalDisagrees(t *testing.T) {
	// Given: a declared approval and a host echo differing in several fields.
	c := Contract{Model: "m", ApprovalPolicy: "never"}
	// When: the host reports on-request.
	findings := c.Findings(map[string]any{"approvalPolicy": "on-request", "model": "wrong"})
	// Then: approval alone is refused first.
	if len(findings) != 1 || findings[0].Code != UnsupportedApproval {
		t.Fatal(findings)
	}
}
func TestFindings_whenSandboxUnobservable(t *testing.T) {
	// Given: a fully specified sandbox expectation.
	c := Contract{ExpectedPolicy: map[string]any{"type": "workspaceWrite", "networkAccess": true}}
	// When: the host gives a malformed sandbox.
	found := c.Findings(map[string]any{"approvalPolicy": "never", "sandbox": "workspaceWrite"})
	// Then: an unreadable answer is a mismatch, not a missing observation.
	if len(found) != 1 || found[0].Code != NotPreserved {
		t.Fatal(found)
	}
}
func TestNormalise_whenPolicyDefaultsAreFilled(t *testing.T) {
	// Given: each host sandbox mode without explicit defaults.
	cases := []struct {
		kind string
		want map[string]any
	}{
		{"readOnly", map[string]any{"type": "readOnly", "networkAccess": false}},
		{"externalSandbox", map[string]any{"type": "externalSandbox", "networkAccess": "restricted"}},
		{"dangerFullAccess", map[string]any{"type": "dangerFullAccess"}},
		{"workspaceWrite", map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			// When: the policy is normalised.
			got := Normalise(map[string]any{"type": tc.kind})
			// Then: the exact Python default policy is returned.
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestNormalise_whenHostPolicyIsMalformed(t *testing.T) {
	// Given: malformed host policies including nested roots.
	cases := []any{nil, "workspaceWrite", 42, []any{"workspaceWrite"}, map[string]any{"no_type": 1}, map[string]any{"type": map[string]any{"unhashable": 1}}, map[string]any{"type": []any{"bad"}}, map[string]any{"type": "workspaceWrite", "writableRoots": nil}, map[string]any{"type": "workspaceWrite", "writableRoots": 7}}
	for _, input := range cases {
		// When: an untrusted host policy is decoded.
		got := Normalise(input)
		// Then: it is unreadable rather than causing a panic or being silently accepted.
		if got != nil {
			t.Fatalf("%v: %v", input, got)
		}
	}
}

func TestFindings_whenRequestedSettingIsMissingOrNull(t *testing.T) {
	// Given: a model and effort expectation, and either an absent or null host answer.
	for _, response := range []map[string]any{{"approvalPolicy": "never", "model": "m"}, {"approvalPolicy": "never", "model": "m", "reasoningEffort": nil}} {
		c := Contract{Model: "m", ReasoningEffort: "xhigh"}
		// When: the host answer is compared.
		found := c.Findings(response)
		// Then: silence is unobservable, not a mismatch.
		if len(found) != 1 || found[0].Code != Unobservable || found[0].Field != "reasoningEffort" {
			t.Fatal(found)
		}
	}
}

func TestFindings_whenHostFillsPolicyDefaults(t *testing.T) {
	// Given: a requested workspace-write mode and the host's expanded policy.
	c := Contract{Sandbox: "workspace-write"}
	response := map[string]any{"approvalPolicy": "never", "sandbox": Normalise(map[string]any{"type": "workspaceWrite"})}
	// When: the expanded answer is compared.
	found := c.Findings(response)
	// Then: protocol defaults are not reported as disagreements.
	if len(found) != 0 {
		t.Fatal(found)
	}
}

func TestPolicy_whenUntransmittableField(t *testing.T) {
	// Given: a read-only policy requesting network access.
	c := Contract{ExpectedPolicy: map[string]any{"type": "readOnly", "networkAccess": true}}
	// When: it is validated before a request.
	err := c.Validate()
	// Then: the local limitation is refused.
	if err == nil || !strings.Contains(err.Error(), Untransmittable) {
		t.Fatal(err)
	}
}
